// Package agentapi is the operation layer shared by the MCP server and the
// built-in chat.
//
// Both need exactly the same abilities — read scenarios, write scenarios, start
// and step runs, inspect locks — so those live here once, as ordinary typed Go
// functions. The MCP server and the chat agent are thin adapters over this
// package rather than two parallel implementations that can drift apart.
package agentapi

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jnk/deadlocker/internal/casedef"
	"github.com/jnk/deadlocker/internal/engine"
	"github.com/jnk/deadlocker/internal/store"
)

// API bundles the library and run manager behind operations.
type API struct {
	lib  *casedef.Library
	mgr  *engine.Manager
	hub  *Hub
	jobs *Jobs
	// versions is optional: without it scenarios still save, they just are not
	// kept in history.
	versions *store.Store
}

func New(lib *casedef.Library, mgr *engine.Manager, hub *Hub) *API {
	return &API{lib: lib, mgr: mgr, hub: hub, jobs: NewJobs()}
}

// UseVersions attaches the revision store and installs the library's save hook,
// so every scenario write — by hand, from the chat, or over MCP — is recorded.
func (a *API) UseVersions(s *store.Store) {
	a.versions = s
	if s == nil {
		a.lib.SetRecorder(nil)
		return
	}
	a.lib.SetRecorder(func(c *casedef.Case, source []byte, note string) {
		if c == nil || c.ID == "" {
			return
		}
		// A failure here must not break the save: the YAML file is the source of
		// truth and is already written by this point.
		_, _, _ = s.RecordScenario(c.ID, c.Name, c.Path, string(source), note)
	})
}

// SeedVersions records a baseline revision for every scenario whose current
// source is not already the newest one on file. It runs at startup so the
// examples shipped with the app, and any edit made outside it, have something
// to roll back to rather than history starting at the first in-app change.
func (a *API) SeedVersions(note string) int {
	if a.versions == nil {
		return 0
	}
	n := 0
	for _, c := range a.lib.List() {
		if c.ID == "" || c.Source == "" {
			continue
		}
		if _, written, err := a.versions.RecordScenario(c.ID, c.Name, c.Path, c.Source, note); err == nil && written {
			n++
		}
	}
	return n
}

// randomBytes is used for job ids.
func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

func (a *API) Hub() *Hub { return a.hub }

func (a *API) note(ctx context.Context, act Activity) {
	act.Source = SourceOf(ctx)
	a.hub.Publish(act)
}

// ---------------------------------------------------------------- scenarios

// ScenarioSummary is the compact form used in listings.
type ScenarioSummary struct {
	ID          string   `json:"id" jsonschema:"the scenario identifier"`
	Name        string   `json:"name"`
	Category    string   `json:"category"`
	Tags        []string `json:"tags,omitempty"`
	Path        string   `json:"path"`
	Actors      int      `json:"actors"`
	Steps       int      `json:"steps"`
	Image       string   `json:"image"`
	Isolation   string   `json:"isolation,omitempty"`
	Description string   `json:"description,omitempty"`
}

type ListScenariosInput struct {
	Category string `json:"category,omitempty" jsonschema:"filter by category, case-insensitive substring"`
	Tag      string `json:"tag,omitempty" jsonschema:"filter by tag"`
	Query    string `json:"query,omitempty" jsonschema:"free text match against name, description and tags"`
}

type ListScenariosOutput struct {
	Scenarios []ScenarioSummary `json:"scenarios"`
	Total     int               `json:"total"`
}

// ListScenarios returns the scenario library, optionally filtered.
func (a *API) ListScenarios(ctx context.Context, in ListScenariosInput) (ListScenariosOutput, error) {
	if err := a.lib.Load(); err != nil {
		return ListScenariosOutput{}, err
	}
	var out ListScenariosOutput
	for _, c := range a.lib.List() {
		if in.Category != "" && !containsFold(c.Category, in.Category) {
			continue
		}
		if in.Tag != "" && !hasTag(c.Tags, in.Tag) {
			continue
		}
		if in.Query != "" {
			hay := strings.ToLower(c.Name + " " + c.Description + " " + strings.Join(c.Tags, " "))
			if !strings.Contains(hay, strings.ToLower(in.Query)) {
				continue
			}
		}
		out.Scenarios = append(out.Scenarios, summarise(c))
	}
	out.Total = len(out.Scenarios)
	return out, nil
}

