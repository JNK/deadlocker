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
	Message  string       `json:"message,omitempty"`
	Error    string       `json:"error,omitempty"`
	Started  time.Time    `json:"started"`

	DeadlockReport  string `json:"deadlock_report,omitempty"`
	Isolation       string `json:"isolation,omitempty"`
	LockWaitTimeout int    `json:"lock_wait_timeout,omitempty"`
}

// actorConn is one simulated client: its own proxy, its own dedicated MySQL
// connection, held open for the whole run so transaction state survives across
// steps.
type actorConn struct {
	id     string
	name   string
	accent string

	proxy *wire.Proxy
	db    *sql.DB
	conn  *sql.Conn
	// connID is CONNECTION_ID(), used to map performance_schema rows back here.
	connID uint64

	busy      bool
	inTrx     bool
	stepIndex int
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
	// lastLocks is the most recent snapshot, kept so a history record can carry
	// the lock landscape the run ended with.
	lastLocks *LockSnapshot

	// onState is called after every state change so the manager can refresh
	// this run's history record. It must not call back into the run's lock.
	onState func(*Run)

	// restoreDeadlockDetect records whether we changed the global so teardown
	// can put it back for other runs sharing the container.
	restoreDeadlockDetect bool
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
	return fmt.Sprintf("%s is still waiting on step %d — a connection can only run one statement at a time. Advance another actor, or wait for the lock to resolve.",
		e.ActorName, e.StepIndex)
}

// Prepare builds a run: creates a scratch database, applies schema and seed
// data, then opens one proxied connection per actor.
func Prepare(ctx context.Context, id string, c *casedef.Case, pool *mysqlbox.Pool, settle time.Duration) (*Run, error) {
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
		startedAt:    time.Now(),
		database:     "dl_" + id,
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

	if err := r.setup(ctx, pool); err != nil {
		r.mu.Lock()
		r.status = StatusFailed
		r.runErr = err.Error()
		r.mu.Unlock()
		r.logf("error", "setup failed: %v", err)
		r.publishState()
		// Release whatever did get created.
		r.Close()
		return r, err
	}

	r.mu.Lock()
	r.status = StatusReady
	r.message = "ready — step through the scenario"
	r.mu.Unlock()
	r.publishState()
	return r, nil
}

func (r *Run) setup(ctx context.Context, pool *mysqlbox.Pool) error {
	r.logf("info", "acquiring MySQL container (%s)", r.Case.MySQL.Image)
	box, err := pool.Get(ctx, r.Case.MySQL.Image, func(s string) { r.logf("info", "%s", s) })
	if err != nil {
		return err
	}
	r.box = box

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

	if dd := r.Case.MySQL.DeadlockDetect; dd != nil {
		val := "ON"
		if !*dd {
			val = "OFF"
		}
		if _, err := admin.ExecContext(ctx, "SET GLOBAL innodb_deadlock_detect = "+val); err != nil {
			return fmt.Errorf("set innodb_deadlock_detect: %w", err)
		}
		r.restoreDeadlockDetect = true
		r.logf("warn", "innodb_deadlock_detect set to %s globally for this run", val)
	}

	for _, a := range r.Case.Actors {
		if err := r.openActor(ctx, a); err != nil {
			return fmt.Errorf("actor %s: %w", a.ID, err)
		}
	}
	return nil
}

func (r *Run) openActor(ctx context.Context, a casedef.Actor) error {
	ac := &actorConn{id: a.ID, name: a.Name, accent: a.Accent}

	proxy, err := wire.Listen(r.box.Addr(), a.ID, func(ev wire.Event) {
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

	r.mu.Lock()
	r.actors[a.ID] = ac
	r.actorOrder = append(r.actorOrder, a.ID)
	r.mu.Unlock()

	r.logf("info", "%s connected as MySQL connection %d via proxy %s", a.Name, ac.connID, proxy.Addr())
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

// onWire tags a decoded packet with the step that was in flight for that actor.
func (r *Run) onWire(ev wire.Event) {
	r.mu.Lock()
	idx := 0
	if a, ok := r.actors[ev.Actor]; ok {
		idx = a.stepIndex
	}
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return
	}
	r.Bus.Publish(Event{Type: EventWire, At: ev.At, RunID: r.ID, Wire: &WireEvent{Event: ev, StepIndex: idx}})
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
	}
	if r.box != nil {
		s.Addr = r.box.Addr()
	}
	for _, id := range r.actorOrder {
		a := r.actors[id]
		s.Actors = append(s.Actors, ActorState{
			ID: a.id, Name: a.name, Accent: a.accent,
			ConnID: a.connID, Busy: a.busy, InTrx: a.inTrx,
			StepIndex: a.stepIndex,
			ProxyAddr: proxyAddr(a),
		})
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
	start := time.Now()
	res := runStatement(r.execCtx, a.conn, sqlText, caseStep.Args)
	end := time.Now()

	r.mu.Lock()
	st.EndedAt = &end
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
	byConn := make(map[uint64]string, len(r.actors))
	for _, a := range r.actors {
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
	actors := make([]*actorConn, 0, len(r.actors))
	for _, id := range r.actorOrder {
		actors = append(actors, r.actors[id])
	}
	box, setupDB, database := r.box, r.setupDB, r.database
	restoreDD := r.restoreDeadlockDetect
	r.status = StatusClosed
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

	var firstErr error
	if box != nil {
		if restoreDD {
			if _, err := box.Admin().ExecContext(ctx, "SET GLOBAL innodb_deadlock_detect = ON"); err != nil {
				firstErr = err
			}
		}
		if database != "" {
			if _, err := box.Admin().ExecContext(ctx, "DROP DATABASE IF EXISTS `"+database+"`"); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}

	r.Bus.Publish(Event{Type: EventLog, RunID: r.ID, Level: "info", Message: "run closed and scratch database dropped"})
	r.Bus.Close()
	return firstErr
}
