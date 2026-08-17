package engine

import (
	"sort"
	"sync"
	"time"
)

// historyLimitRuns caps how many past runs are retained. Records hold step
// results and one lock snapshot -- not the wire stream, which is far too large
// to keep around -- so this stays small.
const historyLimitRuns = 200

// Record is a finished run, trimmed to what is worth keeping: everything the
// case page and the comparison view need, and nothing that grows without bound.
type Record struct {
	RunID           string `json:"run_id"`
	CaseID          string `json:"case_id"`
	CaseName        string `json:"case_name"`
	Image           string `json:"image"`
	Isolation       string `json:"isolation,omitempty"`
	LockWaitTimeout int    `json:"lock_wait_timeout,omitempty"`
	Prepared        bool   `json:"prepared,omitempty"`

	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	Status    string    `json:"status"`

	Steps          []*StepResult `json:"steps"`
	FinalLocks     *LockSnapshot `json:"final_locks,omitempty"`
	DeadlockReport string        `json:"deadlock_report,omitempty"`

	// Outcome is the one-word summary, computed when the record is written so
	// it is present in the JSON API as well as in templates.
	Outcome string `json:"outcome"`
	// Ephemeral marks a run of an unsaved draft, which no saved scenario owns.
	Ephemeral bool `json:"ephemeral,omitempty"`

	// Counters, precomputed so listings do not have to walk the steps.
	Submitted  int `json:"submitted"`
	Matches    int `json:"matches"`
	Mismatches int `json:"mismatches"`
	Blocked    int `json:"blocked"`
	Deadlocks  int `json:"deadlocks"`
	Timeouts   int `json:"timeouts"`
	Errors     int `json:"errors"`
}

// Duration is how long the run was open.
func (r *Record) Duration() time.Duration {
	if r.EndedAt.IsZero() {
		return time.Since(r.StartedAt)
	}
	return r.EndedAt.Sub(r.StartedAt)
}

// outcomeOf summarises a run in one word for the history list.
func outcomeOf(r *Record) string {
	switch {
	case r.Mismatches > 0:
		return "mismatch"
	case r.Deadlocks > 0:
		return "deadlock"
	case r.Timeouts > 0:
		return "timeout"
	case r.Errors > 0:
		return "error"
	case r.Submitted == 0:
		return "not started"
	default:
		return "clean"
	}
}

// snapshotRecord builds a Record from a run's current state.
func snapshotRecord(r *Run) *Record {
	state := r.State()
	steps := r.Steps()

	rec := &Record{
		RunID:           state.ID,
		CaseID:          state.CaseID,
		CaseName:        state.CaseName,
		Image:           state.Image,
		Isolation:       state.Isolation,
		LockWaitTimeout: state.LockWaitTimeout,
		Prepared:        r.Case.MySQL.Prepared,
		StartedAt:       state.Started,
		EndedAt:         time.Now(),
		Ephemeral:       state.Ephemeral,
		Status:          state.Status,
		Steps:           steps,
		DeadlockReport:  state.DeadlockReport,
	}
	for _, s := range steps {
		if s.Status != StepPending {
			rec.Submitted++
		}
		switch s.Verdict {
		case "match":
			rec.Matches++
		case "mismatch":
			rec.Mismatches++
		}
		if s.WasBlocked || s.Status == StepBlocked {
			rec.Blocked++
		}
		if s.Error != nil {
			switch s.Error.Kind {
			case ErrKindDeadlock:
				rec.Deadlocks++
			case ErrKindTimeout:
				rec.Timeouts++
			default:
				rec.Errors++
			}
		}
	}

	r.mu.Lock()
	if r.lastLocks != nil {
		snap := *r.lastLocks
		rec.FinalLocks = &snap
	}
	r.mu.Unlock()

	rec.Outcome = outcomeOf(rec)
	return rec
}

// History retains finished runs so they can be listed per scenario and
// compared against each other after the run itself is gone.
//
// It is in-memory only: this is a local exploration tool, and a run's value is
// in the session you are in the middle of. Restarting the server starts a fresh
// log.
type History struct {
	mu      sync.RWMutex
	records []*Record // oldest first
	byID    map[string]*Record
}

func NewHistory() *History {
	return &History{byID: map[string]*Record{}}
}

// Put inserts or replaces a record, keyed by run id.
func (h *History) Put(rec *Record) {
	if rec == nil || rec.RunID == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	if existing, ok := h.byID[rec.RunID]; ok {
		*existing = *rec
		return
	}
	h.records = append(h.records, rec)
	h.byID[rec.RunID] = rec
	if len(h.records) > historyLimitRuns {
		drop := h.records[0]
		h.records = h.records[1:]
		delete(h.byID, drop.RunID)
	}
}

// Get returns one record.
func (h *History) Get(runID string) (*Record, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	rec, ok := h.byID[runID]
	return rec, ok
}

// ForCase returns the records for one scenario, newest first.
func (h *History) ForCase(caseID string) []*Record {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var out []*Record
	for _, rec := range h.records {
		// A draft run can carry the same id as a saved scenario without being
		// that scenario, so it must never appear in its history.
		if rec.CaseID == caseID && !rec.Ephemeral {
			out = append(out, rec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out
}

// All returns every record, newest first.
func (h *History) All() []*Record {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]*Record, len(h.records))
	copy(out, h.records)
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out
}