func summarise(c *casedef.Case) ScenarioSummary {
	return ScenarioSummary{
		ID: c.ID, Name: c.Name, Category: c.Category, Tags: c.Tags, Path: c.Path,
		Actors: len(c.Actors), Steps: len(c.Steps),
		Image: c.MySQL.Image, Isolation: c.NormalisedIsolation(),
		Description: firstParagraph(c.Description),
	}
}

type GetScenarioInput struct {
	ID string `json:"id" jsonschema:"the scenario id, as returned by list_scenarios"`
}

type GetScenarioOutput struct {
	Scenario ScenarioSummary `json:"scenario"`
	// YAML is the complete source, which is what an agent needs in order to
	// modify it.
	YAML   string          `json:"yaml"`
	Steps  []casedef.Step  `json:"steps"`
	Actors []casedef.Actor `json:"actors"`
	Schema []string        `json:"schema,omitempty"`
	Seed   []string        `json:"seed,omitempty"`
}

// GetScenario returns one scenario in full, including its YAML source.
func (a *API) GetScenario(ctx context.Context, in GetScenarioInput) (GetScenarioOutput, error) {
	c, err := a.mustCase(in.ID)
	if err != nil {
		return GetScenarioOutput{}, err
	}
	return GetScenarioOutput{
		Scenario: summarise(c),
		YAML:     c.Source,
		Steps:    c.Steps,
		Actors:   c.Actors,
		Schema:   c.Schema,
		Seed:     c.Seed,
	}, nil
}

type ValidateScenarioInput struct {
	YAML string `json:"yaml" jsonschema:"the complete scenario YAML to check"`
}

type ValidateScenarioOutput struct {
	Valid bool `json:"valid"`
	// Error explains precisely what is wrong when Valid is false.
	Error    string           `json:"error,omitempty"`
	Scenario *ScenarioSummary `json:"scenario,omitempty"`
	// Warnings are structural problems that parse fine but will not behave as
	// intended when stepped through.
	Warnings []string `json:"warnings,omitempty"`
}

// ValidateScenario parses YAML without writing anything. An agent should call
// this before create or update.
func (a *API) ValidateScenario(ctx context.Context, in ValidateScenarioInput) (ValidateScenarioOutput, error) {
	c, err := casedef.Parse([]byte(in.YAML))
	if err != nil {
		return ValidateScenarioOutput{Valid: false, Error: err.Error()}, nil
	}
	s := summarise(c)
	return ValidateScenarioOutput{Valid: true, Scenario: &s, Warnings: lintCase(c)}, nil
}

// lintCase catches the mistakes that produce a valid file which nonetheless
// wedges when you step through it.
func lintCase(c *casedef.Case) []string {
	var warnings []string

	// An actor whose statement is expected to block cannot run its next step
	// until something releases the lock. If the next step for that actor comes
	// before any other actor's step, the scenario can never finish.
	blocked := map[string]int{}
	for i, s := range c.Steps {
		if prev, stuck := blocked[s.Actor]; stuck {
			warnings = append(warnings, fmt.Sprintf(
				"step %d gives %q another statement while its step %d is expected to block; "+
					"a connection runs one statement at a time, so put the releasing COMMIT or ROLLBACK first",
				i+1, s.Actor, prev))
			delete(blocked, s.Actor)
		}
		if s.Expect == casedef.ExpectBlocks {
			blocked[s.Actor] = i + 1
		}
		up := strings.ToUpper(strings.TrimSpace(s.SQL))
		if strings.HasPrefix(up, "COMMIT") || strings.HasPrefix(up, "ROLLBACK") {
			// Releasing locks can unblock every other actor.
			blocked = map[string]int{}
		}
	}

	used := map[string]bool{}
	for _, s := range c.Steps {
		used[s.Actor] = true
	}
	for _, act := range c.Actors {
		if !used[act.ID] {
			warnings = append(warnings, fmt.Sprintf("actor %q never runs a step", act.ID))
		}
	}

	if len(c.Schema) == 0 {
		warnings = append(warnings, "no schema statements: the scenario will run against an empty database")
	}
	return warnings
}

