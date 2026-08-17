package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/fantasy"

	"github.com/jnk/deadlocker/internal/agentapi"
	"github.com/jnk/deadlocker/internal/casedef"
)

// bind adapts one agentapi operation into a fantasy tool.
//
// Operation errors are returned to the model as a JSON error payload rather
// than as a Go error: a wrong scenario id or an invalid YAML document is
// something the model should read and correct, not something that should abort
// the turn.
func bind[In, Out any](name, description string, fn func(context.Context, In) (Out, error)) fantasy.AgentTool {
	return fantasy.NewAgentTool(name, description,
		func(ctx context.Context, in In, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			ctx = agentapi.WithSource(ctx, agentapi.SourceChat)
			out, err := fn(ctx, in)
			if err != nil {
				return fantasy.NewTextResponse(jsonString(map[string]string{"error": err.Error()})), nil
			}
			return fantasy.NewTextResponse(jsonString(out)), nil
		})
}

func jsonString(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf(`{"error":%q}`, err.Error())
	}
	return string(b)
}

// draft tool inputs.
type setDraftInput struct {
	YAML string `json:"yaml" jsonschema:"the complete scenario YAML for the draft"`
}

type runDraftInput struct {
	// Empty on purpose: the draft is whatever set_draft last stored.
	Confirm bool `json:"confirm,omitempty" jsonschema:"ignored; present so the call has a body"`
}

type saveDraftInput struct {
	Path string `json:"path,omitempty" jsonschema:"file path relative to the case library; derived from the name when omitted"`
}

// toolsFor returns the tool set for a session's mode.
func (s *Service) toolsFor(sess *Session, emit Emit) []fantasy.AgentTool {
	api := s.api

	read := []fantasy.AgentTool{
		bind("list_scenarios", "List the scenario library, optionally filtered.", api.ListScenarios),
		bind("get_scenario", "Fetch one scenario in full, including its YAML source.", api.GetScenario),
		bind("list_history", "List past runs with their outcomes.", api.ListHistory),
	}

	runTools := []fantasy.AgentTool{
		bind("start_run", "Prepare a run from a saved scenario (scenario_id) or ad-hoc YAML. "+
			"The run starts at step zero.", api.StartRun),
		bind("step_run", "Submit the next statement (or several via count). Returns each step's "+
			"outcome together with the lock state it produced, including whether the wait "+
			"graph contains a cycle.", api.StepRun),
		bind("run_all", "Advance a run until it ends or an actor blocks.", api.RunAll),
		bind("get_run", "Current state, steps and locks for a run.", api.GetRun),
		bind("get_locks", "Re-read the current locks for a run.", api.GetLocks),
		bind("close_run", "Tear a run down and drop its scratch database.", api.CloseRun),
	}

	switch sess.Mode {
	case ModeBuild:
		tools := append([]fantasy.AgentTool{}, read...)
		tools = append(tools,
			bind("validate_scenario", "Parse scenario YAML without saving. Returns a precise error "+
				"when invalid, plus warnings about scenarios that parse but will not run as "+
				"intended.", api.ValidateScenario),
			s.setDraftTool(sess, emit),
			s.runDraftTool(sess, emit),
			s.saveDraftTool(sess, emit),
		)
		tools = append(tools, runTools...)
		return tools

	default: // ModeDiscuss
		tools := append([]fantasy.AgentTool{}, read...)
		tools = append(tools, runTools...)
		tools = append(tools,
			bind("compare_runs", "Diff two recorded runs step by step.", api.CompareRuns),
		)
		return tools
	}
}

// setDraftTool updates the scenario the user is watching in the builder. It
// validates first, so the preview pane can never show something unparseable.
func (s *Service) setDraftTool(sess *Session, emit Emit) fantasy.AgentTool {
	return fantasy.NewAgentTool("set_draft",
		"Replace the working draft of the scenario the user is building. The draft is shown "+
			"beside the conversation, so call this whenever the scenario changes, even for a "+
			"small edit. Always send the COMPLETE YAML document, never a fragment or a diff. "+
			"The draft is validated here and rejected if it does not parse; it is not written "+
			"to disk until save_draft.",
		func(ctx context.Context, in setDraftInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			res, err := s.api.ValidateScenario(agentapi.WithSource(ctx, agentapi.SourceChat),
				agentapi.ValidateScenarioInput{YAML: in.YAML})
			if err != nil {
				return fantasy.NewTextResponse(jsonString(map[string]string{"error": err.Error()})), nil
			}
			if !res.Valid {
				return fantasy.NewTextResponse(jsonString(map[string]any{
					"accepted": false,
					"error":    res.Error,
					"hint":     "fix the YAML and call set_draft again with the complete document",
				})), nil
			}

			sess.mu.Lock()
			sess.Draft = in.YAML
			sess.mu.Unlock()

			view := ParseDraft(in.YAML)
			view.Notes = res.Warnings
			emit(Event{Type: EvDraft, YAML: in.YAML, Scenario: view})

			return fantasy.NewTextResponse(jsonString(map[string]any{
				"accepted": true,
				"scenario": res.Scenario,
				"warnings": res.Warnings,
				"next":     "run it with run_draft to confirm it behaves as described before saving",
			})), nil
		})
}

