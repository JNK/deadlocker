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
	"strconv"
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
//
// Every flag here has to exist in every server the version matrix sweeps.
// mysqld rejects an unknown variable by aborting at startup, so one 8.0-only
// flag makes a 5.7 container exit immediately — which surfaces two minutes
// later as a connection timeout rather than as the one-line answer it is.
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
	// The error log deliberately stays at the image default, which is stderr.
	// Pointing --log-error at /dev/stderr does not work: mysqld appends ".err"
	// to the value and then fails to open /dev/stderr.err.
}

// serverArgsFor returns the arguments for one spec, adding the flags that only
// newer servers understand and anything the spec asks to be set at startup.
func serverArgsFor(spec Spec) []string {
	args := append([]string(nil), serverArgs...)
	if atLeast(spec.Image, 8, 0) {
		// Renamed from innodb_log_file_size in 8.0.30. 5.7 aborts on it.
		args = append(args, "--innodb-redo-log-capacity=16777216")
	} else {
		args = append(args, "--innodb-log-file-size=16M")
	}
	if spec.DeadlockDetect != nil && !*spec.DeadlockDetect {
		args = append(args, "--innodb-deadlock-detect=OFF")
	}
	return args
}

// Spec is what a scenario needs from a server. Anything here that cannot be set
// per session has to be baked into the container, because the alternative is a
// run reaching over and changing it for every other run sharing the server.
type Spec struct {
	Image string
	// DeadlockDetect is the one setting a scenario can ask for that MySQL only
	// exposes globally. A run that turned it off with SET GLOBAL turned it off
	// for every concurrent run on the same container -- silently converting
	// their deadlocks into lock waits that sit there until they time out. So a
	// scenario that wants it off gets a server of its own instead.
	DeadlockDetect *bool
}

// Key identifies the container a spec needs. Only settings that cannot be
// varied per session are part of it, so the common case still shares one
// container per image.
func (s Spec) Key() string {
	if s.DeadlockDetect != nil && !*s.DeadlockDetect {
		return s.image() + "#deadlock-detect=off"
	}
	return s.image()
}

func (s Spec) image() string {
	if s.Image == "" {
		return DefaultImage
	}
	return s.Image
}

// DefaultImage is used when a scenario names none.
const DefaultImage = "mysql:8.4"

// atLeast reports whether an image tag names a MySQL at or above major.minor.
// An unparseable tag is treated as current, since that is what "mysql:latest"
// and any bare image name mean in practice.
func atLeast(image string, major, minor int) bool {
	tag := ""
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		tag = image[i+1:]
	}
	parts := strings.SplitN(tag, ".", 3)
	if len(parts) < 2 {
		return true
	}
	maj, err1 := strconv.Atoi(parts[0])
	min, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return true
	}
	return maj > major || (maj == major && min >= minor)
}

// Box is a running MySQL container plus the proxy-free admin connection used
// for setup, teardown and lock introspection.
type Box struct {
	Spec        Spec
	Image       string
	ContainerID string
	HostPort    string
	StartedAt   time.Time

	admin *sql.DB
}

// Key is the pool key this box was started for.
func (b *Box) Key() string { return b.Spec.Key() }

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

// Pool lazily starts and reuses one Box per spec.
type Pool struct {
	docker *dockerctl.Client

	mu    sync.Mutex
	boxes map[string]*Box
	// starting guards against two runs racing to create the same box.
	starting map[string]chan struct{}

	// onLog is called with the pool key of the box a line came from, not with
	// its image: two boxes can share an image, and a run must only be shown the
	// log of the server it is actually talking to.
	onLog   func(key string, line dockerctl.LogLine)
	logCtx  context.Context
	logStop context.CancelFunc

	// owner identifies this process on the containers it starts, and ownerDir is
	// where the liveness locks live.
	owner    *Owner
	ownerDir string
}