type CreateScenarioInput struct {
	YAML string `json:"yaml" jsonschema:"the complete scenario YAML"`
	Path string `json:"path,omitempty" jsonschema:"file path relative to the case library, e.g. my-cases/experiment.yaml; derived from the name when omitted"`
}

type SaveScenarioOutput struct {
	ID       string          `json:"id"`
	Path     string          `json:"path"`
	Scenario ScenarioSummary `json:"scenario"`
	Warnings []string        `json:"warnings,omitempty"`
}

// CreateScenario writes a new scenario file. It refuses to overwrite one that
// already exists; use UpdateScenario for that.
func (a *API) CreateScenario(ctx context.Context, in CreateScenarioInput) (SaveScenarioOutput, error) {
	parsed, err := casedef.Parse([]byte(in.YAML))
	if err != nil {
		return SaveScenarioOutput{}, fmt.Errorf("scenario is not valid: %w", err)
	}
	path := in.Path
	if path == "" {
		path = suggestPath(parsed)
	}

	// A scenario's id normally comes from its file name rather than an explicit
	// `id:` field, so checking the parsed id alone would miss the common case.
	// Refuse if either the target file or the id it would take is already
	// occupied.
	if a.lib.Exists(path) {
		return SaveScenarioOutput{}, fmt.Errorf(
			"%s already exists; use update_scenario to change it, or pass a different path", path)
	}
	candidate := parsed.ID
	if candidate == "" {
		candidate = casedef.IDForPath(path)
	}
	if existing, ok := a.lib.Get(candidate); ok {
		return SaveScenarioOutput{}, fmt.Errorf(
			"a scenario with id %q already exists at %s; use update_scenario to change it, or pass a different path",
			candidate, existing.Path)
	}
	saved, err := a.lib.SaveNote(path, []byte(in.YAML), "created via "+SourceOf(ctx))
	if err != nil {
		return SaveScenarioOutput{}, err
	}
	a.note(ctx, Activity{
		Kind: KindScenarioCreated, Tool: "create_scenario", ScenarioID: saved.ID,
		Summary: fmt.Sprintf("created scenario %q", saved.Name),
	})
	return SaveScenarioOutput{ID: saved.ID, Path: saved.Path, Scenario: summarise(saved), Warnings: lintCase(saved)}, nil
}

type UpdateScenarioInput struct {
	ID   string `json:"id" jsonschema:"the scenario to replace"`
	YAML string `json:"yaml" jsonschema:"the complete new scenario YAML"`
	Note string `json:"note,omitempty" jsonschema:"a short description of the change, kept in the scenario's version history"`
}

// UpdateScenario rewrites an existing scenario in place, validating first.
func (a *API) UpdateScenario(ctx context.Context, in UpdateScenarioInput) (SaveScenarioOutput, error) {
	existing, err := a.mustCase(in.ID)
	if err != nil {
		return SaveScenarioOutput{}, err
	}
	if _, err := casedef.Parse([]byte(in.YAML)); err != nil {
		return SaveScenarioOutput{}, fmt.Errorf("scenario is not valid: %w", err)
	}
	note := in.Note
	if strings.TrimSpace(note) == "" {
		note = "updated via " + SourceOf(ctx)
	}
	saved, err := a.lib.SaveNote(existing.Path, []byte(in.YAML), note)
	if err != nil {
		return SaveScenarioOutput{}, err
	}
	a.note(ctx, Activity{
		Kind: KindScenarioUpdated, Tool: "update_scenario", ScenarioID: saved.ID,
		Summary: fmt.Sprintf("updated scenario %q", saved.Name),
	})
	return SaveScenarioOutput{ID: saved.ID, Path: saved.Path, Scenario: summarise(saved), Warnings: lintCase(saved)}, nil
}

// ---------------------------------------------------------- scenario history

// ScenarioVersionSummary is one revision without its source, for listings.
type ScenarioVersionSummary struct {
	Version   uint64    `json:"version"`
	SavedAt   time.Time `json:"saved_at"`
	Note      string    `json:"note,omitempty"`
	Name      string    `json:"name,omitempty"`
	Path      string    `json:"path,omitempty"`
	Lines     int       `json:"lines"`
	Bytes     int       `json:"bytes"`
	IsCurrent bool      `json:"is_current"`
}

type ListScenarioVersionsInput struct {
	ID    string `json:"id" jsonschema:"the scenario whose history to list"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum revisions to return, newest first"`
}

