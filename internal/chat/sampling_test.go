package chat

import (
	"testing"

	"github.com/jnk/deadlocker/internal/store"
)

func f(v float64) *float64 { return &v }
func i64(v int64) *int64   { return &v }

// An unconfigured setting must not appear in the request body at all: sending
// min_p: 0 is a real sampling change, not a no-op.
func TestExtraBodyParamsOmitsUnset(t *testing.T) {
	body, err := extraBodyParams(store.LLMConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(body) != 0 {
		t.Fatalf("expected an empty body, got %v", body)
	}
}

func TestExtraBodyParamsSetsNonStandardKnobs(t *testing.T) {
	body, err := extraBodyParams(store.LLMConfig{
		TopK:          i64(40),
		MinP:          f(0.05),
		RepeatPenalty: f(1.1),
		Seed:          i64(42),
		Effort:        "  high  ",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]any{
		// top_k is here rather than on the fantasy call: the OpenAI-compatible
		// provider drops it silently.
		"top_k":            int64(40),
		"min_p":            0.05,
		"repeat_penalty":   1.1,
		"seed":             int64(42),
		"reasoning_effort": "high",
	}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("%s = %#v, want %#v", k, body[k], v)
		}
	}
	if len(body) != len(want) {
		t.Errorf("got %d keys, want %d: %v", len(body), len(want), body)
	}
}

// The kwargs object is the escape hatch, so it has to be able to override a
// field that also has a control of its own.
func TestExtraBodyParamsKwargsOverride(t *testing.T) {
	body, err := extraBodyParams(store.LLMConfig{
		MinP:  f(0.05),
		Extra: `{"min_p": 0.2, "top_a": 0.1}`,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if body["min_p"] != 0.2 {
		t.Errorf("min_p = %#v, want 0.2", body["min_p"])
	}
	if body["top_a"] != 0.1 {
		t.Errorf("top_a = %#v, want 0.1", body["top_a"])
	}
}

func TestExtraBodyParamsRejectsMalformedKwargs(t *testing.T) {
	if _, err := extraBodyParams(store.LLMConfig{Extra: "{not json"}); err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
	// A bare array is valid JSON but cannot be merged into a request body.
	if _, err := extraBodyParams(store.LLMConfig{Extra: `["a"]`}); err == nil {
		t.Fatal("expected an error for a non-object")
	}
}
