// Package mysqlbox owns the lifecycle of the MySQL containers the scenarios run
// against.
//
// Containers are expensive to create (MySQL initialises its data directory on
// first boot), so we keep one long-lived container per distinct image and give
// every run its own throwaway database instead. Anything a scenario needs to
// vary per run -- isolation level, lock wait timeout, deadlock detection -- is
// settable per session or as a dynamic global, so a shared server is not a
// limitation in practice.
package mysqlbox

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/jnk/deadlocker/internal/dockerctl"
)

const (
	// LabelKey marks every container we create so stale ones can be reaped.
	LabelKey   = "io.jnk.deadlocker"
	LabelValue = "1"

	RootUser     = "root"
	RootPassword = "deadlocker"

	containerPort = "3306/tcp"
)

// serverArgs are passed to mysqld. They tune the server for fast startup and
// for making lock behaviour observable rather than for realistic performance.
var serverArgs = []string{
	// Deadlock reports land in the error log, which we surface as docker logs.
	"--innodb-print-all-deadlocks=ON",
	"--log-error-verbosity=3",
	// Scenarios are stepped through by hand, so the server default is generous.
	// Individual scenarios override it per session.
	"--innodb-lock-wait-timeout=120",
	// Small footprint so the data directory fits comfortably in tmpfs and
	// initialisation stays quick.
	"--innodb-buffer-pool-size=64M",
	"--innodb-redo-log-capacity=16777216",
	// The error log deliberately stays at the image default, which is stderr.
	// Pointing --log-error at /dev/stderr does not work: mysqld appends ".err"
	// to the value and then fails to open /dev/stderr.err.
}

// Box is a running MySQL container plus the proxy-free admin connection used
// for setup, teardown and lock introspection.
type Box struct {
	Image       string
	ContainerID string
	HostPort    string
	StartedAt   time.Time

	admin *sql.DB
}

// Addr is the host-side address of the container's MySQL port.
func (b *Box) Addr() string { return "127.0.0.1:" + b.HostPort }

// Admin returns a pooled connection that bypasses the wire proxy. It is used
// for schema setup and for lock introspection, neither of which should show up
// in the captured packet timeline.
func (b *Box) Admin() *sql.DB { return b.admin }

// DSN builds a DSN pointing at addr, using the text protocol: arguments are
// interpolated client-side so every statement reaches the server as a readable
// COM_QUERY. Passing the container address yields a direct connection; passing
// the proxy address routes through packet capture.
//
// TLS is explicitly disabled: the proxy can only decode the command phase if
// the connection stays in plaintext. MySQL 8.4's default caching_sha2_password
// still authenticates fine over plaintext via the RSA public-key exchange.
func DSN(addr, database string) string {
	return dsn(addr, database, true)
}

// DSNPrepared builds a DSN that sends real prepared statements, so the wire
// trace shows COM_STMT_PREPARE / COM_STMT_EXECUTE and binary result rows.
func DSNPrepared(addr, database string) string {
	return dsn(addr, database, false)
}

func dsn(addr, database string, interpolate bool) string {
	cfg := mysql.NewConfig()
	cfg.User = RootUser
	cfg.Passwd = RootPassword
	cfg.Net = "tcp"
	cfg.Addr = addr
	cfg.DBName = database
	cfg.TLS = nil
	cfg.AllowNativePasswords = true
	// Required for caching_sha2_password full authentication without TLS.
	cfg.AllowFallbackToPlaintext = true
	cfg.ParseTime = true
	cfg.MultiStatements = false
	cfg.InterpolateParams = interpolate
	cfg.Timeout = 10 * time.Second
	return cfg.FormatDSN()
}

// quietDriverLogger drops the errors go-sql-driver prints to stderr when a
// connection dies in a way we caused on purpose. Tearing a run down kills
// connections that are parked on a lock wait, which always produces "unexpected
// EOF" and "invalid connection"; surfacing those as if they were faults trains
// people to ignore the log. Anything else is passed through.
type quietDriverLogger struct{ out *log.Logger }

func (q quietDriverLogger) Print(v ...any) {
	msg := fmt.Sprint(v...)
	for _, expected := range []string{
		"unexpected EOF",
		"invalid connection",
		"busy buffer",
		"closing bad idle connection",
	} {
		if strings.Contains(msg, expected) {
			return
		}
	}
	q.out.Print(msg)
}

// SilenceExpectedDriverNoise installs the filtered driver logger. It is a
// process-wide setting, so the command wires it up once at start.
func SilenceExpectedDriverNoise() error {
	return mysql.SetLogger(quietDriverLogger{out: log.New(os.Stderr, "[mysql] ", log.LstdFlags)})
}

// Pool lazily starts and reuses one Box per image.
type Pool struct {
	docker *dockerctl.Client

	mu    sync.Mutex
	boxes map[string]*Box
	// starting guards against two runs racing to create the same image's box.
	starting map[string]chan struct{}

	onLog   func(image string, line dockerctl.LogLine)
	logCtx  context.Context
	logStop context.CancelFunc
}

func NewPool(dc *dockerctl.Client, onLog func(image string, line dockerctl.LogLine)) *Pool {
	ctx, stop := context.WithCancel(context.Background())
	return &Pool{
		docker:   dc,
		boxes:    map[string]*Box{},
		starting: map[string]chan struct{}{},
		onLog:    onLog,
		logCtx:   ctx,
		logStop:  stop,
	}
}