type ListScenarioVersionsOutput struct {
	ID       string                   `json:"id"`
	Versions []ScenarioVersionSummary `json:"versions"`
}

// ListScenarioVersions returns a scenario's revision history, newest first.
func (a *API) ListScenarioVersions(ctx context.Context, in ListScenarioVersionsInput) (ListScenarioVersionsOutput, error) {
	c, err := a.mustCase(in.ID)
	if err != nil {
		return ListScenarioVersionsOutput{}, err
	}
	if a.versions == nil {
		return ListScenarioVersionsOutput{ID: c.ID}, nil
	}
	list, err := a.versions.ScenarioVersions(c.ID, c.Source, in.Limit)
	if err != nil {
		return ListScenarioVersionsOutput{}, err
	}
	out := ListScenarioVersionsOutput{ID: c.ID, Versions: make([]ScenarioVersionSummary, 0, len(list))}
	for _, v := range list {
		out.Versions = append(out.Versions, ScenarioVersionSummary{
			Version: v.Version, SavedAt: v.SavedAt, Note: v.Note,
			Name: v.Name, Path: v.Path,
			Lines: strings.Count(strings.TrimRight(v.Source, "\n"), "\n") + 1,
			Bytes: len(v.Source), IsCurrent: v.IsCurrent,
		})
	}
	return out, nil
}

type GetScenarioVersionInput struct {
	ID      string `json:"id" jsonschema:"the scenario"`
	Version uint64 `json:"version" jsonschema:"which revision to read"`
}

type GetScenarioVersionOutput struct {
	ID      string `json:"id"`
	Version uint64 `json:"version"`
	SavedAt string `json:"saved_at"`
	Note    string `json:"note,omitempty"`
	YAML    string `json:"yaml"`
}

// GetScenarioVersion returns the YAML a scenario had at one revision.
func (a *API) GetScenarioVersion(ctx context.Context, in GetScenarioVersionInput) (GetScenarioVersionOutput, error) {
	if a.versions == nil {
		return GetScenarioVersionOutput{}, errors.New("scenario history is not available")
	}
	c, err := a.mustCase(in.ID)
	if err != nil {
		return GetScenarioVersionOutput{}, err
	}
	v, err := a.versions.ScenarioVersion(c.ID, in.Version)
	if err != nil {
		return GetScenarioVersionOutput{}, err
	}
	return GetScenarioVersionOutput{
		ID: c.ID, Version: v.Version, SavedAt: v.SavedAt.Format(time.RFC3339),
		Note: v.Note, YAML: v.Source,
	}, nil
}

type RestoreScenarioVersionInput struct {
	ID      string `json:"id" jsonschema:"the scenario to roll back"`
	Version uint64 `json:"version" jsonschema:"the revision to restore"`
}

// RestoreScenarioVersion writes an old revision back to disk.
//
// History is append-only: the restore itself becomes the newest revision, so
// the state being replaced is still reachable and a rollback can be undone.
func (a *API) RestoreScenarioVersion(ctx context.Context, in RestoreScenarioVersionInput) (SaveScenarioOutput, error) {
	if a.versions == nil {
		return SaveScenarioOutput{}, errors.New("scenario history is not available")
	}
	c, err := a.mustCase(in.ID)
	if err != nil {
		return SaveScenarioOutput{}, err
	}
	v, err := a.versions.ScenarioVersion(c.ID, in.Version)
	if err != nil {
		return SaveScenarioOutput{}, err
	}
	if v.Source == c.Source {
		return SaveScenarioOutput{}, fmt.Errorf("version %d is already what is on disk", in.Version)
	}
	// The old revision still has to parse: a scenario saved before a format
	// change could otherwise be restored into a broken file.
	if _, err := casedef.Parse([]byte(v.Source)); err != nil {
		return SaveScenarioOutput{}, fmt.Errorf("version %d no longer parses: %w", in.Version, err)
	}

	saved, err := a.lib.SaveNote(c.Path, []byte(v.Source),
		fmt.Sprintf("restored version %d", in.Version))
	if err != nil {
		return SaveScenarioOutput{}, err
	}
	a.note(ctx, Activity{
		Kind: KindScenarioUpdated, Tool: "restore_scenario_version", ScenarioID: saved.ID,
		Summary: fmt.Sprintf("restored %q to version %d", saved.Name, in.Version),
	})
	return SaveScenarioOutput{ID: saved.ID, Path: saved.Path, Scenario: summarise(saved), Warnings: lintCase(saved)}, nil
}