func NewPool(dc *dockerctl.Client, onLog func(key string, line dockerctl.LogLine)) *Pool {
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

// Own records this process as the owner of every container the pool starts.
// Without it the pool still works; it just cannot tell its containers apart
// from anyone else's, so it reaps conservatively.
func (p *Pool) Own(o *Owner, dir string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.owner, p.ownerDir = o, dir
}

// Phases a container goes through before it can be used. They are the four
// things that actually take time, and naming them is the difference between
// "the button is stuck" and "it is pulling a 500 MB image".
const (
	PhaseCheck   = "check"   // asking Docker whether the image is here
	PhasePull    = "pull"    // fetching it from the registry
	PhaseCreate  = "create"  // creating and starting the container
	PhaseBoot    = "boot"    // waiting for mysqld to answer
	PhaseReady   = "ready"   // done
	PhaseWaiting = "waiting" // another run is already starting this image
)

// Progress is one observation about how far a container is from usable.
type Progress struct {
	Phase  string
	Detail string
	// Percent is 0..100 where it can be measured, and -1 where it cannot --
	// waiting for mysqld to boot has no denominator.
	Percent int
}

// Get returns a ready Box for the image with default settings.
func (p *Pool) Get(ctx context.Context, image string, onProgress func(Progress)) (*Box, error) {
	return p.Acquire(ctx, Spec{Image: image}, onProgress)
}

// Acquire returns a ready Box for the spec, starting the container if needed.
// onProgress receives status while pulling and booting.
func (p *Pool) Acquire(ctx context.Context, spec Spec, onProgress func(Progress)) (*Box, error) {
	spec.Image = spec.image()
	key := spec.Key()
	for {
		p.mu.Lock()
		if b, ok := p.boxes[key]; ok {
			p.mu.Unlock()
			return b, nil
		}
		if ch, ok := p.starting[key]; ok {
			// Someone else is booting this box; wait and re-check.
			p.mu.Unlock()
			if onProgress != nil {
				onProgress(Progress{
					Phase:   PhaseWaiting,
					Detail:  "another run is already starting " + spec.Image,
					Percent: -1,
				})
			}
			select {
			case <-ch:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		ch := make(chan struct{})
		p.starting[key] = ch
		p.mu.Unlock()

		b, err := p.start(ctx, spec, onProgress)

		p.mu.Lock()
		delete(p.starting, key)
		if err == nil {
			p.boxes[key] = b
		}
		p.mu.Unlock()
		close(ch)
		return b, err
	}
}

// emulated reports whether the image's architecture differs from the host's, in
// which case Docker is translating every instruction.
func (p *Pool) emulated(ctx context.Context, image string) bool {
	imageArch := p.docker.ImageArch(ctx, image)
	hostArch := p.docker.HostArch(ctx)
	return imageArch != "" && hostArch != "" && imageArch != hostArch
}

func (p *Pool) start(ctx context.Context, spec Spec, onProgress func(Progress)) (*Box, error) {
	image, key := spec.Image, spec.Key()
	emit := func(phase string, percent int, format string, args ...any) {
		if onProgress != nil {
			onProgress(Progress{Phase: phase, Detail: fmt.Sprintf(format, args...), Percent: percent})
		}
	}
	progress := func(format string, args ...any) { emit(PhaseBoot, -1, format, args...) }

	emit(PhaseCheck, -1, "checking for %s", image)
	if err := p.docker.PullImage(ctx, image, func(pp dockerctl.PullProgress) {
		emit(PhasePull, pp.Percent, "%s", describePull(image, pp))
	}); err != nil {
		return nil, fmt.Errorf("pull image: %w", err)
	}

	emit(PhaseCreate, -1, "creating the container")
	id, err := p.docker.CreateContainer(ctx, dockerctl.CreateConfig{
		Image: image,
		Cmd:   serverArgsFor(spec),
		Env: []string{
			"MYSQL_ROOT_PASSWORD=" + RootPassword,
			// Allow root from any host: the proxy connects from the host
			// network, not from localhost inside the container.
			"MYSQL_ROOT_HOST=%",
		},
		Labels: map[string]string{
			LabelKey:                  LabelValue,
			LabelOwner:                p.owner.ID(),
			"io.jnk.deadlocker.image": image,
			"io.jnk.deadlocker.spec":  key,
		},
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

	box := &Box{Spec: spec, Image: image, ContainerID: id, HostPort: port, StartedAt: time.Now()}

	// Start following the container log before waiting for readiness so boot
	// output (and any startup failure) is visible in the UI.
	if p.onLog != nil {
		go func() {
			_ = p.docker.StreamLogs(p.logCtx, id, time.Time{}, func(l dockerctl.LogLine) {
				p.onLog(key, l)
			})
		}()
	}

	// An image running under emulation initialises several times slower: an
	// emulated MySQL 5.7 first boot regularly exceeds two minutes, which read as
	// "5.7 never runs" rather than "5.7 is slow here".
	ready := 120 * time.Second
	if p.emulated(ctx, image) {
		// Emulation is slower, though far less than it sounds: an emulated 5.7
		// is ready in about ten seconds. A dead container is now detected
		// directly, so this budget only has to cover a genuinely slow start.
		ready = 4 * time.Minute
		progress("%s runs under emulation on this machine; allowing %s to start", image, ready)
	}

	emit(PhaseBoot, -1, "waiting for mysqld on %s", box.Addr())
	db, err := waitReady(ctx, box, ready, progress, func() error {
		// A container that has exited is never going to answer, and the reason
		// is already in its log. Waiting out the full timeout to report "dial
		// tcp: connection refused" hides a one-line answer for minutes —
		// which is exactly what an unsupported mysqld flag looked like.
		ins, insErr := p.docker.Inspect(ctx, id)
		if insErr != nil || ins.State.Running {
			return nil
		}
		return fmt.Errorf("the container exited before mysqld was ready (%s)%s",
			ins.State.Status, lastErrorLines(ctx, p.docker, id))
	})
	if err != nil {
		_ = p.docker.RemoveContainer(context.Background(), id)
		return nil, err
	}
	box.admin = db
	emit(PhaseReady, 100, "mysql ready on %s", box.Addr())
	return box, nil
}

// describePull renders aggregated pull progress as a sentence. The layer count
// is worth carrying: it is the difference between "this is nearly done" and
// "this is the first of fourteen".
func describePull(image string, pp dockerctl.PullProgress) string {
	switch pp.Phase {
	case "downloading":
		s := fmt.Sprintf("downloading %s", image)
		if pp.Total > 0 {
			s += fmt.Sprintf(" — %s of %s", humanBytes(pp.Current), humanBytes(pp.Total))
		}
		if pp.Layers > 0 {
			s += fmt.Sprintf(" (%d layers)", pp.Layers)
		}
		return s
	case "extracting":
		s := "extracting layers"
		if pp.Layers > 0 {
			s = fmt.Sprintf("extracting layers (%d of %d done)", pp.Complete, pp.Layers)
		}
		return s
	}
	return strings.TrimSpace(pp.Line)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// lastErrorLines pulls the tail of a dead container's log, so the reason it
// died travels with the error rather than only reaching the container log tab.
func lastErrorLines(ctx context.Context, docker *dockerctl.Client, id string) string {
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var lines []string
	_ = docker.StreamLogs(readCtx, id, time.Time{}, func(l dockerctl.LogLine) {
		t := strings.TrimSpace(l.Text)
		if t == "" {
			return
		}
		low := strings.ToLower(t)
		if strings.Contains(low, "error") || strings.Contains(low, "unknown variable") ||
			strings.Contains(low, "aborting") {
			lines = append(lines, t)
		}
	})
	if len(lines) == 0 {
		return ""
	}
	if len(lines) > 3 {
		lines = lines[len(lines)-3:]
	}
	return ": " + strings.Join(lines, " / ")
}

// waitReady polls until mysqld answers. checkAlive, when it returns an error,
// ends the wait early: there is no point waiting out a timeout for a process
// that is gone.
func waitReady(
	ctx context.Context, box *Box, timeout time.Duration,
	progress func(string, ...any), checkAlive func() error,
) (*sql.DB, error) {
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
		// Checked every couple of seconds rather than every attempt: an inspect
		// per 500ms would be noise against the Docker socket.
		if attempt%4 == 0 && checkAlive != nil {
			if dead := checkAlive(); dead != nil {
				return nil, dead
			}
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
// without cleaning up, and returns how many went and how many were left alone.
//
// Containers belonging to another Deadlocker that is still running are left
// alone. Reaping those was a foot-gun with teeth: opening a second instance, or
// running `deadlocker run` while the UI was up, deleted the MySQL the first one
// was in the middle of using.
func (p *Pool) ReapStale(ctx context.Context) (removed int, kept int, err error) {
	containers, err := p.docker.ListByLabel(ctx, LabelKey, LabelValue)
	if err != nil {
		return 0, 0, err
	}
	for _, c := range containers {
		owner := c.Labels[LabelOwner]
		// Our own containers are not stale, and another live instance's are not
		// ours to remove. Anything else -- no owner, or an owner that has
		// exited -- is litter.
		if owner != "" && (owner == p.owner.ID() || ownerAlive(p.ownerDir, owner)) {
			kept++
			continue
		}
		if rmErr := p.docker.RemoveContainer(ctx, c.ID); rmErr == nil {
			removed++
		}
	}
	return removed, kept, nil
}
