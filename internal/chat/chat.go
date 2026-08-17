// Package chat is the built-in assistant.
//
// It drives the same operations the MCP server exposes, through
// charm.land/fantasy against any OpenAI-compatible endpoint — Ollama, LM
// Studio, llama.cpp, vLLM. The tools are adapters over internal/agentapi, so
// the assistant and an external MCP client can do exactly the same things.
package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"

	"github.com/jnk/deadlocker/internal/agentapi"
	"github.com/jnk/deadlocker/internal/store"
)

// Mode selects the system prompt and the tool set.
type Mode string

const (
	// ModeDiscuss reasons about an existing scenario and can run it.
	ModeDiscuss Mode = "discuss"
	// ModeBuild authors a new scenario, maintaining a draft the UI shows.
	ModeBuild Mode = "build"
)

// Event is one thing the browser should render while a turn is in flight.
//
// The stream is deliberately fine-grained: the UI shows reasoning as it
// arrives, names what each tool is doing while it runs, and starts a fresh
// message bubble whenever a tool call or a reasoning block interrupts the
// assistant's prose.
type Event struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`

	// Tool activity. ID correlates a call with its result.
	ID      string `json:"id,omitempty"`
	Tool    string `json:"tool,omitempty"`
	Label   string `json:"label,omitempty"`  // human phrasing of what the tool is doing
	Detail  string `json:"detail,omitempty"` // the salient argument, e.g. the run id
	Input   string `json:"input,omitempty"`
	Result  string `json:"result,omitempty"`
	Summary string `json:"summary,omitempty"` // one line describing the outcome
	Failed  bool   `json:"failed,omitempty"`

	YAML  string `json:"yaml,omitempty"`
	RunID string `json:"run_id,omitempty"`
	// Scenario is the parsed shape of the draft, so the side pane can render
	// steps without a YAML parser in the browser.
	Scenario *DraftView `json:"scenario,omitempty"`

	Message string `json:"message,omitempty"`
}

// Event types.
const (
	EvDelta          = "delta"
	EvReasoningStart = "reasoning_start"
	EvReasoningDelta = "reasoning_delta"
	EvReasoningEnd   = "reasoning_end"
	EvToolInput      = "tool_input"
	EvToolCall       = "tool_call"
	EvToolResult     = "tool_result"
	EvDraft          = "draft"
	EvRun            = "run"
	EvSaved          = "saved"
	EvDone           = "done"
	EvError          = "error"
)

// DraftView is the parsed scenario behind the draft, for the steps pane.
type DraftView struct {
	Name   string       `json:"name"`
	Actors []DraftActor `json:"actors"`
	Steps  []DraftStep  `json:"steps"`
	Valid  bool         `json:"valid"`
	Error  string       `json:"error,omitempty"`
	Notes  []string     `json:"warnings,omitempty"`
}

type DraftActor struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Accent string `json:"accent"`
}

type DraftStep struct {
	Index  int    `json:"index"`
	Actor  string `json:"actor"`
	Accent string `json:"accent"`
	Label  string `json:"label"`
	SQL    string `json:"sql"`
	Note   string `json:"note,omitempty"`
	Expect string `json:"expect,omitempty"`
}

// Emit sends an event to the browser.
type Emit func(Event)

// Turn is one exchange stored for the transcript.
type Turn struct {
	Role string    `json:"role"`
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

// Session holds one conversation.
type Session struct {
	ID         string `json:"id"`
	Mode       Mode   `json:"mode"`
	ScenarioID string `json:"scenario_id,omitempty"`
	RunID      string `json:"run_id,omitempty"`
	// Draft is the scenario being authored in build mode.
	Draft      string    `json:"draft,omitempty"`
	Transcript []Turn    `json:"transcript"`
	CreatedAt  time.Time `json:"created_at"`

	mu      sync.Mutex
	history []fantasy.Message
	busy    bool
}

// Service owns sessions and builds agents on demand.
type Service struct {
	api   *agentapi.API
	store *store.Store

	mu       sync.RWMutex
	sessions map[string]*Session
}

func NewService(api *agentapi.API, st *store.Store) *Service {
	return &Service{api: api, store: st, sessions: map[string]*Session{}}
}

// Config returns the current LLM configuration.
func (s *Service) Config() (store.Config, error) {
	cfg, _, err := s.store.Current()
	return cfg, err
}

// NewSession starts a conversation.
func (s *Service) NewSession(id string, mode Mode, scenarioID, runID, draft string) *Session {
	sess := &Session{
		ID: id, Mode: mode, ScenarioID: scenarioID, RunID: runID,
		Draft: draft, CreatedAt: time.Now(),
	}
	s.mu.Lock()
	s.sessions[id] = sess
	// Keep a lid on memory; conversations are cheap to restart.
	if len(s.sessions) > 50 {
		oldest := ""
		var oldestAt time.Time
		for k, v := range s.sessions {
			if oldest == "" || v.CreatedAt.Before(oldestAt) {
				oldest, oldestAt = k, v.CreatedAt
			}
		}
		delete(s.sessions, oldest)
	}
	s.mu.Unlock()
	return sess
}

func (s *Service) Session(id string) (*Session, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[id]
	return sess, ok
}

// Draft returns the session's current scenario draft.
func (sess *Session) DraftYAML() string {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.Draft
}

// Send runs one turn, streaming events as they happen.
func (s *Service) Send(ctx context.Context, sess *Session, message string, emit Emit) error {
	cfg, err := s.Config()
	if err != nil {
		return err
	}
	if !cfg.Ready() {
		return fmt.Errorf("the assistant is not configured yet: set a base URL and pick a model in Settings")
	}

	sess.mu.Lock()
	if sess.busy {
		sess.mu.Unlock()
		return fmt.Errorf("this conversation is already generating a reply")
	}
	sess.busy = true
	history := append([]fantasy.Message(nil), sess.history...)
	sess.Transcript = append(sess.Transcript, Turn{Role: "user", Text: message, At: time.Now()})
	sess.mu.Unlock()

	defer func() {
		sess.mu.Lock()
		sess.busy = false
		sess.mu.Unlock()
	}()

	agent, err := s.buildAgent(ctx, cfg, sess, emit)
	if err != nil {
		return err
	}

	var reply strings.Builder
	call := fantasy.AgentStreamCall{
		Prompt:   message,
		Messages: history,
		OnTextDelta: func(_, text string) error {
			reply.WriteString(text)
			emit(Event{Type: EvDelta, Text: text})
			return nil
		},

		// Reasoning is streamed so the UI can show it live and then collapse it
		// to "thought for N seconds" once the block closes.
		OnReasoningStart: func(id string, _ fantasy.ReasoningContent) error {
			emit(Event{Type: EvReasoningStart, ID: id})
			return nil
		},
		OnReasoningDelta: func(id, text string) error {
			emit(Event{Type: EvReasoningDelta, ID: id, Text: text})
			return nil
		},
		OnReasoningEnd: func(id string, _ fantasy.ReasoningContent) error {
			emit(Event{Type: EvReasoningEnd, ID: id})
			return nil
		},

		// The tool name arrives before its arguments finish streaming, so the
		// UI can show what is about to happen instead of waiting for a large
		// argument blob (a 60-line YAML draft, say) to finish.
		OnToolInputStart: func(id, toolName string) error {
			label, _ := describeToolCall(toolName, "")
			emit(Event{Type: EvToolInput, ID: id, Tool: toolName, Label: label})
			return nil
		},

		OnToolCall: func(tc fantasy.ToolCallContent) error {
			label, detail := describeToolCall(tc.ToolName, tc.Input)
			emit(Event{
				Type: EvToolCall, ID: tc.ToolCallID, Tool: tc.ToolName,
				Label: label, Detail: detail, Input: truncate(tc.Input, 4000),
			})
			return nil
		},
		OnToolResult: func(tr fantasy.ToolResultContent) error {
			raw := toolResultText(tr)
			summary, failed := describeToolResult(tr.ToolName, raw)
			emit(Event{
				Type: EvToolResult, ID: tr.ToolCallID, Tool: tr.ToolName,
				Summary: summary, Failed: failed, Result: truncate(raw, 4000),
			})
			return nil
		},
		OnError: func(err error) {
			emit(Event{Type: EvError, Message: err.Error()})
		},
	}

	result, err := agent.Stream(ctx, call)
	if err != nil {
		return err
	}

	// Fantasy hands back the full message list per step; appending them keeps
	// tool calls and their results in the history so the next turn has context.
	sess.mu.Lock()
	sess.history = append(sess.history, fantasy.NewUserMessage(message))
	for _, step := range result.Steps {
		sess.history = append(sess.history, step.Messages...)
	}
	final := reply.String()
	if final == "" && result.Response.Content != nil {
		final = result.Response.Content.Text()
	}
	sess.Transcript = append(sess.Transcript, Turn{Role: "assistant", Text: final, At: time.Now()})
	sess.mu.Unlock()

	emit(Event{Type: EvDone, Text: final})
	return nil
}

func (s *Service) buildAgent(ctx context.Context, cfg store.Config, sess *Session, emit Emit) (fantasy.Agent, error) {
	opts := []openaicompat.Option{
		openaicompat.WithBaseURL(cfg.LLM.BaseURL),
		openaicompat.WithName("deadlocker-local"),
	}
	if cfg.LLM.APIKey != "" {
		opts = append(opts, openaicompat.WithAPIKey(cfg.LLM.APIKey))
	}
	provider, err := openaicompat.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("could not reach the model provider: %w", err)
	}
	model, err := provider.LanguageModel(ctx, cfg.LLM.Model)
	if err != nil {
		return nil, fmt.Errorf("could not load model %q: %w", cfg.LLM.Model, err)
	}

	// Sampling options are only sent when configured. A model that already
	// behaves out of the box should not have its defaults overridden by ours.
	opts2 := []fantasy.AgentOption{
		fantasy.WithSystemPrompt(s.systemPrompt(sess)),
		fantasy.WithTools(s.toolsFor(sess, emit)...),
		fantasy.WithStopConditions(fantasy.StepCountIs(cfg.LLM.Steps())),
	}
	if cfg.LLM.Temperature != nil {
		opts2 = append(opts2, fantasy.WithTemperature(*cfg.LLM.Temperature))
	}
	if cfg.LLM.MaxTokens != nil && *cfg.LLM.MaxTokens > 0 {
		opts2 = append(opts2, fantasy.WithMaxOutputTokens(int64(*cfg.LLM.MaxTokens)))
	}
	return fantasy.NewAgent(model, opts2...), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n… truncated"
}

// toolResultText extracts the payload a tool actually returned.
//
// Fantasy wraps it as {"type":"text","data":{"text":"…"}}, and the text inside
// is the JSON our tools produce. Without unwrapping, every result summary sees
// the envelope instead of the content and reports nothing useful.
func toolResultText(tr fantasy.ToolResultContent) string {
	if tr.Result == nil {
		return ""
	}
	b, err := json.Marshal(tr.Result)
	if err != nil {
		return ""
	}

	return unwrapEnvelope(string(b))
}

// unwrapEnvelope pulls the inner text out of fantasy's tool result envelope,
// returning the input unchanged when it is not wrapped.
func unwrapEnvelope(raw string) string {
	var envelope struct {
		Type string `json:"type"`
		Data struct {
			Text string `json:"text"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(raw), &envelope) == nil && envelope.Data.Text != "" {
		return envelope.Data.Text
	}
	return raw
}

