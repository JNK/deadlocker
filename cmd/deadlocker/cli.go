package main

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jnk/deadlocker/internal/casedef"
	"github.com/jnk/deadlocker/internal/dockerctl"
	"github.com/jnk/deadlocker/internal/engine"
	"github.com/jnk/deadlocker/internal/mysqlbox"
)

// The `run` subcommand plays scenarios without the UI.
//
// The point is not convenience — the UI is better for reading a run — but
// automation: a scenario is an executable claim about how MySQL behaves, and a
// claim that is never checked rots. This exits non-zero when any step's
// observed outcome disagrees with what the scenario declares, so a library of
// scenarios can be a CI job.
//
// It shares the whole engine with the server. What differs is only that nobody
// is watching, so it plays every step itself and waits out legitimate blocks.

const cliUsage = `Usage:
  deadlocker                          serve the web UI (default)
  deadlocker run [flags] [scenario…]  run scenarios and report

Run flags:
  -cases dir        scenario directory (default "cases")
  -format f         text, json or junit (default "text")
  -o file           write to a file instead of stdout
  -isolation level  override the isolation level for every scenario
  -settle d         how long a statement may run before it counts as blocked
  -timeout d        give up on a scenario after this long (default 5m)
  -keep-stale       do not remove containers left by a previous session

With no scenario named, every scenario in the directory runs. A scenario can be
named by id or by file path.

Examples:
  deadlocker run classic-ab-ba-deadlock
  deadlocker run -format junit -o results.xml
  deadlocker run -isolation "READ COMMITTED" uuidv7-missing-row-gap-lock
`

// stepOutcome is one step's result, flattened for reporting.
type stepOutcome struct {
	Index       int    `json:"index"`
	Actor       string `json:"actor"`
	Label       string `json:"label"`
	SQL         string `json:"sql"`
	Status      string `json:"status"`
	DurationMS  int64  `json:"duration_ms"`
	Expect      string `json:"expect,omitempty"`
	Actual      string `json:"actual,omitempty"`
	Verdict     string `json:"verdict,omitempty"`
	VerdictNote string `json:"verdict_note,omitempty"`
	Error       string `json:"error,omitempty"`
	WasBlocked  bool   `json:"was_blocked,omitempty"`
}

// scenarioOutcome is one scenario's result.
type scenarioOutcome struct {
	ID             string        `json:"id"`
	Name           string        `json:"name"`
	Path           string        `json:"path,omitempty"`
	Image          string        `json:"image,omitempty"`
	Isolation      string        `json:"isolation,omitempty"`
	Steps          []stepOutcome `json:"steps"`
	Mismatches     int           `json:"mismatches"`
	DurationMS     int64         `json:"duration_ms"`
	DeadlockReport string        `json:"deadlock_report,omitempty"`
	// Error is set when the scenario could not be played at all, which is
	// distinct from a step behaving unexpectedly.
	Error string `json:"error,omitempty"`
}

func (s scenarioOutcome) failed() bool { return s.Error != "" || s.Mismatches > 0 }

type cliReport struct {
	StartedAt time.Time         `json:"started_at"`
	Scenarios []scenarioOutcome `json:"scenarios"`
	Total     int               `json:"total"`
	Failed    int               `json:"failed"`
}

