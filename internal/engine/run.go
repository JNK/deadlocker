package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/jnk/deadlocker/internal/casedef"
	"github.com/jnk/deadlocker/internal/mysqlbox"
	"github.com/jnk/deadlocker/internal/wire"
)

// StepStatus is the lifecycle of a single statement.
type StepStatus string

const (
	StepPending StepStatus = "pending"
	StepRunning StepStatus = "running" // submitted, still executing
	StepBlocked StepStatus = "blocked" // still executing past the settle window: waiting on a lock
	StepDone    StepStatus = "done"
	StepFailed  StepStatus = "error"
)

// Run status values.
const (
	StatusPreparing = "preparing"
	StatusReady     = "ready"
	StatusRunning   = "running"
	StatusFinished  = "finished"
	StatusFailed    = "failed"
	StatusClosed    = "closed"
)

// Error kinds we single out because they are the whole point of the tool.
const (
	ErrKindDeadlock = "deadlock"
	ErrKindTimeout  = "timeout"
	ErrKindOther    = "other"
)

const (
	errNoDeadlock   = 1213 // ER_LOCK_DEADLOCK
	errLockWaitTime = 1205 // ER_LOCK_WAIT_TIMEOUT
)

// SQLError is a MySQL error rendered for the UI.
type SQLError struct {
	Code     uint16 `json:"code"`
	SQLState string `json:"sql_state,omitempty"`
	Message  string `json:"message"`
	Kind     string `json:"kind"`
	// Hint explains the consequence, which differs sharply between the two
	// interesting cases: a deadlock rolls the whole transaction back, a lock
	// wait timeout by default rolls back only the statement.
	Hint string `json:"hint,omitempty"`
}

// StepResult is the observable outcome of one step.
type StepResult struct {
	Index     int    `json:"index"`
	Actor     string `json:"actor"`
	ActorName string `json:"actor_name"`
	Accent    string `json:"accent"`
	Label     string `json:"label"`
	SQL       string `json:"sql"`
	Note      string `json:"note,omitempty"`

	Status      StepStatus `json:"status"`
	SubmittedAt *time.Time `json:"submitted_at,omitempty"`
	EndedAt     *time.Time `json:"ended_at,omitempty"`
	DurationMS  int64      `json:"duration_ms"`

	Columns       []string   `json:"columns,omitempty"`
	Rows          [][]string `json:"rows,omitempty"`
	RowsTruncated bool       `json:"rows_truncated,omitempty"`
	RowCount      int        `json:"row_count"`
	RowsAffected  int64      `json:"rows_affected"`
	LastInsertID  int64      `json:"last_insert_id,omitempty"`
	Error         *SQLError  `json:"error,omitempty"`

	BlockedBy   []string `json:"blocked_by,omitempty"`
	WaitExplain string   `json:"wait_explain,omitempty"`
	// WasBlocked records that the statement waited on a lock at some point,
	// even if it later succeeded. The wait is the observation a scenario is
	// usually asserting on, so it must survive the step completing.
	WasBlocked bool `json:"was_blocked,omitempty"`

	// Plan is the optimizer's plan for this statement. Which index it picks is
	// what decides what gets locked, so this is the missing half of any
	// explanation involving a full scan.
	Plan []PlanRow `json:"plan,omitempty"`

	Expect      casedef.Expectation `json:"expect,omitempty"`
	Actual      casedef.Expectation `json:"actual,omitempty"`
	Verdict     string              `json:"verdict,omitempty"` // match | mismatch
	VerdictNote string              `json:"verdict_note,omitempty"`
}

func (s *StepResult) clone() *StepResult {
	c := *s
	c.Columns = append([]string(nil), s.Columns...)
	c.BlockedBy = append([]string(nil), s.BlockedBy...)
	if s.Rows != nil {
		c.Rows = make([][]string, len(s.Rows))
		for i, r := range s.Rows {
			c.Rows[i] = append([]string(nil), r...)
		}
	}
	if s.Error != nil {
		e := *s.Error
		c.Error = &e
	}
	return &c
}

// PlanRow is one row of EXPLAIN.
type PlanRow struct {
	ID           int    `json:"id"`
	SelectType   string `json:"select_type,omitempty"`
	Table        string `json:"table,omitempty"`
	Type         string `json:"type,omitempty"`
	PossibleKeys string `json:"possible_keys,omitempty"`
	Key          string `json:"key,omitempty"`
	KeyLen       string `json:"key_len,omitempty"`
	Ref          string `json:"ref,omitempty"`
	Rows         int64  `json:"rows"`
	Filtered     string `json:"filtered,omitempty"`
	Extra        string `json:"extra,omitempty"`
	// Explain reads the plan back in terms of locking.
	Explain string `json:"explain,omitempty"`
}

// explainable reports whether a plan is worth having. BEGIN, COMMIT and SET
// have none, and a plain INSERT ... VALUES reads nothing: MySQL still returns a
// dummy row for it, which would be read as a full table scan and be actively
// misleading.
func explainable(sqlText string) bool {
	up := strings.ToUpper(strings.TrimSpace(sqlText))

	if strings.HasPrefix(up, "INSERT") || strings.HasPrefix(up, "REPLACE") {
		// Only the INSERT ... SELECT form actually reads rows.
		return strings.Contains(up, " SELECT ")
	}
	for _, kw := range []string{"SELECT", "UPDATE", "DELETE", "WITH", "TABLE "} {
		if strings.HasPrefix(up, kw) {
			return true
		}
	}
	return false
}

