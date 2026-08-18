package engine

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// The SQL console.
//
// A scenario is a fixed sequence, and the question that a fixed sequence
// provokes is always "what if". What if I select that row now? What does this
// session see from inside its transaction? What happens if a third connection
// tries to insert here?
//
// Answering that meant editing the scenario and running it again from the top,
// which loses the state that made the question interesting. So statements can
// be typed at a run directly, in one of two places:
//
//   - on an actor's connection, where the statement lands inside that actor's
//     open transaction and takes locks as that session;
//   - on a standalone connection, opened on demand, which sees only what a
//     separate session would see.
//
// The difference between those two is most of what isolation means, so the
// console offers both rather than picking one.

// ConsoleEntry is one statement submitted from the console, with the same
// observability a step gets: a plan, a settle window, and a verdict on whether
// it is waiting for a lock.
type ConsoleEntry struct {
	ID         int    `json:"id"`
	Session    string `json:"session"`
	Name       string `json:"name"`
	Accent     string `json:"accent"`
	Standalone bool   `json:"standalone"`
	SQL        string `json:"sql"`

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
	WasBlocked  bool     `json:"was_blocked,omitempty"`

	Plan []PlanRow `json:"plan,omitempty"`
}

func (e *ConsoleEntry) clone() *ConsoleEntry {
	c := *e
	c.Columns = append([]string(nil), e.Columns...)
	c.BlockedBy = append([]string(nil), e.BlockedBy...)
	if e.Rows != nil {
		c.Rows = make([][]string, len(e.Rows))
		for i, row := range e.Rows {
			c.Rows[i] = append([]string(nil), row...)
		}
	}
	if e.Error != nil {
		err := *e.Error
		c.Error = &err
	}
	return &c
}

// consoleAccents colour standalone sessions, from the same five the UI knows.
// They start at the end of the palette because scenarios almost always name
// their actors from the front of it, so a console session usually looks
// different from every actor without needing to negotiate.
var consoleAccents = []string{"violet", "rose", "teal", "amber", "blue"}

// OpenSession opens a standalone connection against the run's scratch database.
// It is configured exactly like an actor's, and shows up in the lock tables the
// same way.
func (r *Run) OpenSession(ctx context.Context) (ActorState, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ActorState{}, fmt.Errorf("run has been closed")
	}
	if r.box == nil {
		r.mu.Unlock()
		return ActorState{}, fmt.Errorf("the run is still starting up")
	}
	if len(r.consoles) >= maxConsoleSessions {
		r.mu.Unlock()
		return ActorState{}, fmt.Errorf("this run already has %d console sessions open; close one first", maxConsoleSessions)
	}
	r.consoleSeq++
	n := r.consoleSeq
	ac := &actorConn{
		id:         fmt.Sprintf("console%d", n),
		name:       fmt.Sprintf("Console %d", n),
		accent:     consoleAccents[(n-1)%len(consoleAccents)],
		standalone: true,
	}
	r.mu.Unlock()

	if err := r.dial(ctx, ac); err != nil {
		return ActorState{}, err
	}

	r.mu.Lock()
	r.consoles[ac.id] = ac
	r.consoleOrder = append(r.consoleOrder, ac.id)
	st := ac.state()
	r.mu.Unlock()

	r.logf("info", "%s connected as MySQL connection %d — a standalone session, outside the scenario", ac.name, ac.connID)
	r.publishState()
	return st, nil
}

// maxConsoleSessions caps standalone connections per run. Each one holds a
// proxy, a connection and possibly an open transaction; a handful is far more
// than an experiment needs, and the cap is what stops a stuck loop from opening
// hundreds.
const maxConsoleSessions = 8

// CloseSession drops a standalone connection, rolling back anything it still
// held open.
func (r *Run) CloseSession(id string) error {
	r.mu.Lock()
	ac, ok := r.consoles[id]
	if !ok {
		r.mu.Unlock()
		return fmt.Errorf("unknown console session %q", id)
	}
	delete(r.consoles, id)
	for i, v := range r.consoleOrder {
		if v == id {
			r.consoleOrder = append(r.consoleOrder[:i], r.consoleOrder[i+1:]...)
			break
		}
	}
	box := r.box
	r.mu.Unlock()

	// Kill first: a session parked on a lock wait does not answer a polite close
	// until its statement returns.
	if box != nil && ac.connID != 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = box.Admin().ExecContext(ctx, fmt.Sprintf("KILL CONNECTION %d", ac.connID))
	}
	if ac.conn != nil {
		_ = ac.conn.Close()
	}
	if ac.db != nil {
		_ = ac.db.Close()
	}
	if ac.proxy != nil {
		_ = ac.proxy.Close()
	}

	r.logf("info", "%s disconnected; any transaction it held was rolled back", ac.name)
	r.publishState()
	r.publishLocks(r.snapshot(context.Background()))
	return nil
}

