package chat

import "testing"

func TestDescribeToolCall(t *testing.T) {
	cases := []struct {
		name, input, label, detail string
	}{
		{"set_draft", `{"yaml":"a\nb\nc"}`, "Updating the draft", "3 lines"},
		{"step_run", `{"run_id":"abc","count":3}`, "Stepping the run", "abc · 3 steps"},
		{"step_run", `{"run_id":"abc"}`, "Stepping the run", "abc"},
		{"start_run", `{"scenario_id":"gap"}`, "Starting a run", "gap"},
		{"list_scenarios", `{"query":"deadlock"}`, "Browsing the scenario library", "matching deadlock"},
		{"close_run", `{"run_id":"z9"}`, "Closing the run", "z9"},
	}
	for _, tc := range cases {
		label, detail := describeToolCall(tc.name, tc.input)
		if label != tc.label || detail != tc.detail {
			t.Errorf("describeToolCall(%s, %s) = (%q, %q), want (%q, %q)",
				tc.name, tc.input, label, detail, tc.label, tc.detail)
		}
	}
}

func TestDescribeToolResult(t *testing.T) {
	cases := []struct {
		name, raw, want string
		failed          bool
	}{
		{"list_scenarios", `{"total":17}`, "17 scenarios", false},
		{"list_scenarios", `{"total":1}`, "1 scenario", false},
		{"validate_scenario", `{"valid":true}`, "valid", false},
		{"validate_scenario", `{"valid":true,"warnings":["a","b"]}`, "valid, 2 warnings", false},
		{"validate_scenario", `{"valid":false,"error":"boom"}`, "invalid: boom", true},
		{"create_scenario", `{"path":"a/b.yaml"}`, "written to a/b.yaml", false},
		{"start_run", `{"run":{"run_id":"r1","total":6}}`, "run r1 ready · 6 steps", false},
		{"close_run", `{"closed":true}`, "run closed, scratch database dropped", false},
		{"step_run", `{"outcomes":[{"step":{"status":"blocked"}},{"step":{"status":"done"}}]}`,
			"2 steps · 1 blocked", false},
		{"get_locks", `{"locks":{"locks":[1,2,3],"waits":[1],"cycle":true}}`,
			"3 locks · 1 wait · CYCLE", false},
		{"anything", `{"error":"nope"}`, "nope", true},
	}
	for _, tc := range cases {
		got, failed := describeToolResult(tc.name, tc.raw)
		if got != tc.want || failed != tc.failed {
			t.Errorf("describeToolResult(%s, %s) = (%q, %v), want (%q, %v)",
				tc.name, tc.raw, got, failed, tc.want, tc.failed)
		}
	}
}

// Fantasy wraps tool output in an envelope; the summariser must see through it.
func TestToolResultUnwrapping(t *testing.T) {
	wrapped := `{"type":"text","data":{"text":"{\"total\":17}"}}`
	got, _ := describeToolResult("list_scenarios", unwrapForTest(wrapped))
	if got != "17 scenarios" {
		t.Errorf("wrapped result summarised as %q, want %q", got, "17 scenarios")
	}
}

// unwrapForTest mirrors what toolResultText does to a marshalled result.
func unwrapForTest(raw string) string {
	return unwrapEnvelope(raw)
}