// runCLI is the `run` subcommand. args excludes the subcommand name itself.
func runCLI(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		casesDir  = fs.String("cases", "cases", "directory containing scenario YAML files")
		format    = fs.String("format", "text", "output format: text, json or junit")
		outPath   = fs.String("o", "", "write the report to this file instead of stdout")
		isolation = fs.String("isolation", "", "override the isolation level for every scenario")
		settle    = fs.Duration("settle", 400*time.Millisecond, "how long a statement may run before it is reported as blocked")
		timeout   = fs.Duration("timeout", 5*time.Minute, "give up on a scenario after this long")
		keepStale = fs.Bool("keep-stale", false, "do not remove containers left behind by a previous session")
		seed      = fs.Bool("seed", false, "copy the built-in example scenarios into the case directory first")
	)
	if err := fs.Parse(args); err != nil {
		fmt.Fprint(os.Stderr, cliUsage)
		return err
	}
	switch *format {
	case "text", "json", "junit":
	default:
		return fmt.Errorf("unknown format %q: want text, json or junit", *format)
	}

	absCases, err := filepath.Abs(*casesDir)
	if err != nil {
		return err
	}
	if *seed {
		if _, err := casedef.Seed(absCases); err != nil {
			return err
		}
	}

	lib := casedef.NewLibrary(absCases)
	if err := lib.Load(); err != nil {
		return fmt.Errorf("load cases: %w", err)
	}
	for path, problem := range lib.Broken() {
		fmt.Fprintf(os.Stderr, "skipping %s: %s\n", path, problem)
	}

	selected, err := selectCases(lib, fs.Args())
	if err != nil {
		return err
	}
	if len(selected) == 0 {
		return fmt.Errorf("no scenarios to run in %s", absCases)
	}

	// The report goes to stdout by default, so progress has to go to stderr —
	// otherwise `-format json` produces something no parser will accept.
	progress := os.Stderr

	if err := mysqlbox.SilenceExpectedDriverNoise(); err != nil {
		return err
	}
	docker, err := dockerctl.New()
	if err != nil {
		return err
	}
	pingCtx, cancelPing := context.WithTimeout(context.Background(), 10*time.Second)
	err = docker.Ping(pingCtx)
	cancelPing()
	if err != nil {
		return fmt.Errorf("cannot reach the Docker daemon: %w", err)
	}

	var mgr *engine.Manager
	pool := mysqlbox.NewPool(docker, func(image string, line dockerctl.LogLine) {
		if mgr != nil {
			mgr.OnDockerLog(image, line)
		}
	})
	mgr = engine.NewManager(pool, *settle)

	if !*keepStale {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if n, _ := pool.ReapStale(ctx); n > 0 {
			fmt.Fprintf(progress, "removed %d stale container(s)\n", n)
		}
		cancel()
	}
	defer func() {
		mgr.CloseAll()
		if err := pool.Close(); err != nil {
			fmt.Fprintf(progress, "container cleanup: %v\n", err)
		}
	}()

	report := cliReport{StartedAt: time.Now(), Total: len(selected)}
	for _, c := range selected {
		fmt.Fprintf(progress, "· %s\n", c.ID)
		out := playScenario(mgr, c, *isolation, *timeout)
		if out.failed() {
			report.Failed++
		}
		report.Scenarios = append(report.Scenarios, out)
	}

	w := io.Writer(os.Stdout)
	if *outPath != "" {
		f, err := os.Create(*outPath)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}
	if err := writeReport(w, *format, report); err != nil {
		return err
	}

	if report.Failed > 0 {
		// Non-zero so CI fails, but reported as a plain line rather than through
		// log.Fatalf: a scenario disagreeing with MySQL is a result, and the
		// report above already says which one and why.
		return &exitError{code: 1, msg: fmt.Sprintf("%d of %d scenario(s) did not behave as documented",
			report.Failed, report.Total)}
	}
	return nil
}

// exitError carries an exit code without an ugly log line.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

// selectCases resolves the positional arguments to cases, accepting either an
// id or a path so shell completion on the case directory is useful.
func selectCases(lib *casedef.Library, names []string) ([]*casedef.Case, error) {
	if len(names) == 0 {
		return lib.List(), nil
	}
	var out []*casedef.Case
	for _, name := range names {
		if c, ok := lib.Get(name); ok {
			out = append(out, c)
			continue
		}
		// Try it as a path, relative to the library root or to the caller.
		id := casedef.IDForPath(strings.TrimSuffix(filepath.Base(name), filepath.Ext(name)))
		if c, ok := lib.Get(id); ok {
			out = append(out, c)
			continue
		}
		return nil, fmt.Errorf("no scenario %q: run `deadlocker run` with no arguments to see the whole library", name)
	}
	return out, nil
}