// explainLocking turns a plan row into the sentence that matters here: what
// this access path means for the locks the statement will take.
func explainLocking(r PlanRow) string {
	switch strings.ToLower(r.Type) {
	case "all":
		return "Full table scan: no index is usable, so InnoDB reads and locks every row in the table, not just the matching ones."
	case "index":
		return "Full index scan: every entry in the index is read, so every corresponding row is locked."
	case "range":
		return "Range scan on " + orIndexName(r.Key) + ": locks the records read plus the gaps between them, which blocks inserts into that range."
	case "ref", "ref_or_null":
		return "Non-unique index lookup on " + orIndexName(r.Key) + ": locks the matching index entries, the rows they point to, and the gap around them."
	case "eq_ref", "const":
		return "Unique index lookup on " + orIndexName(r.Key) + ": locks just the matching record, with no gap lock."
	case "index_merge":
		return "Index merge across " + orIndexName(r.Key) + ": locks everything reached through each index used."
	}
	if strings.Contains(strings.ToLower(r.Extra), "impossible where") {
		return "The optimizer proved no row can match, so nothing is read and nothing is locked."
	}
	return ""
}

func orIndexName(k string) string {
	if k == "" || k == "NULL" {
		return "no index"
	}
	return k
}

// ActorState is one actor's live connection state.
type ActorState struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Accent    string `json:"accent"`
	ConnID    uint64 `json:"conn_id"`
	ProxyAddr string `json:"proxy_addr"`
	Busy      bool   `json:"busy"`
	InTrx     bool   `json:"in_trx"`
	StepIndex int    `json:"step_index"`
	// Standalone marks a session opened from the console rather than declared
	// by the scenario. It runs no steps; it exists to be typed into.
	Standalone bool `json:"standalone,omitempty"`
}

// PrepareState is how far a run is from being usable, published while the
// container is being pulled and booted.
//
// Without this the first run of an image is a button that does nothing for two
// minutes: the work is real, but all of it happens before there is a page to
// show it on.
type PrepareState struct {
	Phase  string `json:"phase"`
	Detail string `json:"detail,omitempty"`
	// Percent is 0..100 where the work has a denominator, -1 where it does not.
	Percent int `json:"percent"`
}

// RunState is the run summary the UI renders in the header.
type RunState struct {
	ID       string       `json:"id"`
	CaseID   string       `json:"case_id"`
	CaseName string       `json:"case_name"`
	Status   string       `json:"status"`
	Database string       `json:"database"`
	Image    string       `json:"image"`
	Addr     string       `json:"addr"`
	Cursor   int          `json:"cursor"`
	Total    int          `json:"total"`
	Actors   []ActorState `json:"actors"`
	// Sessions are the standalone connections opened from the console. They are
	// kept apart from Actors so the scenario's lanes stay the scenario's.
	Sessions []ActorState `json:"sessions,omitempty"`
	Message  string       `json:"message,omitempty"`
	Error    string       `json:"error,omitempty"`
	Started  time.Time    `json:"started"`

	// Prepare is set while the run is still coming up.
	Prepare *PrepareState `json:"prepare,omitempty"`

	DeadlockReport  string `json:"deadlock_report,omitempty"`
	Isolation       string `json:"isolation,omitempty"`
	LockWaitTimeout int    `json:"lock_wait_timeout,omitempty"`
	// Interrupted is set while a stop is pending.
	Interrupted string `json:"interrupted,omitempty"`
	// Ephemeral marks a run of an unsaved draft.
	Ephemeral bool `json:"ephemeral,omitempty"`
}

// actorConn is one simulated client: its own proxy, its own dedicated MySQL
// connection, held open for the whole run so transaction state survives across
// steps.
type actorConn struct {
	id     string
	name   string
	accent string
	// standalone marks a console session: same machinery, but it belongs to no
	// step in the scenario.
	standalone bool

	proxy *wire.Proxy
	db    *sql.DB
	conn  *sql.Conn
	// connID is CONNECTION_ID(), used to map performance_schema rows back here.
	connID uint64

	busy      bool
	inTrx     bool
	stepIndex int
	// consoleID tags the packets of a console statement that is in flight, so
	// the wire panel can tell typed SQL from the scenario's own.
	consoleID int
}

func (a *actorConn) state() ActorState {
	return ActorState{
		ID: a.id, Name: a.name, Accent: a.accent,
		ConnID: a.connID, Busy: a.busy, InTrx: a.inTrx,
		StepIndex: a.stepIndex, Standalone: a.standalone,
		ProxyAddr: proxyAddr(a),
	}
}

// Run is one execution of a scenario.
type Run struct {
	ID   string
	Case *casedef.Case
	Bus  *Bus

	settleWindow time.Duration

	box      *mysqlbox.Box
	database string
	setupDB  *sql.DB

	execCtx    context.Context
	execCancel context.CancelFunc

	mu         sync.Mutex
	status     string
	message    string
	runErr     string
	cursor     int
	steps      []*StepResult
	actors     map[string]*actorConn
	actorOrder []string
	deadlock   string
	startedAt  time.Time
	closed     bool

	// prepare is the live progress of pulling and booting the container.
	prepare *PrepareState
	// ready is closed once setup has finished, successfully or not, so anything
	// that needs a usable run can wait for one instead of being refused.
	ready    chan struct{}
	setupErr error

	// consoles are the standalone connections opened from the SQL console, and
	// consoleSeq numbers both those sessions and the statements typed into them.
	consoles     map[string]*actorConn
	consoleOrder []string
	consoleSeq   int
	entrySeq     int
	// lastLocks is the most recent snapshot, kept so a history record can carry
	// the lock landscape the run ended with.
	lastLocks *LockSnapshot

	// onState is called after every state change so the manager can refresh
	// this run's history record. It must not call back into the run's lock.
	onState func(*Run)

	// interrupt is set when a human stops a run the assistant is driving. The
	// agent's next step_run returns immediately and reports why, so the model
	// learns it was stopped on purpose rather than seeing an unexplained halt.
	interrupt string
}