// suggestPath derives a file path from a scenario's category and name.
func suggestPath(c *casedef.Case) string {
	name := c.ID
	if name == "" {
		name = slug(c.Name)
	}
	if name == "" {
		name = "scenario"
	}
	dir := slug(c.Category)
	if dir == "" || dir == "uncategorised" {
		dir = "custom"
	}
	return filepath.Join(dir, name+".yaml")
}

func slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// -------------------------------------------------------------------- runs

type StartRunInput struct {
	ScenarioID string `json:"scenario_id,omitempty" jsonschema:"run a scenario from the library"`
	YAML       string `json:"yaml,omitempty" jsonschema:"run ad-hoc YAML without saving it; use instead of scenario_id"`
}

// StepView is one step as an agent sees it.
type StepView struct {
	Index        int        `json:"index"`
	Actor        string     `json:"actor"`
	Label        string     `json:"label"`
	SQL          string     `json:"sql"`
	Status       string     `json:"status"`
	DurationMS   int64      `json:"duration_ms"`
	RowCount     int        `json:"row_count,omitempty"`
	RowsAffected int64      `json:"rows_affected,omitempty"`
	Error        string     `json:"error,omitempty"`
	ErrorKind    string     `json:"error_kind,omitempty"`
	BlockedBy    []string   `json:"blocked_by,omitempty"`
	WaitExplain  string     `json:"wait_explain,omitempty"`
	Expect       string     `json:"expect,omitempty"`
	Actual       string     `json:"actual,omitempty"`
	Verdict      string     `json:"verdict,omitempty"`
	VerdictNote  string     `json:"verdict_note,omitempty"`
	Rows         [][]string `json:"rows,omitempty"`
	Columns      []string   `json:"columns,omitempty"`
}

type RunView struct {
	RunID          string     `json:"run_id"`
	ScenarioID     string     `json:"scenario_id"`
	Name           string     `json:"name"`
	Status         string     `json:"status"`
	Cursor         int        `json:"cursor"`
	Total          int        `json:"total"`
	Database       string     `json:"database"`
	Steps          []StepView `json:"steps,omitempty"`
	DeadlockReport string     `json:"deadlock_report,omitempty"`
	URL            string     `json:"url" jsonschema:"open this in the Deadlocker UI to watch the run"`
}

type StartRunOutput struct {
	Run RunView `json:"run"`
}

// StartRun prepares a run and leaves it at step zero.
func (a *API) StartRun(ctx context.Context, in StartRunInput) (StartRunOutput, error) {
	var c *casedef.Case
	switch {
	case in.YAML != "":
		parsed, err := casedef.Parse([]byte(in.YAML))
		if err != nil {
			return StartRunOutput{}, fmt.Errorf("scenario is not valid: %w", err)
		}
		// A run of unsaved YAML is real, but the scenario behind it is not in
		// the library. Flagging it keeps it out of the sidebar and out of any
		// saved scenario's history.
		parsed.Ephemeral = true
		if parsed.ID == "" {
			parsed.ID = "draft"
		}
		c = parsed
	case in.ScenarioID != "":
		got, err := a.mustCase(in.ScenarioID)
		if err != nil {
			return StartRunOutput{}, err
		}
		c = got
	default:
		return StartRunOutput{}, errors.New("provide either scenario_id or yaml")
	}

	startCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 6*time.Minute)
	defer cancel()

	run, err := a.mgr.Start(startCtx, c)
	if err != nil {
		return StartRunOutput{}, fmt.Errorf("could not start the run: %w", err)
	}
	a.note(ctx, Activity{
		Kind: KindRunStarted, Tool: "start_run", ScenarioID: c.ID, RunID: run.ID,
		Summary: fmt.Sprintf("started a run of %q", c.Name),
	})
	return StartRunOutput{Run: a.runView(run, false)}, nil
}

type StepRunInput struct {
	RunID string `json:"run_id"`
	Count int    `json:"count,omitempty" jsonschema:"how many steps to submit; defaults to 1"`
}

