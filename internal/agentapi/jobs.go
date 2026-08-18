package agentapi

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jnk/deadlocker/internal/casedef"
	"github.com/jnk/deadlocker/internal/engine"
)

// Analyses that take many runs happen in the background: each one starts a real
// MySQL run per attempt, so they are measured in tens of seconds, not
// milliseconds.
const (
	JobMatrix  = "isolation-matrix"
	JobVersion = "version-matrix"
	JobShrink  = "shrink"
)

// Job is one background analysis.
type Job struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	ScenarioID string    `json:"scenario_id"`
	Name       string    `json:"name"`
	Status     string    `json:"status"` // running | done | failed
	Progress   string    `json:"progress"`
	Started    time.Time `json:"started"`
	Ended      time.Time `json:"ended,omitempty"`
	Error      string    `json:"error,omitempty"`

	Matrix *MatrixResult `json:"matrix,omitempty"`
	Shrink *ShrinkResult `json:"shrink,omitempty"`
}

// Jobs is a small in-memory registry of background analyses.
type Jobs struct {
	mu    sync.RWMutex
	jobs  map[string]*Job
	order []string
}

func NewJobs() *Jobs { return &Jobs{jobs: map[string]*Job{}} }

func (j *Jobs) put(job *Job) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, ok := j.jobs[job.ID]; !ok {
		j.order = append(j.order, job.ID)
		if len(j.order) > 40 {
			delete(j.jobs, j.order[0])
			j.order = j.order[1:]
		}
	}
	j.jobs[job.ID] = job
}

// All returns every job, newest first.
func (j *Jobs) All() []*Job {
	j.mu.RLock()
	defer j.mu.RUnlock()
	out := make([]*Job, 0, len(j.order))
	for i := len(j.order) - 1; i >= 0; i-- {
		if job, ok := j.jobs[j.order[i]]; ok {
			copied := *job
			out = append(out, &copied)
		}
	}
	return out
}

// Jobs exposes the registry for listings.
func (a *API) Jobs() *Jobs { return a.jobs }

func (j *Jobs) Get(id string) (*Job, bool) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	job, ok := j.jobs[id]
	if !ok {
		return nil, false
	}
	copied := *job
	return &copied, true
}

func (j *Jobs) update(id string, fn func(*Job)) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if job, ok := j.jobs[id]; ok {
		fn(job)
	}
}

// ------------------------------------------------------------ driving a run

// playAll advances a run to the end, waiting out actors that are legitimately
// blocked. It is the same loop a person performs by pressing Step repeatedly.
func playAll(ctx context.Context, run *engine.Run, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	stalls := 0
	for {
		if time.Now().After(deadline) {
			return errors.New("the scenario did not finish within its time budget")
		}
		_, err := run.Step(ctx)
		if err == nil {
			stalls = 0
			continue
		}
		if errors.Is(err, engine.ErrNoMoreSteps) {
			return nil
		}
		var blocked *engine.ActorBlockedError
		if errors.As(err, &blocked) {
			// The next step belongs to an actor still waiting on a lock. Give
			// the server a moment; a scenario that never releases is a wedge,
			// which the stall cap catches.
			stalls++
			if stalls > 80 {
				return nil // wedged: report what did happen rather than failing
			}
			select {
			case <-time.After(250 * time.Millisecond):
			case <-ctx.Done():
				return ctx.Err()
			}
			continue
		}
		return err
	}
}

// outcomeOfRun classifies what a finished run did, which is what both analyses
// compare against.
func outcomeOfRun(run *engine.Run) (deadlocks, timeouts, blocked int) {
	for _, s := range run.Steps() {
		if s.WasBlocked || s.Status == engine.StepBlocked {
			blocked++
		}
		if s.Error != nil {
			switch s.Error.Kind {
			case engine.ErrKindDeadlock:
				deadlocks++
			case engine.ErrKindTimeout:
				timeouts++
			}
		}
	}
	return
}

// ------------------------------------------------------- isolation matrix

type MatrixCell struct {
	Status  string `json:"status"`
	Verdict string `json:"verdict,omitempty"`
	Error   string `json:"error,omitempty"`
	Blocked bool   `json:"blocked,omitempty"`
	// Outcome is the single word to compare and display. A step that waited on
	// a lock and then completed still ends "done", so final status alone hides
	// exactly the difference this matrix exists to show.
	Outcome string `json:"outcome"`
}

