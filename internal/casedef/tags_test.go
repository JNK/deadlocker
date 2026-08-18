package casedef

import (
	"strings"
	"testing"
)

// The shipped library is the vocabulary's reference implementation. If a
// built-in scenario uses a tag that is not in it, either the tag is a typo or
// the vocabulary needs the entry — both worth being told about.
func TestBuiltInScenariosUseKnownTags(t *testing.T) {
	lib := embeddedLibrary(t)
	for _, c := range lib {
		if unknown := UnknownTags(c); len(unknown) > 0 {
			t.Errorf("%s: unknown tag(s) %s", c.Path, strings.Join(unknown, ", "))
		}
	}
}

// A vocabulary of tags used once each is a list, not a taxonomy. This does not
// forbid singletons — some behaviours genuinely appear once — but it does hold
// the line on how many.
func TestTagsAreMostlyShared(t *testing.T) {
	lib := embeddedLibrary(t)
	counts := map[string]int{}
	for _, c := range lib {
		for _, tag := range c.Tags {
			counts[tag]++
		}
	}

	singles := 0
	for _, n := range counts {
		if n == 1 {
			singles++
		}
	}
	if len(counts) == 0 {
		t.Fatal("no tags found")
	}
	// Fewer than half the vocabulary in use may be single-use.
	if singles*2 > len(counts) {
		t.Errorf("%d of %d tags are used only once; tags exist to group scenarios",
			singles, len(counts))
	}
}

func TestEveryScenarioIsTagged(t *testing.T) {
	for _, c := range embeddedLibrary(t) {
		if len(c.Tags) < 2 {
			t.Errorf("%s: %d tag(s); a scenario needs enough to be findable", c.Path, len(c.Tags))
		}
	}
}

// embeddedLibrary parses every scenario that ships in the binary.
func embeddedLibrary(t *testing.T) []*Case {
	t.Helper()
	dir := t.TempDir()
	if _, err := Seed(dir); err != nil {
		t.Fatalf("seed: %v", err)
	}
	lib := NewLibrary(dir)
	if err := lib.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	for path, problem := range lib.Broken() {
		t.Errorf("%s does not parse: %s", path, problem)
	}
	out := lib.List()
	if len(out) == 0 {
		t.Fatal("no scenarios embedded")
	}
	return out
}