// SetDraft replaces the working draft. The human editor and the assistant both
// write here, so they always share one document.
func (sess *Session) SetDraft(yaml string) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	sess.Draft = yaml
}

// SetScenarioID records which saved scenario this session now edits, so a
// subsequent save updates in place instead of creating a duplicate.
func (sess *Session) SetScenarioID(id string) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	sess.ScenarioID = id
}

// ------------------------------------------------------- tool descriptions

// describeToolCall turns a tool name and its raw JSON arguments into something
// worth reading while it runs. "step_run" alone tells you nothing; "Stepping
// the run · 3 steps" tells you what is happening.
func describeToolCall(name, input string) (label, detail string) {
	args := map[string]any{}
	_ = json.Unmarshal([]byte(input), &args)

	str := func(k string) string {
		if v, ok := args[k].(string); ok {
			return v
		}
		return ""
	}
	num := func(k string) string {
		if v, ok := args[k].(float64); ok {
			return strconv.Itoa(int(v))
		}
		return ""
	}

	switch name {
	case "list_scenarios":
		label = "Browsing the scenario library"
		if q := str("query"); q != "" {
			detail = "matching " + q
		} else if c := str("category"); c != "" {
			detail = "in " + c
		}
	case "get_scenario":
		label, detail = "Reading a scenario", str("id")
	case "validate_scenario":
		label = "Validating the YAML"
	case "create_scenario":
		label, detail = "Saving a new scenario", str("path")
	case "update_scenario":
		label, detail = "Updating a scenario", str("id")
	case "set_draft":
		label = "Updating the draft"
		if y := str("yaml"); y != "" {
			detail = strconv.Itoa(len(strings.Split(y, "\n"))) + " lines"
		}
	case "run_draft":
		label = "Starting a test run of the draft"
	case "save_draft":
		label = "Saving the draft to the library"
	case "start_run":
		label = "Starting a run"
		if id := str("scenario_id"); id != "" {
			detail = id
		} else if str("yaml") != "" {
			detail = "from ad-hoc YAML"
		}
	case "step_run":
		label, detail = "Stepping the run", str("run_id")
		if n := num("count"); n != "" && n != "1" {
			detail += " · " + n + " steps"
		}
	case "run_all":
		label, detail = "Running to the end", str("run_id")
	case "get_run":
		label, detail = "Checking the run", str("run_id")
	case "get_locks":
		label, detail = "Reading the current locks", str("run_id")
	case "close_run":
		label, detail = "Closing the run", str("run_id")
	case "list_history":
		label = "Looking at past runs"
	case "compare_runs":
		label = "Comparing two runs"
	default:
		label = name
	}
	return label, detail
}