// StepOutcome is one submitted step plus the lock state it produced. Returning
// the locks alongside the step is the point: it is what lets an agent reason
// about why something blocked rather than guessing.
type StepOutcome struct {
	Step  StepView `json:"step"`
	Locks LockView `json:"locks"`
}

type LockLine struct {
	Actor   string `json:"actor"`
	Table   string `json:"table"`
	Index   string `json:"index,omitempty"`
	Mode    string `json:"mode"`
	Status  string `json:"status"`
	Data    string `json:"data,omitempty"`
	Explain string `json:"explain"`
}

type WaitLine struct {
	Waiting  string `json:"waiting_actor"`
	Blocking string `json:"blocking_actor"`
	Detail   string `json:"detail,omitempty"`
}

type LockView struct {
	Locks []LockLine `json:"locks,omitempty"`
	Waits []WaitLine `json:"waits,omitempty"`
	// Cycle is true when the wait graph contains a loop, which is exactly what
	// InnoDB's deadlock detector reacts to.
	Cycle bool `json:"cycle"`
}

type StepRunOutput struct {
	Outcomes []StepOutcome `json:"outcomes"`
	Run      RunView       `json:"run"`
	// Stopped explains why fewer steps ran than asked for.
	Stopped string `json:"stopped,omitempty"`
}

// StepRun submits the next statement (or several) and evaluates each one.
func (a *API) StepRun(ctx context.Context, in StepRunInput) (StepRunOutput, error) {
	run, ok := a.mgr.Get(in.RunID)
	if !ok {
		return StepRunOutput{}, fmt.Errorf("unknown run %q; start one with start_run", in.RunID)
	}
	count := in.Count
	if count <= 0 {
		count = 1
	}
	if count > 200 {
		count = 200
	}

	var out StepRunOutput
	for i := 0; i < count; i++ {
		// A person can stop a run the assistant is driving. Report it plainly
		// so the model knows this was deliberate and does not simply retry.
		if reason := run.TakeInterrupt(); reason != "" {
			out.Stopped = reason + ". Do not resume unless the user asks; " +
				"ask what they want changed instead."
			break
		}
		res, err := run.Step(context.WithoutCancel(ctx))
		if err != nil {
			if errors.Is(err, engine.ErrNoMoreSteps) {
				out.Stopped = "the scenario has no more steps"
				break
			}
			var blocked *engine.ActorBlockedError
			if errors.As(err, &blocked) {
				out.Stopped = blocked.Error()
				break
			}
			return out, err
		}
		out.Outcomes = append(out.Outcomes, StepOutcome{
			Step:  stepView(res),
			Locks: lockView(run.Snapshot()),
		})
	}

	a.note(ctx, Activity{
		Kind: KindRunStepped, Tool: "step_run", RunID: run.ID, ScenarioID: run.Case.ID,
		Summary: fmt.Sprintf("submitted %d step(s)", len(out.Outcomes)),
	})
	out.Run = a.runView(run, true)
	return out, nil
}

type RunAllInput struct {
	RunID string `json:"run_id"`
}

// RunAll advances until the scenario ends or an actor is stuck waiting.
func (a *API) RunAll(ctx context.Context, in RunAllInput) (StepRunOutput, error) {
	return a.StepRun(ctx, StepRunInput{RunID: in.RunID, Count: 200})
}

type GetRunInput struct {
	RunID string `json:"run_id"`
}

type GetRunOutput struct {
	Run   RunView  `json:"run"`
	Locks LockView `json:"locks"`
}

// GetRun returns the current state of a run, its steps and its locks.
func (a *API) GetRun(ctx context.Context, in GetRunInput) (GetRunOutput, error) {
	run, ok := a.mgr.Get(in.RunID)
	if !ok {
		return GetRunOutput{}, fmt.Errorf("unknown run %q", in.RunID)
	}
	return GetRunOutput{Run: a.runView(run, true), Locks: lockView(run.Snapshot())}, nil
}

type GetLocksInput struct {
	RunID string `json:"run_id"`
}

type GetLocksOutput struct {
	Locks LockView `json:"locks"`
}

// GetLocks re-reads performance_schema for a run.
func (a *API) GetLocks(ctx context.Context, in GetLocksInput) (GetLocksOutput, error) {
	run, ok := a.mgr.Get(in.RunID)
	if !ok {
		return GetLocksOutput{}, fmt.Errorf("unknown run %q", in.RunID)
	}
	return GetLocksOutput{Locks: lockView(run.Snapshot())}, nil
}