// runDraftTool starts a run of the current draft without saving it.
func (s *Service) runDraftTool(sess *Session, emit Emit) fantasy.AgentTool {
	return fantasy.NewAgentTool("run_draft",
		"Start a run of the current draft without saving it to disk. Returns a run id; step "+
			"through it with step_run to verify the scenario actually behaves as described. "+
			"Call set_draft first.",
		func(ctx context.Context, _ runDraftInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			draft := sess.DraftYAML()
			if strings.TrimSpace(draft) == "" {
				return fantasy.NewTextResponse(jsonString(map[string]string{
					"error": "there is no draft yet; call set_draft first",
				})), nil
			}
			out, err := s.api.StartRun(agentapi.WithSource(ctx, agentapi.SourceChat),
				agentapi.StartRunInput{YAML: draft})
			if err != nil {
				return fantasy.NewTextResponse(jsonString(map[string]string{"error": err.Error()})), nil
			}
			sess.mu.Lock()
			sess.RunID = out.Run.RunID
			sess.mu.Unlock()
			emit(Event{Type: EvRun, RunID: out.Run.RunID})
			return fantasy.NewTextResponse(jsonString(out)), nil
		})
}

// saveDraftTool writes the draft into the library, creating or updating.
func (s *Service) saveDraftTool(sess *Session, emit Emit) fantasy.AgentTool {
	return fantasy.NewAgentTool("save_draft",
		"Write the current draft into the scenario library. Only call this when the user has "+
			"asked for it. If the session was opened to edit an existing scenario, this updates "+
			"that file in place; otherwise it creates a new one.",
		func(ctx context.Context, in saveDraftInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			draft := sess.DraftYAML()
			if strings.TrimSpace(draft) == "" {
				return fantasy.NewTextResponse(jsonString(map[string]string{
					"error": "there is no draft to save",
				})), nil
			}
			ctx = agentapi.WithSource(ctx, agentapi.SourceChat)

			var (
				out agentapi.SaveScenarioOutput
				err error
			)
			if sess.ScenarioID != "" {
				out, err = s.api.UpdateScenario(ctx, agentapi.UpdateScenarioInput{ID: sess.ScenarioID, YAML: draft})
			} else {
				out, err = s.api.CreateScenario(ctx, agentapi.CreateScenarioInput{YAML: draft, Path: in.Path})
			}
			if err != nil {
				return fantasy.NewTextResponse(jsonString(map[string]string{"error": err.Error()})), nil
			}
			sess.mu.Lock()
			sess.ScenarioID = out.ID
			sess.mu.Unlock()
			emit(Event{Type: EvSaved, Text: out.Path, YAML: draft})
			return fantasy.NewTextResponse(jsonString(out)), nil
		})
}

// ---------------------------------------------------------- system prompts