// Spec is the server this run needs. Anything a scenario asks for that MySQL
// only exposes globally decides which container it lands on, so that concurrent
// runs cannot reconfigure each other's server.
func (r *Run) Spec() mysqlbox.Spec {
	return mysqlbox.Spec{
		Image:          r.Case.MySQL.Image,
		DeadlockDetect: r.Case.MySQL.DeadlockDetect,
	}
}

var (
	// ErrNoMoreSteps is returned when the scenario has been fully played.
	ErrNoMoreSteps = errors.New("no more steps in this scenario")
)

// ActorBlockedError is returned when the next step belongs to an actor whose
// previous statement is still waiting on a lock. That is not a tool limitation
// but the real constraint: one connection runs one statement at a time.
type ActorBlockedError struct {
	Actor     string
	ActorName string
	StepIndex int
}

func (e *ActorBlockedError) Error() string {
	if e.StepIndex == 0 {
		// A console session runs no steps, so there is no step number to blame.
		return fmt.Sprintf("%s is still running a statement — a connection can only run one at a time. Use another session, or wait for the lock to resolve.",
			e.ActorName)
	}
	return fmt.Sprintf("%s is still waiting on step %d — a connection can only run one statement at a time. Advance another actor, or wait for the lock to resolve.",
		e.ActorName, e.StepIndex)
}

// New builds a run without touching Docker: the scenario's steps and actors are
// laid out immediately, so the run has a page worth showing while its container
// is still being pulled. Setup does the slow half.
func New(id string, c *casedef.Case, settle time.Duration) *Run {
	execCtx, cancel := context.WithCancel(context.Background())
	r := &Run{
		ID:           id,
		Case:         c,
		Bus:          NewBus(),
		settleWindow: settle,
		execCtx:      execCtx,
		execCancel:   cancel,
		status:       StatusPreparing,
		actors:       map[string]*actorConn{},
		consoles:     map[string]*actorConn{},
		ready:        make(chan struct{}),
		startedAt:    time.Now(),
		database:     "dl_" + id,
		prepare:      &PrepareState{Phase: mysqlbox.PhaseCheck, Detail: "starting", Percent: -1},
	}
	for i, st := range c.Steps {
		actor, _ := c.Actor(st.Actor)
		r.steps = append(r.steps, &StepResult{
			Index:     i + 1,
			Actor:     st.Actor,
			ActorName: actor.Name,
			Accent:    actor.Accent,
			Label:     st.Label,
			SQL:       st.SQL,
			Note:      st.Note,
			Status:    StepPending,
			Expect:    st.Expect,
		})
	}
	// The actors exist before their connections do. Naming them now is what lets
	// the run page draw its lanes while the container is still coming up.
	for _, a := range c.Actors {
		r.actors[a.ID] = &actorConn{id: a.ID, name: a.Name, accent: a.Accent}
		r.actorOrder = append(r.actorOrder, a.ID)
	}
	return r
}

// Prepare builds a run and brings it all the way up, returning only once it is
// ready to be stepped.
func Prepare(ctx context.Context, id string, c *casedef.Case, pool *mysqlbox.Pool, settle time.Duration) (*Run, error) {
	r := New(id, c, settle)
	return r, r.Setup(ctx, pool)
}

// Setup creates the scratch database, applies schema and seed data, and opens
// one proxied connection per actor. It reports progress on the run's bus as it
// goes, and closes the run's ready channel when it is done either way.
func (r *Run) Setup(ctx context.Context, pool *mysqlbox.Pool) error {
	err := r.setup(ctx, pool)

	r.mu.Lock()
	r.setupErr = err
	if err != nil {
		r.status = StatusFailed
		r.runErr = err.Error()
	} else {
		r.status = StatusReady
		r.message = "ready — step through the scenario"
		r.prepare = nil
	}
	select {
	case <-r.ready:
	default:
		close(r.ready)
	}
	r.mu.Unlock()

	if err != nil {
		r.logf("error", "setup failed: %v", err)
		r.publishState()
		// Release whatever did get created.
		r.Close()
		return err
	}
	r.publishState()
	return nil
}