// outcomeOfCell reduces a step to the word that matters for comparison.
func outcomeOfCell(c MatrixCell) string {
	switch {
	case c.Error == string(engine.ErrKindDeadlock):
		return "deadlock"
	case c.Error == string(engine.ErrKindTimeout):
		return "timeout"
	case c.Error != "":
		return "error"
	case c.Blocked:
		return "blocked"
	case c.Status == string(engine.StepPending):
		return "not reached"
	default:
		return "ok"
	}
}

type MatrixColumn struct {
	// Label is what the column is headed with -- an isolation level, or a MySQL
	// image. Isolation is kept alongside it because it is also a fact about the
	// run, not only a heading.
	Label     string       `json:"label"`
	Isolation string       `json:"isolation"`
	Image     string       `json:"image,omitempty"`
	Cells     []MatrixCell `json:"cells"`
	Deadlocks int          `json:"deadlocks"`
	Timeouts  int          `json:"timeouts"`
	Blocked   int          `json:"blocked"`
	Error     string       `json:"error,omitempty"`
	RunID     string       `json:"run_id,omitempty"`
}

type MatrixResult struct {
	ScenarioID string `json:"scenario_id"`
	Name       string `json:"name"`
	// Axis names what the columns vary: "isolation" or "version".
	Axis       string         `json:"axis"`
	AxisLabel  string         `json:"axis_label"`
	StepLabels []string       `json:"step_labels"`
	StepActors []string       `json:"step_actors"`
	Columns    []MatrixColumn `json:"columns"`
	// Summary states the finding in one line.
	Summary string `json:"summary"`
}

// IsolationLevels is the order the matrix presents, weakest to strongest.
var IsolationLevels = []string{
	"READ UNCOMMITTED", "READ COMMITTED", "REPEATABLE READ", "SERIALIZABLE",
}

// DefaultVersions is the version sweep. 5.7 is included because it is still
// widely deployed and its locking genuinely differs; 8.0 and 8.4 bracket the
// current line.
var DefaultVersions = []string{"mysql:5.7", "mysql:8.0", "mysql:8.4"}

type IsolationMatrixInput struct {
	ScenarioID string `json:"scenario_id,omitempty" jsonschema:"the scenario to sweep"`
	YAML       string `json:"yaml,omitempty" jsonschema:"ad-hoc YAML instead of a saved scenario"`
}

type JobStartedOutput struct {
	JobID  string `json:"job_id"`
	Status string `json:"status"`
	Note   string `json:"note"`
}

// StartIsolationMatrix runs the same scenario at every isolation level and
// reports where the outcomes diverge. It answers "what would READ COMMITTED do
// here" by doing it rather than reasoning about it.
func (a *API) StartIsolationMatrix(ctx context.Context, in IsolationMatrixInput) (JobStartedOutput, error) {
	base, err := a.caseFor(in.ScenarioID, in.YAML)
	if err != nil {
		return JobStartedOutput{}, err
	}
	id, err := newJobID()
	if err != nil {
		return JobStartedOutput{}, err
	}

	job := &Job{
		ID: id, Kind: JobMatrix, ScenarioID: base.ID, Name: base.Name,
		Status: "running", Started: time.Now(),
		Progress: "starting",
	}
	a.jobs.put(job)

	go a.runMatrix(job, base)

	return JobStartedOutput{
		JobID: id, Status: "running",
		Note: "Runs the scenario once per isolation level. Poll get_job with this id; it takes tens of seconds.",
	}, nil
}

type VersionMatrixInput struct {
	ScenarioID string   `json:"scenario_id,omitempty" jsonschema:"the scenario to sweep"`
	YAML       string   `json:"yaml,omitempty" jsonschema:"ad-hoc YAML instead of a saved scenario"`
	Images     []string `json:"images,omitempty" jsonschema:"container images to compare; defaults to mysql:5.7, mysql:8.0 and mysql:8.4"`
}