// StopRunInput asks a run to stop advancing without tearing it down.
type StopRunInput struct {
	RunID  string `json:"run_id"`
	Reason string `json:"reason,omitempty"`
}

type StopRunOutput struct {
	Stopped bool `json:"stopped"`
}

// StopRun halts a run that is being stepped, leaving it open for inspection.
func (a *API) StopRun(ctx context.Context, in StopRunInput) (StopRunOutput, error) {
	run, ok := a.mgr.Get(in.RunID)
	if !ok {
		return StopRunOutput{}, fmt.Errorf("unknown run %q", in.RunID)
	}
	run.Interrupt(in.Reason)
	a.note(ctx, Activity{
		Kind: KindRunStepped, Tool: "stop_run", RunID: in.RunID,
		Summary: "stopped a run",
	})
	return StopRunOutput{Stopped: true}, nil
}

type CloseRunInput struct {
	RunID string `json:"run_id"`
}

type CloseRunOutput struct {
	Closed bool `json:"closed"`
}

// CloseRun tears a run down and drops its scratch database.
func (a *API) CloseRun(ctx context.Context, in CloseRunInput) (CloseRunOutput, error) {
	if err := a.mgr.CloseRun(in.RunID); err != nil {
		return CloseRunOutput{}, err
	}
	a.note(ctx, Activity{
		Kind: KindRunClosed, Tool: "close_run", RunID: in.RunID,
		Summary: "closed a run",
	})
	return CloseRunOutput{Closed: true}, nil
}

// ----------------------------------------------------------------- history

type ListHistoryInput struct {
	ScenarioID string `json:"scenario_id,omitempty" jsonschema:"limit to one scenario"`
	Limit      int    `json:"limit,omitempty" jsonschema:"maximum records to return; defaults to 20"`
}

type HistoryRecord struct {
	RunID      string    `json:"run_id"`
	ScenarioID string    `json:"scenario_id"`
	Name       string    `json:"name"`
	StartedAt  time.Time `json:"started_at"`
	Outcome    string    `json:"outcome"`
	Isolation  string    `json:"isolation,omitempty"`
	Submitted  int       `json:"submitted"`
	Blocked    int       `json:"blocked"`
	Deadlocks  int       `json:"deadlocks"`
	Timeouts   int       `json:"timeouts"`
	Mismatches int       `json:"mismatches"`
}

type ListHistoryOutput struct {
	Runs []HistoryRecord `json:"runs"`
}

// ListHistory returns past runs, newest first.
func (a *API) ListHistory(ctx context.Context, in ListHistoryInput) (ListHistoryOutput, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}
	var records []*engine.Record
	if in.ScenarioID != "" {
		records = a.mgr.History().ForCase(in.ScenarioID)
	} else {
		records = a.mgr.History().All()
	}
	var out ListHistoryOutput
	for i, r := range records {
		if i >= limit {
			break
		}
		out.Runs = append(out.Runs, HistoryRecord{
			RunID: r.RunID, ScenarioID: r.CaseID, Name: r.CaseName,
			StartedAt: r.StartedAt, Outcome: r.Outcome, Isolation: r.Isolation,
			Submitted: r.Submitted, Blocked: r.Blocked, Deadlocks: r.Deadlocks,
			Timeouts: r.Timeouts, Mismatches: r.Mismatches,
		})
	}
	return out, nil
}

type CompareRunsInput struct {
	A string `json:"a" jsonschema:"run id of the first run"`
	B string `json:"b" jsonschema:"run id of the second run"`
}

type CompareRunsOutput struct {
	Setup   []engine.FieldDiff `json:"setup,omitempty"`
	Steps   []StepDiffView     `json:"steps"`
	Changed int                `json:"changed"`
}

type StepDiffView struct {
	Index       int                `json:"index"`
	Label       string             `json:"label"`
	Differences []engine.FieldDiff `json:"differences,omitempty"`
}