// WaitReady blocks until setup has finished, and reports what it concluded. A
// run that is still pulling an image is not broken, so anything that wants to
// step it waits rather than being told no.
func (r *Run) WaitReady(ctx context.Context) error {
	r.mu.Lock()
	ready := r.ready
	r.mu.Unlock()
	if ready == nil {
		return nil
	}
	select {
	case <-ready:
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.setupErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// setProgress records a preparation phase and publishes it, throttled: a pull
// emits progress dozens of times a second and every publish is an SSE frame to
// every watcher.
func (r *Run) setProgress(p mysqlbox.Progress) {
	r.mu.Lock()
	prev := r.prepare
	next := &PrepareState{Phase: p.Phase, Detail: p.Detail, Percent: p.Percent}
	worthPublishing := prev == nil || prev.Phase != next.Phase ||
		next.Percent < 0 || next.Percent-prev.Percent >= 1 || next.Percent == 100
	r.prepare = next
	r.mu.Unlock()

	if !worthPublishing {
		return
	}
	// The activity log gets phase changes and whole percentage points, not the
	// byte-by-byte stream.
	if prev == nil || prev.Phase != next.Phase || next.Percent < 0 {
		r.logf("info", "%s", p.Detail)
	}
	r.publishState()
}

func (r *Run) setup(ctx context.Context, pool *mysqlbox.Pool) error {
	spec := r.Spec()
	r.logf("info", "acquiring MySQL container (%s)", r.Case.MySQL.Image)
	if spec.DeadlockDetect != nil && !*spec.DeadlockDetect {
		r.logf("info", "this scenario needs innodb_deadlock_detect=OFF, which is a global: it gets a container of its own so no other run is affected")
	}
	box, err := pool.Acquire(ctx, spec, r.setProgress)
	if err != nil {
		return err
	}
	r.box = box

	r.setProgress(mysqlbox.Progress{
		Phase: mysqlbox.PhaseReady, Detail: "creating the scratch database", Percent: -1,
	})
	admin := box.Admin()
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+r.database+"`"); err != nil {
		return fmt.Errorf("create database: %w", err)
	}
	r.logf("info", "created scratch database %s", r.database)

	setupDB, err := sql.Open("mysql", mysqlbox.DSN(box.Addr(), r.database))
	if err != nil {
		return fmt.Errorf("open setup connection: %w", err)
	}
	setupDB.SetMaxOpenConns(4)
	r.setupDB = setupDB

	r.setProgress(mysqlbox.Progress{
		Phase: mysqlbox.PhaseReady, Detail: "applying the schema", Percent: -1,
	})
	for i, stmt := range r.Case.Schema {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := setupDB.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("schema statement %d: %w", i+1, err)
		}
	}
	if n := len(r.Case.Schema); n > 0 {
		r.logf("info", "applied %d schema statement(s)", n)
	}
	for i, stmt := range r.Case.Seed {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := setupDB.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("seed statement %d: %w", i+1, err)
		}
	}
	if n := len(r.Case.Seed); n > 0 {
		r.logf("info", "applied %d seed statement(s)", n)
	}

	r.setProgress(mysqlbox.Progress{
		Phase: mysqlbox.PhaseReady, Detail: "connecting the actors", Percent: -1,
	})
	for _, a := range r.Case.Actors {
		if err := r.openActor(ctx, a); err != nil {
			return fmt.Errorf("actor %s: %w", a.ID, err)
		}
	}
	return nil
}

func (r *Run) openActor(ctx context.Context, a casedef.Actor) error {
	r.mu.Lock()
	ac := r.actors[a.ID]
	if ac == nil {
		ac = &actorConn{id: a.ID, name: a.Name, accent: a.Accent}
		r.actors[a.ID] = ac
		r.actorOrder = append(r.actorOrder, a.ID)
	}
	r.mu.Unlock()

	if err := r.dial(ctx, ac); err != nil {
		return err
	}
	r.logf("info", "%s connected as MySQL connection %d via proxy %s", ac.name, ac.connID, ac.proxy.Addr())
	return nil
}

// dial gives one session its own proxy and its own dedicated connection.
//
// Actors and console sessions go through the same path deliberately: a
// standalone connection you type into is only a fair comparison with the
// scenario's own sessions if it was configured the same way — same isolation
// level, same lock wait timeout, same session variables.
func (r *Run) dial(ctx context.Context, ac *actorConn) error {
	proxy, err := wire.Listen(r.box.Addr(), ac.id, func(ev wire.Event) {
		r.onWire(ev)
	})
	if err != nil {
		return err
	}
	ac.proxy = proxy

	dsn := mysqlbox.DSN(proxy.Addr(), r.database)
	if r.Case.MySQL.Prepared {
		dsn = mysqlbox.DSNPrepared(proxy.Addr(), r.database)
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		proxy.Close()
		return err
	}
	// One connection, held for the run: transaction state must persist between
	// steps, and the captured wire trace must belong to a single session.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)
	ac.db = db

	conn, err := db.Conn(ctx)
	if err != nil {
		db.Close()
		proxy.Close()
		return err
	}
	ac.conn = conn

	if err := conn.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&ac.connID); err != nil {
		conn.Close()
		db.Close()
		proxy.Close()
		return err
	}

	if to := r.Case.MySQL.LockWaitTimeout; to > 0 {
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("SET SESSION innodb_lock_wait_timeout = %d", to)); err != nil {
			return err
		}
	}
	if iso := r.Case.NormalisedIsolation(); iso != "" {
		if _, err := conn.ExecContext(ctx, "SET SESSION TRANSACTION ISOLATION LEVEL "+iso); err != nil {
			return err
		}
	}
	for k, v := range r.Case.MySQL.Vars {
		if !safeIdentifier(k) {
			return fmt.Errorf("invalid session variable name %q", k)
		}
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("SET SESSION %s = %s", k, quoteVarValue(v))); err != nil {
			return fmt.Errorf("set session %s: %w", k, err)
		}
	}
	return nil
}

