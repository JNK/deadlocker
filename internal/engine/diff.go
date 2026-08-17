package engine

import "fmt"

// FieldDiff is one difference between two runs, expressed in the terms the UI
// shows rather than as raw field names.
type FieldDiff struct {
	Label string `json:"label"`
	A     string `json:"a"`
	B     string `json:"b"`
}

// StepDiff pairs the same step index from two runs.
type StepDiff struct {
	Index int         `json:"index"`
	Label string      `json:"label"`
	A     *StepResult `json:"a,omitempty"`
	B     *StepResult `json:"b,omitempty"`
	// Differences is empty when the two steps behaved identically.
	Differences []FieldDiff `json:"differences,omitempty"`
}

func (d StepDiff) Differs() bool { return len(d.Differences) > 0 }

// DiffResult is a full comparison of two runs.
type DiffResult struct {
	A       *Record     `json:"a"`
	B       *Record     `json:"b"`
	Setup   []FieldDiff `json:"setup,omitempty"`
	Steps   []StepDiff  `json:"steps"`
	Changed int         `json:"changed"`
	// SameCase is false when the two runs came from different scenarios, in
	// which case aligning by step index is a rough approximation and the UI
	// says so.
	SameCase bool `json:"same_case"`
}

// Diff compares two run records, aligning steps by index.
//
// Aligning by index rather than by content is deliberate: the interesting
// comparison is "the same scenario under a different isolation level", where
// step N is the same statement in both runs and only its outcome moved.
func Diff(a, b *Record) DiffResult {
	res := DiffResult{A: a, B: b, SameCase: a.CaseID == b.CaseID}

	res.Setup = setupDiffs(a, b)

	n := len(a.Steps)
	if len(b.Steps) > n {
		n = len(b.Steps)
	}
	for i := 0; i < n; i++ {
		var sa, sb *StepResult
		if i < len(a.Steps) {
			sa = a.Steps[i]
		}
		if i < len(b.Steps) {
			sb = b.Steps[i]
		}

		sd := StepDiff{Index: i + 1}
		switch {
		case sa != nil:
			sd.Label = sa.Label
		case sb != nil:
			sd.Label = sb.Label
		}

		sd.A, sd.B = sa, sb
		sd.Differences = stepDiffs(sa, sb)
		if sd.Differs() {
			res.Changed++
		}
		res.Steps = append(res.Steps, sd)
	}
	return res
}

func setupDiffs(a, b *Record) []FieldDiff {
	var out []FieldDiff
	add := func(label, x, y string) {
		if x != y {
			out = append(out, FieldDiff{Label: label, A: x, B: y})
		}
	}
	add("Scenario", a.CaseName, b.CaseName)
	add("Image", a.Image, b.Image)
	add("Isolation", orDash(a.Isolation), orDash(b.Isolation))
	add("Lock wait timeout", fmt.Sprintf("%ds", a.LockWaitTimeout), fmt.Sprintf("%ds", b.LockWaitTimeout))
	add("Protocol", protocolName(a.Prepared), protocolName(b.Prepared))
	add("Steps submitted", fmt.Sprint(a.Submitted), fmt.Sprint(b.Submitted))
	add("Steps that blocked", fmt.Sprint(a.Blocked), fmt.Sprint(b.Blocked))
	add("Deadlocks", fmt.Sprint(a.Deadlocks), fmt.Sprint(b.Deadlocks))
	add("Lock wait timeouts", fmt.Sprint(a.Timeouts), fmt.Sprint(b.Timeouts))
	add("Other errors", fmt.Sprint(a.Errors), fmt.Sprint(b.Errors))
	add("Expectation mismatches", fmt.Sprint(a.Mismatches), fmt.Sprint(b.Mismatches))
	return out
}

func stepDiffs(a, b *StepResult) []FieldDiff {
	if a == nil && b == nil {
		return nil
	}
	if a == nil {
		return []FieldDiff{{Label: "Present", A: "not in this run", B: "step " + fmt.Sprint(b.Index)}}
	}
	if b == nil {
		return []FieldDiff{{Label: "Present", A: "step " + fmt.Sprint(a.Index), B: "not in this run"}}
	}

	var out []FieldDiff
	add := func(label, x, y string) {
		if x != y {
			out = append(out, FieldDiff{Label: label, A: x, B: y})
		}
	}

	add("Statement", normaliseSQL(a.SQL), normaliseSQL(b.SQL))
	add("Actor", a.Actor, b.Actor)
	add("Outcome", string(a.Status), string(b.Status))
	add("Hit a lock wait", yesNo(a.WasBlocked || a.Status == StepBlocked), yesNo(b.WasBlocked || b.Status == StepBlocked))
	add("Error", errorSummary(a.Error), errorSummary(b.Error))
	add("Verdict", orDash(a.Verdict), orDash(b.Verdict))
	add("Rows returned", fmt.Sprint(a.RowCount), fmt.Sprint(b.RowCount))
	add("Rows affected", fmt.Sprint(a.RowsAffected), fmt.Sprint(b.RowsAffected))
	add("Blocked by", joinOrDash(a.BlockedBy), joinOrDash(b.BlockedBy))

	// Timing is compared in coarse buckets. Millisecond jitter between runs is
	// noise; the difference between "instant" and "waited for a timeout" is not.
	add("Duration", durationBucket(a.DurationMS), durationBucket(b.DurationMS))
	return out
}

// durationBucket collapses a duration into a band, so a diff only fires when
// the timing changed in a way that means something.
func durationBucket(ms int64) string {
	switch {
	case ms < 50:
		return "instant (<50ms)"
	case ms < 500:
		return "fast (<500ms)"
	case ms < 2000:
		return "noticeable (<2s)"
	case ms < 10000:
		return "slow (<10s)"
	default:
		return "very slow (>10s)"
	}
}

func normaliseSQL(s string) string {
	return oneLineFields(s)
}

func oneLineFields(s string) string {
	out := make([]rune, 0, len(s))
	space := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			space = true
			continue
		}
		if space && len(out) > 0 {
			out = append(out, ' ')
		}
		space = false
		out = append(out, r)
	}
	return string(out)
}

func errorSummary(e *SQLError) string {
	if e == nil {
		return "none"
	}
	return fmt.Sprintf("%d %s", e.Code, e.Kind)
}

func protocolName(prepared bool) string {
	if prepared {
		return "binary (prepared statements)"
	}
	return "text (COM_QUERY)"
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func joinOrDash(items []string) string {
	if len(items) == 0 {
		return "—"
	}
	out := items[0]
	for _, s := range items[1:] {
		out += ", " + s
	}
	return out
}