// CompareRuns diffs two recorded runs.
func (a *API) CompareRuns(ctx context.Context, in CompareRunsInput) (CompareRunsOutput, error) {
	ra, okA := a.mgr.History().Get(in.A)
	rb, okB := a.mgr.History().Get(in.B)
	if !okA {
		return CompareRunsOutput{}, fmt.Errorf("unknown run %q", in.A)
	}
	if !okB {
		return CompareRunsOutput{}, fmt.Errorf("unknown run %q", in.B)
	}
	diff := engine.Diff(ra, rb)
	out := CompareRunsOutput{Setup: diff.Setup, Changed: diff.Changed}
	for _, s := range diff.Steps {
		if !s.Differs() {
			continue
		}
		out.Steps = append(out.Steps, StepDiffView{Index: s.Index, Label: s.Label, Differences: s.Differences})
	}
	return out, nil
}

// ------------------------------------------------------------------ helpers

func (a *API) mustCase(id string) (*casedef.Case, error) {
	if c, ok := a.lib.Get(id); ok {
		return c, nil
	}
	if err := a.lib.Load(); err != nil {
		return nil, err
	}
	if c, ok := a.lib.Get(id); ok {
		return c, nil
	}
	return nil, fmt.Errorf("no scenario with id %q; call list_scenarios to see what exists", id)
}

func (a *API) runView(run *engine.Run, withSteps bool) RunView {
	st := run.State()
	v := RunView{
		RunID: st.ID, ScenarioID: st.CaseID, Name: st.CaseName, Status: st.Status,
		Cursor: st.Cursor, Total: st.Total, Database: st.Database,
		DeadlockReport: st.DeadlockReport,
		URL:            "/run/" + st.ID,
	}
	if withSteps {
		for _, s := range run.Steps() {
			v.Steps = append(v.Steps, stepView(s))
		}
	}
	return v
}

func stepView(s *engine.StepResult) StepView {
	v := StepView{
		Index: s.Index, Actor: s.Actor, Label: s.Label, SQL: s.SQL,
		Status: string(s.Status), DurationMS: s.DurationMS,
		RowCount: s.RowCount, RowsAffected: s.RowsAffected,
		BlockedBy: s.BlockedBy, WaitExplain: s.WaitExplain,
		Expect: string(s.Expect), Actual: string(s.Actual),
		Verdict: s.Verdict, VerdictNote: s.VerdictNote,
		Columns: s.Columns, Rows: s.Rows,
	}
	if s.Error != nil {
		v.Error = fmt.Sprintf("%d: %s", s.Error.Code, s.Error.Message)
		v.ErrorKind = s.Error.Kind
	}
	return v
}

func lockView(snap engine.LockSnapshot) LockView {
	var v LockView
	for _, l := range snap.Locks {
		v.Locks = append(v.Locks, LockLine{
			Actor: l.Actor, Table: l.Table, Index: l.Index, Mode: l.LockMode,
			Status: l.LockStatus, Data: l.LockData, Explain: l.Explain,
		})
	}
	edges := map[string][]string{}
	for _, w := range snap.Waits {
		detail := ""
		if w.WaitingLock != nil && w.BlockingLock != nil {
			detail = fmt.Sprintf("requesting %s, blocked by %s on %s",
				w.WaitingLock.LockMode, w.BlockingLock.LockMode, emptyDash(w.BlockingLock.LockData))
		}
		v.Waits = append(v.Waits, WaitLine{Waiting: w.WaitingActor, Blocking: w.BlockingActor, Detail: detail})
		if w.WaitingActor != "" && w.BlockingActor != "" {
			edges[w.WaitingActor] = append(edges[w.WaitingActor], w.BlockingActor)
		}
	}
	v.Cycle = hasCycle(edges)
	return v
}

func hasCycle(edges map[string][]string) bool {
	const (
		onPath = 1
		done   = 2
	)
	state := map[string]int{}
	found := false
	var visit func(string)
	visit = func(n string) {
		if found || state[n] == done {
			return
		}
		if state[n] == onPath {
			found = true
			return
		}
		state[n] = onPath
		for _, m := range edges[n] {
			visit(m)
		}
		state[n] = done
	}
	keys := make([]string, 0, len(edges))
	for k := range edges {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		visit(k)
	}
	return found
}

func emptyDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func containsFold(haystack, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if strings.EqualFold(t, want) {
			return true
		}
	}
	return false
}

func firstParagraph(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "\n\n"); i > 0 {
		s = s[:i]
	}
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 240 {
		s = s[:239] + "…"
	}
	return s
}