// Get returns a ready Box for the image, starting the container if needed.
// onProgress receives human-readable status while pulling and booting.
func (p *Pool) Get(ctx context.Context, image string, onProgress func(string)) (*Box, error) {
	if image == "" {
		image = "mysql:8.4"
	}
	for {
		p.mu.Lock()
		if b, ok := p.boxes[image]; ok {
			p.mu.Unlock()
			return b, nil
		}
		if ch, ok := p.starting[image]; ok {
			// Someone else is booting this image; wait and re-check.
			p.mu.Unlock()
			select {
			case <-ch:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		ch := make(chan struct{})
		p.starting[image] = ch
		p.mu.Unlock()

		b, err := p.start(ctx, image, onProgress)

		p.mu.Lock()
		delete(p.starting, image)
		if err == nil {
			p.boxes[image] = b
		}
		p.mu.Unlock()
		close(ch)
		return b, err
	}
}

func (p *Pool) start(ctx context.Context, image string, onProgress func(string)) (*Box, error) {
	progress := func(format string, args ...any) {
		if onProgress != nil {
			onProgress(fmt.Sprintf(format, args...))
		}
	}

	progress("checking image %s", image)
	if err := p.docker.PullImage(ctx, image, func(s string) { progress("pull: %s", s) }); err != nil {
		return nil, fmt.Errorf("pull image: %w", err)
	}

	progress("creating container")
	id, err := p.docker.CreateContainer(ctx, dockerctl.CreateConfig{
		Image: image,
		Cmd:   serverArgs,
		Env: []string{
			"MYSQL_ROOT_PASSWORD=" + RootPassword,
			// Allow root from any host: the proxy connects from the host
			// network, not from localhost inside the container.
			"MYSQL_ROOT_HOST=%",
		},
		Labels:      map[string]string{LabelKey: LabelValue, "io.jnk.deadlocker.image": image},
		ExposedPort: containerPort,
		HostPort:    "", // let Docker pick a free ephemeral port
		// A tmpfs data directory makes first boot noticeably faster and leaves
		// nothing behind on disk when the container goes away.
		Tmpfs:      map[string]string{"/var/lib/mysql": "rw,size=1g"},
		AutoRemove: false,
	})
	if err != nil {
		return nil, fmt.Errorf("create container: %w", err)
	}

	if err := p.docker.StartContainer(ctx, id); err != nil {
		_ = p.docker.RemoveContainer(context.Background(), id)
		return nil, fmt.Errorf("start container: %w", err)
	}

	insp, err := p.docker.Inspect(ctx, id)
	if err != nil {
		_ = p.docker.RemoveContainer(context.Background(), id)
		return nil, fmt.Errorf("inspect container: %w", err)
	}
	port, ok := insp.HostPort(containerPort)
	if !ok {
		_ = p.docker.RemoveContainer(context.Background(), id)
		return nil, fmt.Errorf("container %s exposed no host port for %s", id[:12], containerPort)
	}

	box := &Box{Image: image, ContainerID: id, HostPort: port, StartedAt: time.Now()}

	// Start following the container log before waiting for readiness so boot
	// output (and any startup failure) is visible in the UI.
	if p.onLog != nil {
		go func() {
			_ = p.docker.StreamLogs(p.logCtx, id, time.Time{}, func(l dockerctl.LogLine) {
				p.onLog(image, l)
			})
		}()
	}

	progress("waiting for mysqld on %s", box.Addr())
	db, err := waitReady(ctx, box, 120*time.Second, progress)
	if err != nil {
		_ = p.docker.RemoveContainer(context.Background(), id)
		return nil, err
	}
	box.admin = db
	progress("mysql ready on %s", box.Addr())
	return box, nil
}

func waitReady(ctx context.Context, box *Box, timeout time.Duration, progress func(string, ...any)) (*sql.DB, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	attempt := 0
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		attempt++
		db, err := sql.Open("mysql", DSN(box.Addr(), ""))
		if err != nil {
			lastErr = err
		} else {
			pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			err = db.PingContext(pingCtx)
			cancel()
			if err == nil {
				db.SetMaxOpenConns(8)
				db.SetMaxIdleConns(4)
				db.SetConnMaxLifetime(0)
				return db, nil
			}
			lastErr = err
			_ = db.Close()
		}
		if attempt%10 == 0 {
			progress("still waiting for mysqld (%v)", compactErr(lastErr))
		}
		select {
		case <-time.After(500 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, fmt.Errorf("mysql did not become ready within %s: %w", timeout, lastErr)
}

func compactErr(err error) string {
	if err == nil {
		return "no error"
	}
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

// Boxes returns a snapshot of the running boxes.
func (p *Pool) Boxes() []*Box {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*Box, 0, len(p.boxes))
	for _, b := range p.boxes {
		out = append(out, b)
	}
	return out
}

// Close stops and removes every container the pool started.
func (p *Pool) Close() error {
	p.logStop()
	p.mu.Lock()
	boxes := p.boxes
	p.boxes = map[string]*Box{}
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var firstErr error
	for _, b := range boxes {
		if b.admin != nil {
			_ = b.admin.Close()
		}
		if err := p.docker.RemoveContainer(ctx, b.ContainerID); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ReapStale removes containers left behind by a previous process that exited
// without cleaning up.
func (p *Pool) ReapStale(ctx context.Context) (int, error) {
	ids, err := p.docker.ListByLabel(ctx, LabelKey, LabelValue)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		if err := p.docker.RemoveContainer(ctx, id); err == nil {
			n++
		}
	}
	return n, nil
}