func (s *Service) systemPrompt(sess *Session) string {
	var b strings.Builder

	b.WriteString(`You are the assistant built into Deadlocker, a tool that provokes and explains
MySQL locking behaviour by stepping through concurrency scenarios against a real
MySQL server running in Docker.

You are talking to an engineer who is investigating locking behaviour. Be
concrete and technical. Assume they know SQL.

The single most valuable thing you can do is VERIFY rather than assert. You have
tools that run scenarios against a real MySQL 8.4 server and report the actual
lock modes taken. When you are asked what MySQL does in some situation, prefer
building or running a scenario and reading the result over answering from
memory. When you do answer from memory, say so.

When you report a lock, use the real mode names: X,REC_NOT_GAP is a record lock,
X,GAP is a gap lock, bare X on a record is a next-key lock, X,INSERT_INTENTION
is what an INSERT takes and is what conflicts with someone else's gap lock.

Keep replies tight. No preamble, no restating the question, no bulleted summary
of what you are about to do.
`)

	switch sess.Mode {
	case ModeBuild:
		b.WriteString(`
## Your task

You are helping the user build ONE scenario, in a builder view: the conversation
is on one side and the live draft of the scenario is on the other.

How to work:

1. Ask at most one or two short clarifying questions if the request is genuinely
   ambiguous. Otherwise just build something and iterate.
2. Call set_draft with the COMPLETE YAML every time the scenario changes, so the
   user can see it. Never reply with YAML in the chat body — put it in the
   draft. Describe what you changed in one or two sentences instead.
3. Verify it: run_draft, then step_run through the whole thing, and read the
   lock state each step returns. If the scenario does not do what you claimed,
   fix it and say what surprised you.
4. Call close_run when you are done verifying.
5. Only call save_draft when the user explicitly asks to save.

A scenario that has not been run is not finished. The point of this tool is that
claims about MySQL are checked against MySQL.

`)
		b.WriteString(agentapi.FormatDoc)

	default:
		b.WriteString(`
## Your task

You are discussing an existing scenario with the user. You can start runs, step
through them, and read the resulting locks to answer questions or test a
hypothesis they raise.

When the user wonders "what if X", the good move is usually to run it: start a
run of a modified scenario with start_run using ad-hoc yaml, step through it,
and report what actually happened. Close runs you started with close_run.

Do not modify the saved scenario. If the user wants changes made permanent,
tell them to open the builder.
`)
	}

	if sess.ScenarioID != "" {
		b.WriteString("\n## Current scenario\n\nThe user is looking at scenario id `" + sess.ScenarioID + "`.\n")
		if out, err := s.api.GetScenario(context.Background(), agentapi.GetScenarioInput{ID: sess.ScenarioID}); err == nil {
			b.WriteString("Its current YAML is:\n\n```yaml\n")
			b.WriteString(out.YAML)
			b.WriteString("\n```\n")
		}
	}
	if sess.RunID != "" {
		b.WriteString("\nThere is an active run with id `" + sess.RunID + "`.\n")
	}
	if draft := sess.DraftYAML(); draft != "" && sess.Mode == ModeBuild {
		b.WriteString("\n## Current draft\n\n```yaml\n" + draft + "\n```\n")
	}

	return b.String()
}

// StarterDraft is the skeleton a fresh builder session begins from, so the
// preview pane is never empty.
func StarterDraft() string {
	return starterDraft
}

const starterDraft = `name: New scenario
category: Custom
description: |
  Describe what this scenario demonstrates.

mysql:
  image: mysql:8.4
  isolation: REPEATABLE READ
  lock_wait_timeout: 300

schema:
  - |
    CREATE TABLE t (
      id INT NOT NULL,
      val VARCHAR(32) NOT NULL,
      PRIMARY KEY (id)
    ) ENGINE=InnoDB

seed:
  - INSERT INTO t (id, val) VALUES (10, 'ten'), (20, 'twenty')

actors:
  - id: a
    name: Session A
    accent: blue
  - id: b
    name: Session B
    accent: amber

steps:
  - actor: a
    sql: BEGIN
    expect: ok
`

// EnsureValidDraft returns the draft if it parses, otherwise the starter.
func EnsureValidDraft(draft string) string {
	if strings.TrimSpace(draft) == "" {
		return starterDraft
	}
	if _, err := casedef.Parse([]byte(draft)); err != nil {
		return starterDraft
	}
	return draft
}

// ParseDraft renders a draft as the steps pane needs it. Parsing happens here
// rather than in the browser because the canonical parser and validator are
// already in Go; shipping a YAML parser to the client would risk the two
// disagreeing about what a scenario means.
func ParseDraft(yaml string) *DraftView {
	view := &DraftView{}
	c, err := casedef.Parse([]byte(yaml))
	if err != nil {
		view.Error = err.Error()
		return view
	}

	view.Valid = true
	view.Name = c.Name

	accents := map[string]string{}
	for _, a := range c.Actors {
		accents[a.ID] = a.Accent
		view.Actors = append(view.Actors, DraftActor{ID: a.ID, Name: a.Name, Accent: a.Accent})
	}
	for i, st := range c.Steps {
		view.Steps = append(view.Steps, DraftStep{
			Index: i + 1, Actor: st.Actor, Accent: accents[st.Actor],
			Label: st.Label, SQL: st.SQL, Note: st.Note, Expect: string(st.Expect),
		})
	}
	return view
}