// StartVersionMatrix runs the same scenario against several MySQL versions.
//
// Locking behaviour is not fixed across releases -- 8.0 changed how SELECT
// COUNT(*) and several optimiser paths lock, and 5.7 predates NOWAIT and SKIP
// LOCKED entirely -- so "does this still behave the same on the version we
// actually run" is a question worth answering by running it.
//
// It is slower than the isolation sweep: each new image is a pull.
func (a *API) StartVersionMatrix(ctx context.Context, in VersionMatrixInput) (JobStartedOutput, error) {
	base, err := a.caseFor(in.ScenarioID, in.YAML)
	if err != nil {
		return JobStartedOutput{}, err
	}
	images := in.Images
	if len(images) == 0 {
		images = DefaultVersions
	}
	id, err := newJobID()
	if err != nil {
		return JobStartedOutput{}, err
	}

	job := &Job{
		ID: id, Kind: JobVersion, ScenarioID: base.ID, Name: base.Name,
		Status: "running", Started: time.Now(),
		Progress: "starting",
	}
	a.jobs.put(job)

	go a.runVersionMatrix(job, base, images)

	return JobStartedOutput{
		JobID: id, Status: "running",
		Note: "Runs the scenario once per MySQL version. Poll get_job with this id; " +
			"the first run of an image includes a pull, so this can take minutes.",
	}, nil
}

func (a *API) runMatrix(job *Job, base *casedef.Case) {
	a.runSweep(job, base, "isolation", "isolation level", IsolationLevels,
		func(c *casedef.Case, level string) { c.MySQL.Isolation = level })
}

func (a *API) runVersionMatrix(job *Job, base *casedef.Case, images []string) {
	a.runSweep(job, base, "version", "MySQL version", images,
		func(c *casedef.Case, image string) { c.MySQL.Image = image })
}

// runSweep runs the scenario once per value along one axis and records where
// the outcomes diverge. Isolation level and server version are the two axes
// worth sweeping, and the only difference between them is which field of the
// case a variant sets.
func (a *API) runSweep(
	job *Job, base *casedef.Case,
	axis, axisLabel string, values []string,
	apply func(*casedef.Case, string),
) {
	ctx := WithSource(context.Background(), SourceUI)
	res := &MatrixResult{
		ScenarioID: base.ID, Name: base.Name,
		Axis: axis, AxisLabel: axisLabel,
	}
	for _, s := range base.Steps {
		res.StepLabels = append(res.StepLabels, s.Label)
		res.StepActors = append(res.StepActors, s.Actor)
	}

	for i, value := range values {
		a.jobs.update(job.ID, func(j *Job) {
			j.Progress = fmt.Sprintf("running %s (%d of %d)", value, i+1, len(values))
		})

		variant := cloneCase(base)
		apply(variant, value)
		variant.Ephemeral = true

		col := MatrixColumn{
			Label: value, Isolation: variant.MySQL.Isolation, Image: variant.MySQL.Image,
		}

		run, err := a.mgr.Start(ctx, variant)
		if err != nil {
			col.Error = err.Error()
			res.Columns = append(res.Columns, col)
			continue
		}
		col.RunID = run.ID

		if err := playAll(ctx, run, 3*time.Minute); err != nil {
			col.Error = err.Error()
		}
		for _, s := range run.Steps() {
			cell := MatrixCell{
				Status: string(s.Status), Verdict: s.Verdict,
				Blocked: s.WasBlocked || s.Status == engine.StepBlocked,
			}
			if s.Error != nil {
				cell.Error = s.Error.Kind
			}
			cell.Outcome = outcomeOfCell(cell)
			col.Cells = append(col.Cells, cell)
		}
		col.Deadlocks, col.Timeouts, col.Blocked = outcomeOfRun(run)
		_ = a.mgr.CloseRun(run.ID)

		res.Columns = append(res.Columns, col)
	}

	res.Summary = summariseMatrix(res)
	a.jobs.update(job.ID, func(j *Job) {
		j.Status = "done"
		j.Progress = "finished"
		j.Ended = time.Now()
		j.Matrix = res
	})
	a.note(ctx, Activity{
		Kind: KindAnalysis, Tool: job.Kind, ScenarioID: base.ID,
		Summary: axisLabel + " matrix finished: " + res.Summary,
	})
}

