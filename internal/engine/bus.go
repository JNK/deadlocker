package engine

import (
	"sync"
	"time"

	"github.com/jnk/deadlocker/internal/wire"
)

// Event types published on a run's bus.
const (
	EventState  = "state"  // run lifecycle / status change
	EventStep   = "step"   // a step changed status
	EventWire   = "wire"   // a decoded MySQL packet
	EventLocks  = "locks"  // a lock snapshot
	EventDocker = "docker" // a container log line
	EventLog    = "log"    // a message from the tool itself
)

// WireEvent is a decoded packet tagged with the step that was in flight when
// it crossed the wire, so the UI can show per-step packet traces.
type WireEvent struct {
	wire.Event
	StepIndex int `json:"step_index"`
}

// DockerLine is a container log line.
type DockerLine struct {
	Stream string    `json:"stream"`
	At     time.Time `json:"at"`
	Text   string    `json:"text"`
	// Deadlock marks lines that belong to an InnoDB deadlock report, which the
	// UI highlights.
	Deadlock bool `json:"deadlock,omitempty"`
}

// Event is one item in a run's timeline.
type Event struct {
	Seq   int       `json:"seq"`
	Type  string    `json:"type"`
	At    time.Time `json:"at"`
	RunID string    `json:"run_id"`

	State  *RunState     `json:"state,omitempty"`
	Step   *StepResult   `json:"step,omitempty"`
	Wire   *WireEvent    `json:"wire,omitempty"`
	Locks  *LockSnapshot `json:"locks,omitempty"`
	Docker *DockerLine   `json:"docker,omitempty"`

	Message string `json:"message,omitempty"`
	Level   string `json:"level,omitempty"` // info | warn | error
}

// historyLimit caps the retained event log. Wire capture is chatty; a few
// thousand events is far more than any hand-driven scenario produces, and the
// cap only matters for a runaway loop.
const historyLimit = 20000

// Bus fans events out to live subscribers and retains history so a page reload
// or a late SSE connection can replay the run from the beginning.
type Bus struct {
	mu      sync.RWMutex
	seq     int
	history []Event
	subs    map[int]chan Event
	nextSub int
	closed  bool
}

func NewBus() *Bus {
	return &Bus{subs: map[int]chan Event{}}
}

// Publish stamps and broadcasts an event. It never blocks: a subscriber that
// cannot keep up loses events rather than stalling the engine, and can recover
// the full sequence from History.
func (b *Bus) Publish(ev Event) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.seq++
	ev.Seq = b.seq
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	b.history = append(b.history, ev)
	if len(b.history) > historyLimit {
		// Drop the oldest quarter at once so this is amortised, not per-event.
		drop := historyLimit / 4
		b.history = append([]Event(nil), b.history[drop:]...)
	}
	subs := make([]chan Event, 0, len(b.subs))
	for _, ch := range b.subs {
		subs = append(subs, ch)
	}
	b.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// Subscribe returns a channel of future events plus the history so far. The
// returned cancel function must be called to release the subscription.
func (b *Bus) Subscribe(sinceSeq int) (<-chan Event, []Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()

	var backlog []Event
	for _, ev := range b.history {
		if ev.Seq > sinceSeq {
			backlog = append(backlog, ev)
		}
	}
	if b.closed {
		ch := make(chan Event)
		close(ch)
		return ch, backlog, func() {}
	}

	id := b.nextSub
	b.nextSub++
	ch := make(chan Event, 512)
	b.subs[id] = ch

	return ch, backlog, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if sub, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(sub)
		}
	}
}

// History returns every retained event after sinceSeq.
func (b *Bus) History(sinceSeq int) []Event {
	b.mu.RLock()
	defer b.mu.RUnlock()
	var out []Event
	for _, ev := range b.history {
		if ev.Seq > sinceSeq {
			out = append(out, ev)
		}
	}
	return out
}

// Close releases all subscribers.
func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for id, ch := range b.subs {
		delete(b.subs, id)
		close(ch)
	}
}