// safeIdentifier guards the session variable names we interpolate. Values go
// through quoteVarValue; names cannot be parameterised in SET statements.
func safeIdentifier(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// quoteVarValue passes bare numbers and known keywords through, and quotes
// everything else as a string literal.
func quoteVarValue(v string) string {
	t := strings.TrimSpace(v)
	if t == "" {
		return "''"
	}
	switch strings.ToUpper(t) {
	case "ON", "OFF", "DEFAULT", "TRUE", "FALSE", "NULL":
		return strings.ToUpper(t)
	}
	isNum := true
	for _, r := range t {
		if !(r >= '0' && r <= '9' || r == '.' || r == '-' || r == '+') {
			isNum = false
			break
		}
	}
	if isNum {
		return t
	}
	return "'" + strings.ReplaceAll(strings.ReplaceAll(t, `\`, `\\`), `'`, `\'`) + "'"
}

// onWire tags a decoded packet with the step that was in flight for that
// session, or with the console statement if one is running instead.
func (r *Run) onWire(ev wire.Event) {
	r.mu.Lock()
	idx, console := 0, 0
	if a := r.sessionLocked(ev.Actor); a != nil {
		idx = a.stepIndex
		console = a.consoleID
	}
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return
	}
	r.Bus.Publish(Event{
		Type: EventWire, At: ev.At, RunID: r.ID,
		Wire: &WireEvent{Event: ev, StepIndex: idx, ConsoleID: console},
	})
}

// sessionLocked looks an id up among the actors and then the console sessions.
// Callers must hold r.mu.
func (r *Run) sessionLocked(id string) *actorConn {
	if a, ok := r.actors[id]; ok {
		return a
	}
	if a, ok := r.consoles[id]; ok {
		return a
	}
	return nil
}

// AppendDockerLog forwards a container log line into this run's timeline.
func (r *Run) AppendDockerLog(stream string, at time.Time, text string) {
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return
	}
	if at.IsZero() {
		at = time.Now()
	}
	r.Bus.Publish(Event{
		Type: EventDocker, At: at, RunID: r.ID,
		Docker: &DockerLine{Stream: stream, At: at, Text: text, Deadlock: isDeadlockLogLine(text)},
	})
}

func isDeadlockLogLine(s string) bool {
	l := strings.ToLower(s)
	return strings.Contains(l, "deadlock") || strings.Contains(l, "*** (1) transaction") ||
		strings.Contains(l, "*** (2) transaction") || strings.Contains(l, "we roll back")
}

func (r *Run) logf(level, format string, args ...any) {
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed && level != "error" {
		return
	}
	r.Bus.Publish(Event{Type: EventLog, RunID: r.ID, Level: level, Message: fmt.Sprintf(format, args...)})
}

// Interrupt asks the run to stop advancing. The reason is handed to whatever
// is stepping it, which for the assistant means its tool call comes back
// explaining that a person intervened.
func (r *Run) Interrupt(reason string) {
	if reason == "" {
		reason = "stopped by the user"
	}
	r.mu.Lock()
	r.interrupt = reason
	r.mu.Unlock()
	r.logf("warn", "%s", reason)
	r.publishState()
}

// TakeInterrupt returns and clears any pending interrupt, so a later request to
// continue is not refused by a stale flag.
func (r *Run) TakeInterrupt() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	reason := r.interrupt
	r.interrupt = ""
	return reason
}

// Interrupted reports whether a stop is pending, without clearing it.
func (r *Run) Interrupted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.interrupt != ""
}

// State returns a snapshot of the run state.
func (r *Run) State() RunState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stateLocked()
}

func (r *Run) stateLocked() RunState {
	s := RunState{
		ID:       r.ID,
		CaseID:   r.Case.ID,
		CaseName: r.Case.Name,
		Status:   r.status,
		Database: r.database,
		Image:    r.Case.MySQL.Image,
		Cursor:   r.cursor,
		Total:    len(r.steps),
		Message:  r.message,
		Error:    r.runErr,
		Started:  r.startedAt,

		DeadlockReport:  r.deadlock,
		Isolation:       r.Case.NormalisedIsolation(),
		LockWaitTimeout: r.Case.MySQL.LockWaitTimeout,
		Interrupted:     r.interrupt,
		Ephemeral:       r.Case.Ephemeral,
		Prepare:         r.prepare,
	}
	if r.box != nil {
		s.Addr = r.box.Addr()
	}
	for _, id := range r.actorOrder {
		s.Actors = append(s.Actors, r.actors[id].state())
	}
	for _, id := range r.consoleOrder {
		s.Sessions = append(s.Sessions, r.consoles[id].state())
	}
	return s
}

func proxyAddr(a *actorConn) string {
	if a.proxy == nil {
		return ""
	}
	return a.proxy.Addr()
}

// Steps returns a copy of every step's current result.
func (r *Run) Steps() []*StepResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*StepResult, len(r.steps))
	for i, s := range r.steps {
		out[i] = s.clone()
	}
	return out
}

func (r *Run) publishState() {
	r.mu.Lock()
	closed := r.closed
	st := r.stateLocked()
	hook := r.onState
	r.mu.Unlock()
	if closed {
		return
	}
	r.Bus.Publish(Event{Type: EventState, RunID: r.ID, State: &st})
	if hook != nil {
		hook(r)
	}
}

// publishLocks records and broadcasts a lock snapshot.
func (r *Run) publishLocks(snap LockSnapshot) {
	r.mu.Lock()
	r.lastLocks = &snap
	r.mu.Unlock()
	r.Bus.Publish(Event{Type: EventLocks, RunID: r.ID, Locks: &snap})
}

func (r *Run) publishStep(s *StepResult) {
	r.Bus.Publish(Event{Type: EventStep, RunID: r.ID, Step: s})
}

