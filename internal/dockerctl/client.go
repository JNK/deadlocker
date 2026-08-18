// Package dockerctl is a small Docker Engine API client speaking HTTP over the
// unix socket. We only need image pull, container create/start/stop/remove,
// inspect and log streaming, so pulling in the full Docker SDK (and its
// transitive dependency tree) is not worth it.
package dockerctl

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// APIVersion is pinned low enough to work with any reasonably recent daemon
// (the daemon reports MinAPIVersion 1.40) while still supporting everything we
// use.
const APIVersion = "v1.43"

type Client struct {
	http *http.Client
	host string // used as the HTTP Host header; the dial target is the socket
}

// New returns a client bound to the local Docker daemon. It honours DOCKER_HOST
// when it points at a unix socket, otherwise it probes the usual locations
// (including the Docker Desktop / OrbStack per-user socket).
func New() (*Client, error) {
	sock, err := resolveSocket()
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &Client{
		host: "docker",
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return dialer.DialContext(ctx, "unix", sock)
				},
				// Log/attach streams are long-lived; never time them out here.
				ResponseHeaderTimeout: 60 * time.Second,
			},
		},
	}, nil
}

func resolveSocket() (string, error) {
	var candidates []string
	if h := os.Getenv("DOCKER_HOST"); strings.HasPrefix(h, "unix://") {
		candidates = append(candidates, strings.TrimPrefix(h, "unix://"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(home, ".docker/run/docker.sock"),
			filepath.Join(home, ".orbstack/run/docker.sock"),
			filepath.Join(home, ".colima/default/docker.sock"),
		)
	}
	candidates = append(candidates, "/var/run/docker.sock")

	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("no docker socket found (tried %s); is Docker running?", strings.Join(candidates, ", "))
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(buf)
	}
	u := "http://" + c.host + "/" + APIVersion + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		var apiErr struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(msg, &apiErr) == nil && apiErr.Message != "" {
			return nil, fmt.Errorf("docker %s %s: %s", method, path, apiErr.Message)
		}
		return nil, fmt.Errorf("docker %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(msg)))
	}
	return resp, nil
}

