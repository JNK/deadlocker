package agentapi

import (
	"context"
	"sync"
	"time"
)

// Activity kinds.
const (
	KindScenarioCreated = "scenario.created"
	KindScenarioUpdated = "scenario.updated"
	KindRunStarted      = "run.started"
	KindRunStepped      = "run.stepped"
	KindRunClosed       = "run.closed"
	KindConfigSaved     = "config.saved"
	KindToolCalled      = "tool.called"
)

// Sources an operation can come from. The UI shows this so it is always clear
// whether a change was made by hand, by an MCP client, or by the built-in chat.
const (
	SourceUI   = "ui"
	SourceMCP  = "mcp"
	SourceChat = "chat"
)

type sourceKey struct{}

// WithSource tags a context so operations performed under it are attributed
// correctly in the activity feed.
func WithSource(ctx context.Context, source string) context.Context {
	return context.WithValue(ctx, sourceKey{}, source)
}

// SourceOf returns the attributed source, defaulting to the UI.
func SourceOf(ctx context.Context) string {
	if s, ok := ctx.Value(sourceKey{}).(string); ok && s != "" {
		return s
	}
	return SourceUI
}

// Activity is one thing that happened, for the live feed.
type Activity struct {
	Seq        int       `json:"seq"`
	At         time.Time `json:"at"`
	Source     string    `json:"source"`
	Kind       string    `json:"kind"`
	Tool       string    `json:"tool,omitempty"`
	Summary    string    `json:"summary"`
	ScenarioID string    `json:"scenario_id,omitempty"`
	RunID      string    `json:"run_id,omitempty"`
	// Detail carries anything the UI wants to act on, such as the new YAML of
	// a scenario draft.
	Detail map[string]any `json:"detail,omitempty"`
}

const activityHistory = 500

// Hub broadcasts activity to every connected UI, so a scenario written by an
// MCP client or a run stepped by the chat shows up immediately in an open
// browser tab.
type Hub struct {
	mu      sync.RWMutex
	seq     int
	history []Activity
	subs    map[int]chan Activity
	nextSub int
}

func NewHub() *Hub { return &Hub{subs: map[int]chan Activity{}} }

// Publish stamps and fans out an activity. It never blocks on a slow consumer.
func (h *Hub) Publish(a Activity) Activity {
	h.mu.Lock()
	h.seq++
	a.Seq = h.seq
	if a.At.IsZero() {
		a.At = time.Now()
	}
	h.history = append(h.history, a)
	if len(h.history) > activityHistory {
		h.history = append([]Activity(nil), h.history[len(h.history)-activityHistory:]...)
	}
	subs := make([]chan Activity, 0, len(h.subs))
	for _, ch := range h.subs {
		subs = append(subs, ch)
	}
	h.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- a:
		default:
		}
	}
	return a
}

// Subscribe returns a channel of future activity plus anything after sinceSeq.
func (h *Hub) Subscribe(sinceSeq int) (<-chan Activity, []Activity, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()

	var backlog []Activity
	for _, a := range h.history {
		if a.Seq > sinceSeq {
			backlog = append(backlog, a)
		}
	}

	id := h.nextSub
	h.nextSub++
	ch := make(chan Activity, 128)
	h.subs[id] = ch

	return ch, backlog, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if sub, ok := h.subs[id]; ok {
			delete(h.subs, id)
			close(sub)
		}
	}
}