// playScenario runs one scenario to the end, waiting out legitimate blocks.
func playScenario(mgr *engine.Manager, c *casedef.Case, isolation string, timeout time.Duration) scenarioOutcome {
	started := time.Now()
	out := scenarioOutcome{ID: c.ID, Name: c.Name, Path: c.Path, Image: c.MySQL.Image}

	// The override is applied to a copy: the file on disk is not the CLI's to
	// change, and the library is shared with whatever else is running.
	if isolation != "" {
		copied := *c
		copied.MySQL.Isolation = isolation
		c = &copied
	}
	out.Isolation = c.MySQL.Isolation

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	run, err := mgr.Start(ctx, c)
	if err != nil {
		out.Error = err.Error()
		out.DurationMS = time.Since(started).Milliseconds()
		return out
	}
	defer func() { _ = mgr.CloseRun(run.ID) }()

	if err := drive(ctx, run); err != nil {
		out.Error = err.Error()
	}

	// A statement that was blocked at the moment the last step was submitted
	// may still be resolving, and its verdict is not final until it has.
	settleAfterLastStep(ctx, run)

	for _, st := range run.Steps() {
		so := stepOutcome{
			Index: st.Index, Actor: st.Actor, Label: st.Label,
			SQL:        strings.Join(strings.Fields(st.SQL), " "),
			Status:     string(st.Status),
			DurationMS: st.DurationMS,
			Expect:     string(st.Expect), Actual: string(st.Actual),
			Verdict: st.Verdict, VerdictNote: st.VerdictNote,
			WasBlocked: st.WasBlocked,
		}
		if st.Error != nil {
			so.Error = st.Error.Message
		}
		if st.Verdict == "mismatch" {
			out.Mismatches++
		}
		out.Steps = append(out.Steps, so)
	}
	out.DeadlockReport = run.DeadlockReport()
	out.DurationMS = time.Since(started).Milliseconds()
	return out
}

// drive submits every step, waiting when the next one belongs to an actor whose
// previous statement is still on a lock. That wait is the scenario working as
// designed, not an error.
func drive(ctx context.Context, run *engine.Run) error {
	var blocked *engine.ActorBlockedError
	stalls := 0
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("timed out with %d step(s) left", run.State().Total-run.State().Cursor)
		}
		_, err := run.Step(ctx)
		switch {
		case err == nil:
			stalls = 0
		case errors.Is(err, engine.ErrNoMoreSteps):
			return nil
		case errors.As(err, &blocked):
			stalls++
			if stalls > 240 {
				return fmt.Errorf("stuck: %s", err)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(250 * time.Millisecond):
			}
		default:
			return err
		}
	}
}