// Sessions lists everything a console statement can be sent to: the scenario's
// actors first, then the standalone connections.
func (r *Run) Sessions() []ActorState {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ActorState, 0, len(r.actorOrder)+len(r.consoleOrder))
	for _, id := range r.actorOrder {
		out = append(out, r.actors[id].state())
	}
	for _, id := range r.consoleOrder {
		out = append(out, r.consoles[id].state())
	}
	return out
}

// Console runs one statement on the named session.
//
// It behaves exactly like a step, because the interesting outcomes are the
// same ones: it returns as soon as the statement finishes or has waited long
// enough to be called blocked, and a blocked statement keeps running in the
// background until it succeeds, times out, or is picked as a deadlock victim.
func (r *Run) Console(ctx context.Context, sessionID, sqlText string) (*ConsoleEntry, error) {
	sqlText = trimStatement(sqlText)
	if sqlText == "" {
		return nil, fmt.Errorf("nothing to run")
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, fmt.Errorf("run has been closed")
	}
	if r.status == StatusPreparing {
		r.mu.Unlock()
		return nil, fmt.Errorf("the run is still starting up")
	}
	ac := r.sessionLocked(sessionID)
	if ac == nil {
		r.mu.Unlock()
		return nil, fmt.Errorf("unknown session %q", sessionID)
	}
	if ac.conn == nil {
		r.mu.Unlock()
		return nil, fmt.Errorf("%s is not connected", ac.name)
	}
	if ac.busy {
		blockedOn, name := ac.stepIndex, ac.name
		r.mu.Unlock()
		return nil, &ActorBlockedError{Actor: sessionID, ActorName: name, StepIndex: blockedOn}
	}

	r.entrySeq++
	now := time.Now()
	entry := &ConsoleEntry{
		ID:          r.entrySeq,
		Session:     ac.id,
		Name:        ac.name,
		Accent:      ac.accent,
		Standalone:  ac.standalone,
		SQL:         sqlText,
		Status:      StepRunning,
		SubmittedAt: &now,
	}
	ac.busy = true
	ac.consoleID = entry.ID
	out := entry.clone()
	r.mu.Unlock()

	r.publishConsole(out)
	r.publishState()

	done := make(chan struct{})
	go r.executeConsole(ac, entry, done)

	select {
	case <-done:
	case <-time.After(r.settleWindow):
	case <-ctx.Done():
	}

	snap := r.snapshot(context.Background())

	r.mu.Lock()
	if entry.Status == StepRunning {
		entry.Status = StepBlocked
		entry.WasBlocked = true
		entry.BlockedBy = snap.BlockedBy(entry.Session)
		if w := snap.WaitFor(entry.Session); w != nil {
			entry.WaitExplain = describeWait(w)
		}
	}
	out = entry.clone()
	r.mu.Unlock()

	r.publishLocks(snap)
	r.publishConsole(out)
	r.publishState()
	return out, nil
}

func (r *Run) executeConsole(ac *actorConn, entry *ConsoleEntry, done chan struct{}) {
	plan := r.explainStep(entry.SQL, nil)

	start := time.Now()
	res := runStatement(r.execCtx, ac.conn, entry.SQL, nil)
	end := time.Now()

	r.mu.Lock()
	entry.EndedAt = &end
	entry.Plan = plan
	entry.DurationMS = end.Sub(start).Milliseconds()
	entry.Columns = res.columns
	entry.Rows = res.rows
	entry.RowCount = res.rowCount
	entry.RowsTruncated = res.truncated
	entry.RowsAffected = res.affected
	entry.LastInsertID = res.lastInsertID
	if res.err != nil {
		entry.Status = StepFailed
		entry.Error = res.err
	} else {
		entry.Status = StepDone
	}
	ac.busy = false
	ac.consoleID = 0
	ac.inTrx = trackTransaction(ac.inTrx, entry.SQL, res.err)
	out := entry.clone()
	isDeadlock := res.err != nil && res.err.Kind == ErrKindDeadlock
	r.mu.Unlock()

	close(done)
	r.publishConsole(out)

	if isDeadlock {
		r.captureDeadlockReport()
	}
	r.publishLocks(r.snapshot(context.Background()))
	r.publishState()
}

func (r *Run) publishConsole(e *ConsoleEntry) {
	r.Bus.Publish(Event{Type: EventConsole, RunID: r.ID, Console: e})
}

// trimStatement tidies typed SQL: a trailing semicolon is how everyone ends a
// statement everywhere else, and the driver rejects it because multi-statement
// support is deliberately off.
func trimStatement(s string) string {
	s = strings.TrimSpace(s)
	for strings.HasSuffix(s, ";") {
		s = strings.TrimSpace(strings.TrimSuffix(s, ";"))
	}
	return s
}