// plural picks a form by count.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// summariseMatrix states the interesting part: whether the swept axis changes
// anything, and where.
func summariseMatrix(res *MatrixResult) string {
	var differing []string
	for i := range res.StepLabels {
		seen := map[string]bool{}
		for _, col := range res.Columns {
			if i < len(col.Cells) {
				seen[col.Cells[i].Outcome] = true
			}
		}
		if len(seen) > 1 {
			// Name the step and how it splits, which is the finding.
			var outcomes []string
			for _, col := range res.Columns {
				if i < len(col.Cells) {
					outcomes = append(outcomes, col.Label+": "+col.Cells[i].Outcome)
				}
			}
			differing = append(differing, fmt.Sprintf("step %d (%s)", i+1, strings.Join(outcomes, ", ")))
		}
	}
	// A column that could not run is not evidence of agreement. Saying "every
	// column behaves identically" when one of them never executed would be the
	// most misleading thing this summary could do.
	var failed []string
	for _, col := range res.Columns {
		if col.Error != "" || len(col.Cells) == 0 {
			failed = append(failed, col.Label)
		}
	}
	suffix := ""
	if len(failed) > 0 {
		suffix = fmt.Sprintf(" (%s did not run, so %s not compared)",
			strings.Join(failed, ", "), plural(len(failed), "it was", "they were"))
	}

	if len(differing) == 0 {
		if len(failed) == len(res.Columns) {
			return "nothing could be compared: no column ran"
		}
		return "the " + res.AxisLabel + " changes nothing here; every column that ran behaves identically" + suffix
	}
	return "outcomes differ at " + strings.Join(differing, "; ") + suffix
}

// ------------------------------------------------------------- shrink

// ShrinkStep is one step of the original scenario, marked with whether the
// reduction kept it. Rendering the original with the drops marked says more
// than showing only the survivors.
type ShrinkStep struct {
	Index  int    `json:"index"`
	Actor  string `json:"actor"`
	Accent string `json:"accent"`
	Label  string `json:"label"`
	SQL    string `json:"sql"`
	Expect string `json:"expect,omitempty"`
	Kept   bool   `json:"kept"`
}

type ShrinkResult struct {
	ScenarioID string `json:"scenario_id"`
	Name       string `json:"name"`
	Target     string `json:"target"`
	Original   int    `json:"original_steps"`
	Minimal    int    `json:"minimal_steps"`
	// RemovedLabels names what turned out to be unnecessary.
	RemovedLabels []string     `json:"removed_labels,omitempty"`
	Steps         []ShrinkStep `json:"steps,omitempty"`
	YAML          string       `json:"yaml,omitempty"`
	Attempts      int          `json:"attempts"`
	Note          string       `json:"note,omitempty"`
}

type ShrinkInput struct {
	ScenarioID string `json:"scenario_id,omitempty" jsonschema:"the scenario to reduce"`
	YAML       string `json:"yaml,omitempty" jsonschema:"ad-hoc YAML instead of a saved scenario"`
	Target     string `json:"target,omitempty" jsonschema:"what to preserve: deadlock, timeout or blocks; inferred from a first run when omitted"`
}

// StartShrink reduces a scenario to the fewest steps that still reproduce its
// interesting outcome. It is delta debugging against a real server: each
// candidate is actually run.
func (a *API) StartShrink(ctx context.Context, in ShrinkInput) (JobStartedOutput, error) {
	base, err := a.caseFor(in.ScenarioID, in.YAML)
	if err != nil {
		return JobStartedOutput{}, err
	}
	id, err := newJobID()
	if err != nil {
		return JobStartedOutput{}, err
	}

	job := &Job{
		ID: id, Kind: JobShrink, ScenarioID: base.ID, Name: base.Name,
		Status: "running", Started: time.Now(), Progress: "measuring the original",
	}
	a.jobs.put(job)

	go a.runShrink(job, base, strings.ToLower(strings.TrimSpace(in.Target)))

	return JobStartedOutput{
		JobID: id, Status: "running",
		Note: "Runs the scenario repeatedly, dropping steps that turn out not to matter. Poll get_job with this id.",
	}, nil
}

// reproduces reports whether a candidate still shows the target behaviour.
func (a *API) reproduces(ctx context.Context, c *casedef.Case, target string) (bool, error) {
	c = cloneCase(c)
	c.Ephemeral = true
	run, err := a.mgr.Start(ctx, c)
	if err != nil {
		return false, err
	}
	defer a.mgr.CloseRun(run.ID)

	if err := playAll(ctx, run, 2*time.Minute); err != nil {
		return false, nil
	}
	deadlocks, timeouts, blocked := outcomeOfRun(run)
	switch target {
	case "deadlock":
		return deadlocks > 0, nil
	case "timeout":
		return timeouts > 0, nil
	default:
		return blocked > 0, nil
	}
}