// describeToolResult condenses a tool's JSON response into one line, so the UI
// can show an outcome rather than a blob.
func describeToolResult(name, raw string) (summary string, failed bool) {
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return "", false
	}
	// validate_scenario reports an invalid document through `error`, which is a
	// normal outcome rather than a tool failure, so it is handled below with
	// its own phrasing.
	if msg, ok := payload["error"].(string); ok && msg != "" && name != "validate_scenario" {
		return msg, true
	}

	num := func(k string) (int, bool) {
		v, ok := payload[k].(float64)
		return int(v), ok
	}
	arr := func(k string) ([]any, bool) {
		v, ok := payload[k].([]any)
		return v, ok
	}

	switch name {
	case "list_scenarios":
		if n, ok := num("total"); ok {
			return plural(n, "scenario", "scenarios"), false
		}
	case "validate_scenario":
		valid, _ := payload["valid"].(bool)
		if !valid {
			msg, _ := payload["error"].(string)
			return "invalid: " + msg, true
		}
		if w, ok := arr("warnings"); ok && len(w) > 0 {
			return "valid, " + plural(len(w), "warning", "warnings"), false
		}
		return "valid", false
	case "set_draft":
		if accepted, ok := payload["accepted"].(bool); ok && !accepted {
			msg, _ := payload["error"].(string)
			return "rejected: " + msg, true
		}
		return "draft updated", false
	case "create_scenario", "update_scenario", "save_draft":
		if path, ok := payload["path"].(string); ok {
			return "written to " + path, false
		}
	case "start_run", "run_draft":
		if run, ok := payload["run"].(map[string]any); ok {
			id, _ := run["run_id"].(string)
			total, _ := run["total"].(float64)
			return "run " + id + " ready · " + plural(int(total), "step", "steps"), false
		}
	case "step_run", "run_all":
		return summariseSteps(payload), false
	case "get_locks", "get_run":
		if locks, ok := payload["locks"].(map[string]any); ok {
			return summariseLocks(locks), false
		}
	case "close_run":
		return "run closed, scratch database dropped", false
	case "list_history":
		if runs, ok := arr("runs"); ok {
			return plural(len(runs), "past run", "past runs"), false
		}
	case "compare_runs":
		if n, ok := num("changed"); ok {
			return plural(n, "step differs", "steps differ"), false
		}
	}
	return "", false
}