// Step submits the next statement in the scenario.
//
// It returns as soon as the statement either finishes or has been waiting long
// enough to be classified as blocked. A blocked statement keeps running in the
// background; when it eventually succeeds, times out or is chosen as a deadlock
// victim, that update is published on the bus.
func (r *Run) Step(ctx context.Context) (*StepResult, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, errors.New("run has been closed")
	}
	if r.status == StatusPreparing || r.status == StatusFailed {
		st := r.status
		r.mu.Unlock()
		return nil, fmt.Errorf("run is %s", st)
	}
	if r.cursor >= len(r.steps) {
		r.mu.Unlock()
		return nil, ErrNoMoreSteps
	}
	idx := r.cursor
	st := r.steps[idx]
	a, ok := r.actors[st.Actor]
	if !ok {
		r.mu.Unlock()
		return nil, fmt.Errorf("step %d references unknown actor %q", idx+1, st.Actor)
	}
	if a.busy {
		blockedOn := a.stepIndex
		name := a.name
		r.mu.Unlock()
		return nil, &ActorBlockedError{Actor: st.Actor, ActorName: name, StepIndex: blockedOn}
	}

	r.cursor++
	now := time.Now()
	st.Status = StepRunning
	st.SubmittedAt = &now
	st.EndedAt = nil
	st.Error = nil
	st.BlockedBy = nil
	st.WaitExplain = ""
	st.Verdict = ""
	st.Actual = ""
	a.busy = true
	a.stepIndex = idx + 1
	r.status = StatusRunning
	r.message = ""
	stepCopy := st.clone()
	r.mu.Unlock()

	r.publishStep(stepCopy)
	r.publishState()

	done := make(chan struct{})
	go r.execute(idx, done)

	select {
	case <-done:
	case <-time.After(r.settleWindow):
	case <-ctx.Done():
	}

	// A snapshot right here is the payoff: for a blocked step it shows exactly
	// which lock mode is being waited on and who holds it.
	snap := r.snapshot(context.Background())

	r.mu.Lock()
	if st.Status == StepRunning {
		st.Status = StepBlocked
		st.WasBlocked = true
		st.BlockedBy = snap.BlockedBy(st.Actor)
		if w := snap.WaitFor(st.Actor); w != nil {
			st.WaitExplain = describeWait(w)
		}
		r.evaluateLocked(st)
	}
	out := st.clone()
	r.mu.Unlock()

	r.publishLocks(snap)
	r.publishStep(out)
	r.publishState()
	return out, nil
}

func describeWait(w *LockWait) string {
	var b strings.Builder
	if w.BlockingActor != "" {
		fmt.Fprintf(&b, "Waiting for a lock held by %s. ", w.BlockingActor)
	} else {
		b.WriteString("Waiting for a lock held by another transaction. ")
	}
	if w.WaitingLock != nil {
		fmt.Fprintf(&b, "Requesting %s on %s.%s (%s): %s ",
			w.WaitingLock.LockMode, w.WaitingLock.Table, w.WaitingLock.Index,
			emptyDash(w.WaitingLock.LockData), w.WaitingLock.Explain)
	}
	if w.BlockingLock != nil {
		fmt.Fprintf(&b, "Blocked by %s on %s.%s (%s): %s",
			w.BlockingLock.LockMode, w.BlockingLock.Table, w.BlockingLock.Index,
			emptyDash(w.BlockingLock.LockData), w.BlockingLock.Explain)
	}
	return strings.TrimSpace(b.String())
}

func emptyDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// execute runs one statement to completion on its actor's connection.
func (r *Run) execute(idx int, done chan struct{}) {
	r.mu.Lock()
	st := r.steps[idx]
	a := r.actors[st.Actor]
	sqlText := st.SQL
	r.mu.Unlock()

	caseStep := r.Case.Steps[idx]

	// Explain first, on the admin connection: it takes no row locks and must
	// not disturb the actor's transaction. A plan is worth having even when the
	// statement itself is about to block.
	plan := r.explainStep(sqlText, caseStep.Args)

	start := time.Now()
	res := runStatement(r.execCtx, a.conn, sqlText, caseStep.Args)
	end := time.Now()

	r.mu.Lock()
	st.EndedAt = &end
	st.Plan = plan
	st.DurationMS = end.Sub(start).Milliseconds()
	st.Columns = res.columns
	st.Rows = res.rows
	st.RowCount = res.rowCount
	st.RowsTruncated = res.truncated
	st.RowsAffected = res.affected
	st.LastInsertID = res.lastInsertID
	if res.err != nil {
		st.Status = StepFailed
		st.Error = res.err
	} else {
		st.Status = StepDone
	}
	a.busy = false
	a.inTrx = trackTransaction(a.inTrx, sqlText, res.err)
	r.evaluateLocked(st)
	out := st.clone()
	finished := r.cursor >= len(r.steps) && !r.anyBusyLocked()
	if finished && r.status == StatusRunning {
		r.status = StatusFinished
	}
	isDeadlock := res.err != nil && res.err.Kind == ErrKindDeadlock
	r.mu.Unlock()

	close(done)
	r.publishStep(out)

	if isDeadlock {
		r.captureDeadlockReport()
	}
	// The lock landscape always changes when a statement completes; publishing
	// a fresh snapshot keeps the UI honest for steps that unblock later.
	snap := r.snapshot(context.Background())
	r.publishLocks(snap)
	r.publishState()
}

