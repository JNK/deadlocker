package engine

import (
	"context"
	"crypto/rand"
	"fmt"
	"sync"
	"time"

	"github.com/jnk/deadlocker/internal/casedef"
	"github.com/jnk/deadlocker/internal/dockerctl"
	"github.com/jnk/deadlocker/internal/mysqlbox"
)

// maxRuns caps how many scenarios stay resident. Each run holds open
// connections and a scratch database, so the oldest is retired when the cap is
// reached.
const maxRuns = 10

// Manager owns the live runs, routes container logs to them, and keeps the
// history of finished runs.
type Manager struct {
	pool    *mysqlbox.Pool
	settle  time.Duration
	history *History

	mu    sync.RWMutex
	runs  map[string]*Run
	order []string
}

func NewManager(pool *mysqlbox.Pool, settle time.Duration) *Manager {
	if settle <= 0 {
		settle = 400 * time.Millisecond
	}
	return &Manager{
		pool:    pool,
		settle:  settle,
		history: NewHistory(),
		runs:    map[string]*Run{},
	}
}

// History exposes the run log.
func (m *Manager) History() *History { return m.history }

// SettleWindow is how long a statement may run before it is called blocked.
func (m *Manager) SettleWindow() time.Duration { return m.settle }

// Start prepares a new run for the case.
func (m *Manager) Start(ctx context.Context, c *casedef.Case) (*Run, error) {
	m.evictIfNeeded()

	id, err := newRunID()
	if err != nil {
		return nil, err
	}
	run, err := Prepare(ctx, id, c, m.pool, m.settle)
	if run != nil {
		// Keep this run's history record current as it progresses, so a run
		// still in flight shows up in the scenario's history and can be
		// compared without being closed first.
		run.mu.Lock()
		run.onState = func(r *Run) { m.history.Put(snapshotRecord(r)) }
		run.mu.Unlock()
		m.history.Put(snapshotRecord(run))
	}
	if err != nil {
		// Keep the failed run addressable so the UI can show why it failed.
		if run != nil {
			m.mu.Lock()
			m.runs[id] = run
			m.order = append(m.order, id)
			m.mu.Unlock()
		}
		return run, err
	}

	m.mu.Lock()
	m.runs[id] = run
	m.order = append(m.order, id)
	m.mu.Unlock()
	return run, nil
}

func (m *Manager) evictIfNeeded() {
	m.mu.Lock()
	var victim *Run
	if len(m.order) >= maxRuns {
		oldest := m.order[0]
		victim = m.runs[oldest]
		delete(m.runs, oldest)
		m.order = m.order[1:]
	}
	m.mu.Unlock()
	if victim != nil {
		m.history.Put(snapshotRecord(victim))
		_ = victim.Close()
	}
}

func (m *Manager) Get(id string) (*Run, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.runs[id]
	return r, ok
}

// List returns the live runs, newest first.
func (m *Manager) List() []*Run {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Run, 0, len(m.order))
	for i := len(m.order) - 1; i >= 0; i-- {
		if r, ok := m.runs[m.order[i]]; ok {
			out = append(out, r)
		}
	}
	return out
}

// CloseRun tears down and forgets a run.
func (m *Manager) CloseRun(id string) error {
	m.mu.Lock()
	r, ok := m.runs[id]
	if ok {
		delete(m.runs, id)
		for i, v := range m.order {
			if v == id {
				m.order = append(m.order[:i], m.order[i+1:]...)
				break
			}
		}
	}
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown run %q", id)
	}
	// Record before closing: teardown resets the run's status and drops its
	// connections, and the history entry should reflect where the run actually
	// got to.
	m.history.Put(snapshotRecord(r))
	return r.Close()
}

// OnDockerLog fans a container log line out to every run using that image.
// mysqld writes deadlock reports to the error log, so this is where the InnoDB
// deadlock narrative reaches the UI.
func (m *Manager) OnDockerLog(image string, line dockerctl.LogLine) {
	m.mu.RLock()
	runs := make([]*Run, 0, len(m.runs))
	for _, r := range m.runs {
		if r.Case.MySQL.Image == image {
			runs = append(runs, r)
		}
	}
	m.mu.RUnlock()

	for _, r := range runs {
		r.AppendDockerLog(line.Stream, line.Time, line.Text)
	}
}

// CloseAll tears down every run.
func (m *Manager) CloseAll() {
	m.mu.Lock()
	runs := make([]*Run, 0, len(m.runs))
	for _, r := range m.runs {
		runs = append(runs, r)
	}
	m.runs = map[string]*Run{}
	m.order = nil
	m.mu.Unlock()

	for _, r := range runs {
		m.history.Put(snapshotRecord(r))
		_ = r.Close()
	}
}

// newRunID returns a short lowercase alphanumeric id. It doubles as the suffix
// of the scratch database name, so it must be a legal MySQL identifier.
func newRunID() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, len(buf))
	for i, b := range buf {
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	// Lead with a letter so the identifier never looks like a number.
	out[0] = alphabet[int(buf[0])%26]
	return string(out), nil
}