func (a *API) runShrink(job *Job, base *casedef.Case, target string) {
	ctx := WithSource(context.Background(), SourceUI)
	res := &ShrinkResult{ScenarioID: base.ID, Name: base.Name, Original: len(base.Steps)}

	// Establish what the scenario does, so there is something to preserve.
	if target == "" {
		probe := cloneCase(base)
		probe.Ephemeral = true
		run, err := a.mgr.Start(ctx, probe)
		if err != nil {
			a.failJob(job, err)
			return
		}
		_ = playAll(ctx, run, 3*time.Minute)
		deadlocks, timeouts, blocked := outcomeOfRun(run)
		_ = a.mgr.CloseRun(run.ID)

		switch {
		case deadlocks > 0:
			target = "deadlock"
		case timeouts > 0:
			target = "timeout"
		case blocked > 0:
			target = "blocks"
		default:
			a.jobs.update(job.ID, func(j *Job) {
				j.Status = "done"
				j.Ended = time.Now()
				j.Progress = "nothing to reduce"
				j.Shrink = &ShrinkResult{
					ScenarioID: base.ID, Name: base.Name, Original: len(base.Steps),
					Minimal: len(base.Steps),
					Note:    "This scenario neither blocks nor deadlocks, so there is no behaviour to preserve while removing steps.",
				}
			})
			return
		}
	}
	res.Target = target

	current := cloneCase(base)
	// kept maps each surviving step back to its position in the original, so
	// the result can mark the drops against the scenario the user knows.
	kept := make([]int, len(base.Steps))
	for i := range kept {
		kept[i] = i
	}
	attempts := 0
	const maxAttempts = 80

	// Greedy from the end: a later step is more likely to be incidental, and
	// removing it keeps the earlier setup intact.
	changed := true
	for changed && attempts < maxAttempts {
		changed = false
		for i := len(current.Steps) - 1; i >= 0 && attempts < maxAttempts; i-- {
			candidate := cloneCase(current)
			candidate.Steps = append(append([]casedef.Step{}, current.Steps[:i]...), current.Steps[i+1:]...)
			candidateKept := append(append([]int{}, kept[:i]...), kept[i+1:]...)
			if len(candidate.Steps) == 0 {
				continue
			}
			if err := candidate.Validate(); err != nil {
				continue
			}

			attempts++
			a.jobs.update(job.ID, func(j *Job) {
				j.Progress = fmt.Sprintf("attempt %d: %d step(s) remaining", attempts, len(candidate.Steps))
			})

			ok, err := a.reproduces(ctx, candidate, target)
			if err != nil {
				continue
			}
			if ok {
				res.RemovedLabels = append(res.RemovedLabels, current.Steps[i].Label)
				current = candidate
				kept = candidateKept
				changed = true
			}
		}
	}

	res.Minimal = len(current.Steps)
	res.Attempts = attempts

	// Render the original sequence with the drops marked.
	keptSet := map[int]bool{}
	for _, idx := range kept {
		keptSet[idx] = true
	}
	accents := map[string]string{}
	for _, a := range base.Actors {
		accents[a.ID] = a.Accent
	}
	for i, step := range base.Steps {
		res.Steps = append(res.Steps, ShrinkStep{
			Index: i + 1, Actor: step.Actor, Accent: accents[step.Actor],
			Label: step.Label, SQL: strings.TrimSpace(step.SQL),
			Expect: string(step.Expect), Kept: keptSet[i],
		})
	}
	// The reduction must not claim the original's identity: an explicit `id`
	// would collide with the scenario it came from when saved alongside it.
	// Without one the id is derived from whatever file it lands in.
	forFile := cloneCase(current)
	forFile.ID = ""
	forFile.Path = ""
	if yamlBytes, err := casedef.Marshal(forFile); err == nil {
		res.YAML = string(yamlBytes)
	}
	if res.Minimal == res.Original {
		res.Note = "Every step turned out to be necessary: removing any one of them stops the " +
			target + " from happening."
	} else {
		res.Note = fmt.Sprintf("Reduced from %d steps to %d while still producing a %s.",
			res.Original, res.Minimal, target)
	}
	sort.Strings(res.RemovedLabels)

	a.jobs.update(job.ID, func(j *Job) {
		j.Status = "done"
		j.Progress = "finished"
		j.Ended = time.Now()
		j.Shrink = res
	})
	a.note(ctx, Activity{
		Kind: KindAnalysis, Tool: "shrink_scenario", ScenarioID: base.ID,
		Summary: res.Note,
	})
}

func (a *API) failJob(job *Job, err error) {
	a.jobs.update(job.ID, func(j *Job) {
		j.Status = "failed"
		j.Ended = time.Now()
		j.Error = err.Error()
	})
}