// explainStep asks the optimizer what it would do, on a connection that holds
// no locks of its own.
func (r *Run) explainStep(sqlText string, args []any) []PlanRow {
	if !explainable(sqlText) {
		return nil
	}
	r.mu.Lock()
	db := r.setupDB
	r.mu.Unlock()
	if db == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	rows, err := db.QueryContext(ctx, "EXPLAIN "+sqlText, args...)
	if err != nil {
		// A statement the optimizer refuses to explain is not an error worth
		// surfacing; the step itself is what matters.
		return nil
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil
	}

	var out []PlanRow
	for rows.Next() {
		raw := make([]sql.RawBytes, len(cols))
		scan := make([]any, len(cols))
		for i := range raw {
			scan[i] = &raw[i]
		}
		if err := rows.Scan(scan...); err != nil {
			return out
		}
		var pr PlanRow
		for i, c := range cols {
			v := string(raw[i])
			switch strings.ToLower(c) {
			case "id":
				fmt.Sscanf(v, "%d", &pr.ID)
			case "select_type":
				pr.SelectType = v
			case "table":
				pr.Table = v
			case "type":
				pr.Type = v
			case "possible_keys":
				pr.PossibleKeys = v
			case "key":
				pr.Key = v
			case "key_len":
				pr.KeyLen = v
			case "ref":
				pr.Ref = v
			case "rows":
				fmt.Sscanf(v, "%d", &pr.Rows)
			case "filtered":
				pr.Filtered = v
			case "extra":
				pr.Extra = v
			}
		}
		pr.Explain = explainLocking(pr)
		out = append(out, pr)
	}
	return out
}

func (r *Run) anyBusyLocked() bool {
	for _, a := range r.actors {
		if a.busy {
			return true
		}
	}
	return false
}

// trackTransaction keeps a best-effort view of whether the session holds an
// open transaction, for the actor badges in the UI. The authoritative signal is
// the IN_TRANS status flag decoded from OK packets in the wire panel.
func trackTransaction(current bool, sqlText string, err *SQLError) bool {
	if err != nil && err.Kind == ErrKindDeadlock {
		// InnoDB rolls the whole transaction back when it picks a victim.
		return false
	}
	head := strings.ToUpper(strings.TrimSpace(sqlText))
	switch {
	case strings.HasPrefix(head, "BEGIN"), strings.HasPrefix(head, "START TRANSACTION"):
		return err == nil
	case strings.HasPrefix(head, "COMMIT"), strings.HasPrefix(head, "ROLLBACK"):
		if err == nil {
			return false
		}
	}
	return current
}

func (r *Run) captureDeadlockReport() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	report, err := LatestDeadlock(ctx, r.box.Admin())
	if err != nil {
		r.logf("warn", "could not read the InnoDB deadlock report: %v", err)
		return
	}
	if report == "" {
		return
	}
	r.mu.Lock()
	r.deadlock = report
	r.mu.Unlock()
	r.logf("warn", "InnoDB detected a deadlock and rolled back a victim transaction")
	r.publishState()
}

type stmtResult struct {
	columns      []string
	rows         [][]string
	rowCount     int
	truncated    bool
	affected     int64
	lastInsertID int64
	err          *SQLError
}

const maxRows = 200

// runStatement executes one statement, choosing between the query and exec
// paths by inspecting the leading keyword.
func runStatement(ctx context.Context, conn *sql.Conn, sqlText string, args []any) stmtResult {
	var res stmtResult
	if returnsRows(sqlText) {
		rows, err := conn.QueryContext(ctx, sqlText, args...)
		if err != nil {
			res.err = toSQLError(err)
			return res
		}
		defer rows.Close()

		cols, err := rows.Columns()
		if err != nil {
			res.err = toSQLError(err)
			return res
		}
		res.columns = cols

		holders := make([]sql.RawBytes, len(cols))
		scan := make([]any, len(cols))
		for i := range holders {
			scan[i] = &holders[i]
		}
		for rows.Next() {
			res.rowCount++
			if len(res.rows) >= maxRows {
				res.truncated = true
				continue
			}
			if err := rows.Scan(scan...); err != nil {
				res.err = toSQLError(err)
				return res
			}
			row := make([]string, len(cols))
			for i, h := range holders {
				if h == nil {
					row[i] = "NULL"
				} else {
					row[i] = string(h)
				}
			}
			res.rows = append(res.rows, row)
		}
		if err := rows.Err(); err != nil {
			res.err = toSQLError(err)
		}
		return res
	}

	out, err := conn.ExecContext(ctx, sqlText, args...)
	if err != nil {
		res.err = toSQLError(err)
		return res
	}
	if n, err := out.RowsAffected(); err == nil {
		res.affected = n
	}
	if n, err := out.LastInsertId(); err == nil {
		res.lastInsertID = n
	}
	return res
}

// returnsRows decides which database/sql path a statement needs.
func returnsRows(sqlText string) bool {
	s := strings.TrimLeft(sqlText, " \t\r\n(")
	// Skip leading line comments so "-- note\nSELECT …" is classified correctly.
	for strings.HasPrefix(s, "--") || strings.HasPrefix(s, "#") {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = strings.TrimLeft(s[i+1:], " \t\r\n(")
		} else {
			return false
		}
	}
	upper := strings.ToUpper(s)
	for _, kw := range []string{"SELECT", "SHOW", "EXPLAIN", "DESCRIBE", "DESC ", "WITH", "TABLE ", "VALUES ", "ANALYZE TABLE", "CHECK TABLE"} {
		if strings.HasPrefix(upper, kw) {
			return true
		}
	}
	return false
}

func toSQLError(err error) *SQLError {
	if err == nil {
		return nil
	}
	out := &SQLError{Message: err.Error(), Kind: ErrKindOther}
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		out.Code = me.Number
		out.SQLState = string(me.SQLState[:])
		out.Message = me.Message
		switch me.Number {
		case errNoDeadlock:
			out.Kind = ErrKindDeadlock
			out.Hint = "InnoDB detected a cycle in the wait-for graph and chose this transaction as the victim. The entire transaction was rolled back, not just this statement — the application must retry it from the beginning."
		case errLockWaitTime:
			out.Kind = ErrKindTimeout
			out.Hint = "The statement waited longer than innodb_lock_wait_timeout. With the default innodb_rollback_on_timeout=OFF only this statement was rolled back; the transaction is still open and still holds every lock it had already acquired."
		}
	}
	return out
}