func (c *Client) doJSON(ctx context.Context, method, path string, query url.Values, body, out any) error {
	resp, err := c.do(ctx, method, path, query, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Ping verifies the daemon is reachable.
func (c *Client) Ping(ctx context.Context) error {
	var v struct {
		Version string `json:"Version"`
	}
	return c.doJSON(ctx, http.MethodGet, "/version", nil, nil, &v)
}

// HasImage reports whether the image is present locally.
func (c *Client) HasImage(ctx context.Context, image string) bool {
	resp, err := c.do(ctx, http.MethodGet, "/images/"+url.PathEscape(image)+"/json", nil, nil)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// ImageArch reports an image's architecture, or "" if it cannot be read. It is
// how the caller finds out that an image is going to run under emulation, which
// changes how long everything takes.
func (c *Client) ImageArch(ctx context.Context, image string) string {
	resp, err := c.do(ctx, http.MethodGet, "/images/"+url.PathEscape(image)+"/json", nil, nil)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var out struct {
		Architecture string `json:"Architecture"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return ""
	}
	return out.Architecture
}

// HostArch is the architecture Docker itself reports for this machine.
func (c *Client) HostArch(ctx context.Context) string {
	resp, err := c.do(ctx, http.MethodGet, "/version", nil, nil)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var out struct {
		Arch string `json:"Arch"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return ""
	}
	return out.Arch
}

// PullImage pulls an image, reporting progress lines to onStatus. It is a no-op
// when the image already exists locally.
func (c *Client) PullImage(ctx context.Context, image string, onStatus func(string)) error {
	if c.HasImage(ctx, image) {
		return nil
	}
	err := c.pull(ctx, image, "", onStatus)
	if err == nil || !isNoManifestErr(err) {
		return err
	}

	// No image for this machine's architecture. Old MySQL tags are the common
	// case: there is no arm64 build of mysql:5.7 at all, so on Apple Silicon
	// every 5.7 scenario would simply fail to start. Docker can run the amd64
	// build under emulation, which is slow but correct -- and a slow 5.7 run is
	// worth much more than no 5.7 run.
	if onStatus != nil {
		onStatus("no image for this architecture; retrying as linux/amd64 (emulated)")
	}
	return c.pull(ctx, image, "linux/amd64", onStatus)
}

// isNoManifestErr reports whether a pull failed because the registry has no
// build for the requested platform.
func isNoManifestErr(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "no matching manifest") ||
		strings.Contains(s, "no match for platform")
}

func (c *Client) pull(ctx context.Context, image, platform string, onStatus func(string)) error {
	name, tag := image, "latest"
	// Split on the last colon, but only if it is not part of a registry:port.
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		name, tag = image[:i], image[i+1:]
	}
	q := url.Values{"fromImage": {name}, "tag": {tag}}
	if platform != "" {
		q.Set("platform", platform)
	}
	resp, err := c.do(ctx, http.MethodPost, "/images/create", q, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	dec := json.NewDecoder(resp.Body)
	for {
		var msg struct {
			Status   string `json:"status"`
			Progress string `json:"progress"`
			Error    string `json:"error"`
		}
		if err := dec.Decode(&msg); err == io.EOF {
			return nil
		} else if err != nil {
			return err
		}
		if msg.Error != "" {
			return fmt.Errorf("pull %s: %s", image, msg.Error)
		}
		if onStatus != nil && msg.Status != "" {
			line := msg.Status
			if msg.Progress != "" {
				line += " " + msg.Progress
			}
			onStatus(line)
		}
	}
}

type CreateConfig struct {
	Image        string
	Cmd          []string
	Env          []string
	Labels       map[string]string
	ExposedPort  string // container-side port, e.g. "3306/tcp"
	HostPort     string // "" lets Docker choose an ephemeral port
	Tmpfs        map[string]string
	AutoRemove   bool
	Name         string
	Binds        []string
	ExtraHostCfg map[string]any
}

// CreateContainer creates a container and returns its ID.
func (c *Client) CreateContainer(ctx context.Context, cfg CreateConfig) (string, error) {
	hostCfg := map[string]any{
		"PortBindings": map[string]any{
			cfg.ExposedPort: []map[string]string{{"HostIp": "127.0.0.1", "HostPort": cfg.HostPort}},
		},
		"AutoRemove": cfg.AutoRemove,
	}
	if len(cfg.Tmpfs) > 0 {
		hostCfg["Tmpfs"] = cfg.Tmpfs
	}
	if len(cfg.Binds) > 0 {
		hostCfg["Binds"] = cfg.Binds
	}
	for k, v := range cfg.ExtraHostCfg {
		hostCfg[k] = v
	}

	body := map[string]any{
		"Image":        cfg.Image,
		"Env":          cfg.Env,
		"Labels":       cfg.Labels,
		"ExposedPorts": map[string]any{cfg.ExposedPort: struct{}{}},
		"HostConfig":   hostCfg,
	}
	if len(cfg.Cmd) > 0 {
		body["Cmd"] = cfg.Cmd
	}

	q := url.Values{}
	if cfg.Name != "" {
		q.Set("name", cfg.Name)
	}
	var out struct {
		ID       string   `json:"Id"`
		Warnings []string `json:"Warnings"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/containers/create", q, body, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

func (c *Client) StartContainer(ctx context.Context, id string) error {
	return c.doJSON(ctx, http.MethodPost, "/containers/"+id+"/start", nil, nil, nil)
}

// StopContainer stops the container, waiting up to timeout seconds for a clean
// shutdown before the daemon kills it.
func (c *Client) StopContainer(ctx context.Context, id string, timeout int) error {
	q := url.Values{"t": {fmt.Sprint(timeout)}}
	err := c.doJSON(ctx, http.MethodPost, "/containers/"+id+"/stop", q, nil, nil)
	if err != nil && (strings.Contains(err.Error(), "No such container") || strings.Contains(err.Error(), "is not running")) {
		return nil
	}
	return err
}

func (c *Client) RemoveContainer(ctx context.Context, id string) error {
	q := url.Values{"v": {"1"}, "force": {"1"}}
	err := c.doJSON(ctx, http.MethodDelete, "/containers/"+id, q, nil, nil)
	if err != nil && strings.Contains(err.Error(), "No such container") {
		return nil
	}
	return err
}

type ContainerState struct {
	Running bool   `json:"Running"`
	Status  string `json:"Status"`
	Health  *struct {
		Status string `json:"Status"`
	} `json:"Health"`
}

type ContainerInspect struct {
	ID              string         `json:"Id"`
	Name            string         `json:"Name"`
	State           ContainerState `json:"State"`
	NetworkSettings struct {
		Ports map[string][]struct {
			HostIP   string `json:"HostIp"`
			HostPort string `json:"HostPort"`
		} `json:"Ports"`
	} `json:"NetworkSettings"`
}

func (c *Client) Inspect(ctx context.Context, id string) (*ContainerInspect, error) {
	var out ContainerInspect
	if err := c.doJSON(ctx, http.MethodGet, "/containers/"+id+"/json", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// HostPort returns the host-side port bound to the given container port.
func (ci *ContainerInspect) HostPort(containerPort string) (string, bool) {
	binds, ok := ci.NetworkSettings.Ports[containerPort]
	if !ok || len(binds) == 0 {
		return "", false
	}
	for _, b := range binds {
		if b.HostPort != "" {
			return b.HostPort, true
		}
	}
	return "", false
}

// ListByLabel returns containers (including stopped ones) carrying label=value.
func (c *Client) ListByLabel(ctx context.Context, label, value string) ([]string, error) {
	filters, _ := json.Marshal(map[string][]string{"label": {label + "=" + value}})
	q := url.Values{"all": {"1"}, "filters": {string(filters)}}
	var out []struct {
		ID string `json:"Id"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/containers/json", q, nil, &out); err != nil {
		return nil, err
	}
	ids := make([]string, len(out))
	for i, o := range out {
		ids[i] = o.ID
	}
	return ids, nil
}

// LogLine is one demultiplexed line from a container's stdout/stderr.
type LogLine struct {
	Stream string    `json:"stream"` // "stdout" | "stderr"
	Time   time.Time `json:"time"`
	Text   string    `json:"text"`
}

// StreamLogs follows the container log and calls onLine for each line until the
// context is cancelled or the stream ends.
//
// Docker multiplexes stdout/stderr into frames of an 8-byte header
// (stream-type, 3 reserved bytes, big-endian uint32 payload length) followed by
// the payload. That framing is only present when the container was created
// without a TTY, which is how we create ours.
func (c *Client) StreamLogs(ctx context.Context, id string, since time.Time, onLine func(LogLine)) error {
	q := url.Values{
		"stdout":     {"1"},
		"stderr":     {"1"},
		"follow":     {"1"},
		"timestamps": {"1"},
	}
	if !since.IsZero() {
		q.Set("since", fmt.Sprint(since.Unix()))
	}
	resp, err := c.do(ctx, http.MethodGet, "/containers/"+id+"/logs", q, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	br := bufio.NewReaderSize(resp.Body, 64<<10)
	header := make([]byte, 8)
	for {
		if _, err := io.ReadFull(br, header); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF || ctx.Err() != nil {
				return nil
			}
			return err
		}
		n := binary.BigEndian.Uint32(header[4:8])
		if n == 0 {
			continue
		}
		if n > 4<<20 { // guard against a desynchronised stream
			return fmt.Errorf("log frame too large: %d bytes", n)
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(br, payload); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF || ctx.Err() != nil {
				return nil
			}
			return err
		}
		stream := "stdout"
		if header[0] == 2 {
			stream = "stderr"
		}
		for _, raw := range strings.Split(strings.TrimRight(string(payload), "\n"), "\n") {
			if raw == "" {
				continue
			}
			ts, text := splitLogTimestamp(raw)
			onLine(LogLine{Stream: stream, Time: ts, Text: text})
		}
		if ctx.Err() != nil {
			return nil
		}
	}
}

// splitLogTimestamp peels the RFC3339Nano prefix Docker prepends when
// timestamps=1. If it is missing or unparseable the whole line is the text.
func splitLogTimestamp(line string) (time.Time, string) {
	i := strings.IndexByte(line, ' ')
	if i < 0 {
		return time.Time{}, line
	}
	ts, err := time.Parse(time.RFC3339Nano, line[:i])
	if err != nil {
		return time.Time{}, line
	}
	return ts, line[i+1:]
}