func summariseSteps(payload map[string]any) string {
	outcomes, _ := payload["outcomes"].([]any)
	var blocked, failed int
	var last string
	for _, o := range outcomes {
		m, ok := o.(map[string]any)
		if !ok {
			continue
		}
		step, _ := m["step"].(map[string]any)
		status, _ := step["status"].(string)
		switch status {
		case "blocked":
			blocked++
		case "error":
			failed++
		}
		last = status
	}

	parts := []string{plural(len(outcomes), "step", "steps")}
	if blocked > 0 {
		parts = append(parts, strconv.Itoa(blocked)+" blocked")
	}
	if failed > 0 {
		parts = append(parts, strconv.Itoa(failed)+" errored")
	}
	if len(outcomes) == 1 && blocked == 0 && failed == 0 && last != "" {
		parts = append(parts, last)
	}
	if stopped, ok := payload["stopped"].(string); ok && stopped != "" {
		parts = append(parts, "stopped early")
	}
	return strings.Join(parts, " · ")
}

func summariseLocks(locks map[string]any) string {
	held, _ := locks["locks"].([]any)
	waits, _ := locks["waits"].([]any)
	cycle, _ := locks["cycle"].(bool)

	out := plural(len(held), "lock", "locks")
	if len(waits) > 0 {
		out += " · " + plural(len(waits), "wait", "waits")
	}
	if cycle {
		out += " · CYCLE"
	}
	return out
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}