// settleAfterLastStep gives a still-blocked final statement a moment to resolve,
// so its verdict reflects what actually happened rather than where it was when
// the loop ran out of steps.
func settleAfterLastStep(ctx context.Context, run *engine.Run) {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		anyBlocked := false
		for _, st := range run.Steps() {
			if st.Status == engine.StepBlocked {
				anyBlocked = true
				break
			}
		}
		if !anyBlocked {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// ---------------------------------------------------------------- reporting

func writeReport(w io.Writer, format string, r cliReport) error {
	switch format {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	case "junit":
		return writeJUnit(w, r)
	default:
		return writeText(w, r)
	}
}

func writeText(w io.Writer, r cliReport) error {
	for _, s := range r.Scenarios {
		fmt.Fprintf(w, "%s  %s\n", verdictMark(s), s.Name)
		fmt.Fprintf(w, "   %s", s.ID)
		if s.Isolation != "" {
			fmt.Fprintf(w, " · %s", s.Isolation)
		}
		fmt.Fprintf(w, " · %s\n", time.Duration(s.DurationMS)*time.Millisecond)

		if s.Error != "" {
			fmt.Fprintf(w, "   could not run: %s\n\n", s.Error)
			continue
		}
		for _, st := range s.Steps {
			mark := " "
			if st.Verdict == "mismatch" {
				mark = "!"
			}
			fmt.Fprintf(w, "   %s %2d %-6s %-8s %s\n",
				mark, st.Index, st.Actor, st.Status, truncate(st.Label, 48))
			if st.Verdict == "mismatch" {
				fmt.Fprintf(w, "        %s\n", st.VerdictNote)
			}
		}
		if s.DeadlockReport != "" {
			fmt.Fprintf(w, "   + InnoDB deadlock report captured\n")
		}
		fmt.Fprintln(w)
	}

	if r.Failed == 0 {
		fmt.Fprintf(w, "all %d scenario(s) behaved as documented\n", r.Total)
		return nil
	}
	fmt.Fprintf(w, "%d of %d scenario(s) did not behave as documented\n", r.Failed, r.Total)
	return nil
}

func verdictMark(s scenarioOutcome) string {
	if s.failed() {
		return "FAIL"
	}
	return "ok  "
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// JUnit XML, so a scenario library can be a CI job whose failures show up where
// every other test failure does. One testsuite per scenario, one testcase per
// step: a mismatch then points at the exact statement rather than at the file.
type junitSuites struct {
	XMLName  xml.Name     `xml:"testsuites"`
	Name     string       `xml:"name,attr"`
	Tests    int          `xml:"tests,attr"`
	Failures int          `xml:"failures,attr"`
	Time     string       `xml:"time,attr"`
	Suites   []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name     string      `xml:"name,attr"`
	Tests    int         `xml:"tests,attr"`
	Failures int         `xml:"failures,attr"`
	Errors   int         `xml:"errors,attr"`
	Time     string      `xml:"time,attr"`
	Cases    []junitCase `xml:"testcase"`
}

type junitCase struct {
	Name      string        `xml:"name,attr"`
	ClassName string        `xml:"classname,attr"`
	Time      string        `xml:"time,attr"`
	Failure   *junitFailure `xml:"failure,omitempty"`
	Error     *junitFailure `xml:"error,omitempty"`
	SystemOut string        `xml:"system-out,omitempty"`
}

type junitFailure struct {
	Message string `xml:"message,attr"`
	Type    string `xml:"type,attr"`
	Body    string `xml:",chardata"`
}

func secs(ms int64) string { return fmt.Sprintf("%.3f", float64(ms)/1000) }

func writeJUnit(w io.Writer, r cliReport) error {
	out := junitSuites{Name: "deadlocker"}
	var totalMS int64

	for _, s := range r.Scenarios {
		suite := junitSuite{Name: s.ID, Time: secs(s.DurationMS)}
		totalMS += s.DurationMS

		if s.Error != "" {
			suite.Tests, suite.Errors = 1, 1
			suite.Cases = append(suite.Cases, junitCase{
				Name: "run", ClassName: s.ID, Time: secs(s.DurationMS),
				Error: &junitFailure{Message: s.Error, Type: "run-failed", Body: s.Error},
			})
			out.Tests++
			out.Failures++
			out.Suites = append(out.Suites, suite)
			continue
		}

		for _, st := range s.Steps {
			tc := junitCase{
				Name:      fmt.Sprintf("%d %s: %s", st.Index, st.Actor, st.Label),
				ClassName: s.ID,
				Time:      secs(st.DurationMS),
				SystemOut: st.SQL,
			}
			if st.Verdict == "mismatch" {
				tc.Failure = &junitFailure{
					Message: st.VerdictNote,
					Type:    "expectation",
					Body: fmt.Sprintf("expected %s, observed %s\n\n%s",
						st.Expect, st.Actual, st.SQL),
				}
				suite.Failures++
				out.Failures++
			}
			suite.Tests++
			out.Tests++
			suite.Cases = append(suite.Cases, tc)
		}
		out.Suites = append(out.Suites, suite)
	}
	out.Time = secs(totalMS)

	if _, err := io.WriteString(w, xml.Header); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(out); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n")
	return err
}