// evaluateLocked compares the observed outcome against the scenario's stated
// expectation. Callers must hold r.mu.
func (r *Run) evaluateLocked(st *StepResult) {
	st.Actual = classify(st)
	if st.Expect == casedef.ExpectAny {
		st.Verdict = ""
		return
	}
	if matchesExpectation(st.Expect, st.Actual) {
		st.Verdict = "match"
		st.VerdictNote = ""
		return
	}
	// "blocks" asserts that the statement hit a lock wait, not that it is still
	// waiting now. A step that blocked and then completed, timed out, or was
	// rolled back as a deadlock victim still satisfies the claim.
	if st.Expect == casedef.ExpectBlocks && st.WasBlocked {
		st.Verdict = "match"
		st.VerdictNote = fmt.Sprintf("blocked as expected, then %s", st.Actual)
		return
	}
	st.Verdict = "mismatch"
	st.VerdictNote = fmt.Sprintf("expected %s, observed %s", st.Expect, st.Actual)
}

func classify(st *StepResult) casedef.Expectation {
	switch st.Status {
	case StepBlocked, StepRunning:
		return casedef.ExpectBlocks
	case StepDone:
		return casedef.ExpectOK
	case StepFailed:
		if st.Error != nil {
			switch st.Error.Kind {
			case ErrKindDeadlock:
				return casedef.ExpectDeadlock
			case ErrKindTimeout:
				return casedef.ExpectTimeout
			}
		}
		return casedef.ExpectError
	}
	return casedef.ExpectAny
}

// matchesExpectation treats "error" as the general case covering deadlock and
// timeout, so a scenario can assert loosely or precisely.
func matchesExpectation(want, got casedef.Expectation) bool {
	if want == got {
		return true
	}
	if want == casedef.ExpectError {
		return got == casedef.ExpectDeadlock || got == casedef.ExpectTimeout
	}
	return false
}

func (r *Run) snapshot(ctx context.Context) LockSnapshot {
	r.mu.Lock()
	db := r.setupDB
	schema := r.database
	byConn := make(map[uint64]string, len(r.actors)+len(r.consoles))
	for _, a := range r.actors {
		byConn[a.connID] = a.id
	}
	// Console sessions are mapped too: a standalone connection that takes a lock
	// has to appear in the lock table and in the wait-for graph, or the console
	// would be a way to make a run lie about itself.
	for _, a := range r.consoles {
		byConn[a.connID] = a.id
	}
	closed := r.closed
	r.mu.Unlock()

	if db == nil || closed {
		return LockSnapshot{At: time.Now()}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return Snapshot(ctx, db, schema, byConn)
}

// Snapshot exposes an on-demand lock snapshot for the UI's refresh button.
func (r *Run) Snapshot() LockSnapshot {
	snap := r.snapshot(context.Background())
	r.publishLocks(snap)
	return snap
}

// DeadlockReport returns the captured InnoDB report, refreshing it on demand.
func (r *Run) DeadlockReport() string {
	r.mu.Lock()
	existing := r.deadlock
	box := r.box
	r.mu.Unlock()
	if existing != "" || box == nil {
		return existing
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	report, err := LatestDeadlock(ctx, box.Admin())
	if err != nil {
		return ""
	}
	return report
}

// Close tears the run down: kills any statement still waiting on a lock,
// releases connections and proxies, and drops the scratch database.
func (r *Run) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	actors := make([]*actorConn, 0, len(r.actors)+len(r.consoles))
	for _, id := range r.actorOrder {
		actors = append(actors, r.actors[id])
	}
	for _, id := range r.consoleOrder {
		actors = append(actors, r.consoles[id])
	}
	box, setupDB, database := r.box, r.setupDB, r.database
	r.status = StatusClosed
	// Release anything still waiting for a run that is never going to be ready.
	select {
	case <-r.ready:
	default:
		if r.setupErr == nil {
			r.setupErr = errors.New("the run was closed before it was ready")
		}
		close(r.ready)
	}
	r.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Kill first: a connection parked on a lock wait will not respond to a
	// polite Close until its statement returns.
	if box != nil {
		for _, a := range actors {
			if a.connID != 0 {
				_, _ = box.Admin().ExecContext(ctx, fmt.Sprintf("KILL CONNECTION %d", a.connID))
			}
		}
	}
	r.execCancel()

	for _, a := range actors {
		if a.conn != nil {
			_ = a.conn.Close()
		}
		if a.db != nil {
			_ = a.db.Close()
		}
		if a.proxy != nil {
			_ = a.proxy.Close()
		}
	}
	if setupDB != nil {
		_ = setupDB.Close()
	}

	// Nothing global has to be put back: everything a scenario can vary is
	// either per session or baked into a container of its own, so tearing a run
	// down leaves the server exactly as the next run expects to find it.
	var firstErr error
	if box != nil && database != "" {
		if _, err := box.Admin().ExecContext(ctx, "DROP DATABASE IF EXISTS `"+database+"`"); err != nil {
			firstErr = err
		}
	}

	r.Bus.Publish(Event{Type: EventLog, RunID: r.ID, Level: "info", Message: "run closed and scratch database dropped"})
	r.Bus.Close()
	return firstErr
}