// ------------------------------------------------------------------ shared

type GetJobInput struct {
	JobID string `json:"job_id"`
}

type GetJobOutput struct {
	Job *Job `json:"job"`
}

// GetJob returns a background analysis, finished or still running.
func (a *API) GetJob(ctx context.Context, in GetJobInput) (GetJobOutput, error) {
	job, ok := a.jobs.Get(in.JobID)
	if !ok {
		return GetJobOutput{}, fmt.Errorf("unknown job %q", in.JobID)
	}
	return GetJobOutput{Job: job}, nil
}

// caseFor resolves either a saved scenario or ad-hoc YAML.
func (a *API) caseFor(scenarioID, yaml string) (*casedef.Case, error) {
	if strings.TrimSpace(yaml) != "" {
		parsed, err := casedef.Parse([]byte(yaml))
		if err != nil {
			return nil, fmt.Errorf("scenario is not valid: %w", err)
		}
		if parsed.ID == "" {
			parsed.ID = "draft"
		}
		return parsed, nil
	}
	if scenarioID == "" {
		return nil, errors.New("provide either scenario_id or yaml")
	}
	return a.mustCase(scenarioID)
}

// cloneCase copies a scenario deeply enough that a variant cannot disturb the
// original.
func cloneCase(c *casedef.Case) *casedef.Case {
	out := *c
	out.Steps = append([]casedef.Step(nil), c.Steps...)
	out.Actors = append([]casedef.Actor(nil), c.Actors...)
	out.Schema = append([]string(nil), c.Schema...)
	out.Seed = append([]string(nil), c.Seed...)
	out.Tags = append([]string(nil), c.Tags...)
	if c.MySQL.Vars != nil {
		out.MySQL.Vars = map[string]string{}
		for k, v := range c.MySQL.Vars {
			out.MySQL.Vars[k] = v
		}
	}
	return &out
}

func newJobID() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b, err := randomBytes(10)
	if err != nil {
		return "", err
	}
	out := make([]byte, len(b))
	for i, v := range b {
		out[i] = alphabet[int(v)%len(alphabet)]
	}
	return string(out), nil
}

// ApplyShrinkInput turns a finished reduction into a scenario on disk.
type ApplyShrinkInput struct {
	JobID string `json:"job_id"`
	// Mode is "replace" to overwrite the scenario it came from, or "new" to
	// save it as a separate scenario.
	Mode string `json:"mode"`
	Path string `json:"path,omitempty" jsonschema:"file path when saving as new; derived from the original when omitted"`
}

// ApplyShrink writes a minimal reproduction into the library. The YAML comes
// from the stored job rather than from the caller, so what is saved is exactly
// what was verified.
func (a *API) ApplyShrink(ctx context.Context, in ApplyShrinkInput) (SaveScenarioOutput, error) {
	job, ok := a.jobs.Get(in.JobID)
	if !ok {
		return SaveScenarioOutput{}, fmt.Errorf("unknown job %q", in.JobID)
	}
	if job.Shrink == nil || strings.TrimSpace(job.Shrink.YAML) == "" {
		return SaveScenarioOutput{}, errors.New("that job has no reduced scenario to save")
	}

	if in.Mode == "replace" {
		if job.ScenarioID == "" {
			return SaveScenarioOutput{}, errors.New("this reduction came from ad-hoc YAML, so there is no scenario to replace")
		}
		return a.UpdateScenario(ctx, UpdateScenarioInput{ID: job.ScenarioID, YAML: job.Shrink.YAML})
	}

	path := in.Path
	if path == "" {
		if existing, err := a.mustCase(job.ScenarioID); err == nil {
			path = strings.TrimSuffix(existing.Path, ".yaml") + "-minimal.yaml"
		}
	}

	// Saved beside the original, it needs a name of its own: two scenarios
	// sharing a title is confusing in every listing.
	yaml := job.Shrink.YAML
	if parsed, err := casedef.Parse([]byte(yaml)); err == nil {
		if !strings.Contains(strings.ToLower(parsed.Name), "minimal") {
			parsed.Name += " (minimal repro)"
			parsed.ID = ""
			parsed.Path = ""
			if out, mErr := casedef.Marshal(parsed); mErr == nil {
				yaml = string(out)
			}
		}
	}
	return a.CreateScenario(ctx, CreateScenarioInput{YAML: yaml, Path: path})
}
