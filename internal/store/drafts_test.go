package store

import (
	"path/filepath"
	"testing"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return s
}

func TestSaveDraftCreatesThenUpdates(t *testing.T) {
	s := testStore(t)

	first, err := s.SaveDraft(Draft{Name: "Gap locks", Source: "name: gap\n"})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if first.ID == "" {
		t.Fatal("a new draft should be given an id")
	}
	if first.CreatedAt.IsZero() || first.UpdatedAt.IsZero() {
		t.Fatal("a new draft should be stamped")
	}

	second, err := s.SaveDraft(Draft{ID: first.ID, Name: "Gap locks", Source: "name: gap locks\n"})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("updating made a second draft: %q then %q", first.ID, second.ID)
	}
	// The moment it was started is what makes a draft findable later; only the
	// touch time moves.
	if !second.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("created time changed on update: %v then %v", first.CreatedAt, second.CreatedAt)
	}

	all, err := s.Drafts(0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected one draft, got %d", len(all))
	}
	if all[0].Source != "name: gap locks\n" {
		t.Errorf("the update was not stored: %q", all[0].Source)
	}
}

func TestDraftRoundTripAndDelete(t *testing.T) {
	s := testStore(t)
	saved, err := s.SaveDraft(Draft{Source: "name: x\n"})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if saved.Name != UntitledDraft {
		t.Errorf("a nameless draft should read as %q, got %q", UntitledDraft, saved.Name)
	}

	got, ok, err := s.Draft(saved.ID)
	if err != nil || !ok {
		t.Fatalf("read back: ok=%v err=%v", ok, err)
	}
	if got.Source != "name: x\n" {
		t.Errorf("source came back as %q", got.Source)
	}

	if err := s.DeleteDraft(saved.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok, _ := s.Draft(saved.ID); ok {
		t.Error("the draft survived deletion")
	}
	// Discarding is offered in two places and can land twice.
	if err := s.DeleteDraft(saved.ID); err != nil {
		t.Errorf("deleting twice should be harmless, got %v", err)
	}
}

func TestDraftForScenario(t *testing.T) {
	s := testStore(t)
	if _, ok, err := s.DraftForScenario("nothing"); ok || err != nil {
		t.Fatalf("expected no draft, got ok=%v err=%v", ok, err)
	}
	if _, err := s.SaveDraft(Draft{Source: "an older attempt", ScenarioID: "gap-locks"}); err != nil {
		t.Fatalf("save the older draft: %v", err)
	}
	saved, err := s.SaveDraft(Draft{Source: "name: edit\n", ScenarioID: "gap-locks", Path: "02/gap.yaml"})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	got, ok, err := s.DraftForScenario("gap-locks")
	if err != nil || !ok {
		t.Fatalf("expected the scenario's draft: ok=%v err=%v", ok, err)
	}
	// Two editors on the same file are editing the same pending change, so the
	// most recently touched draft is the one to offer back.
	if got.ID != saved.ID {
		t.Errorf("found draft %q, expected the newest one %q", got.ID, saved.ID)
	}
}

func TestEmptyDraftIsRefused(t *testing.T) {
	s := testStore(t)
	if _, err := s.SaveDraft(Draft{Source: "  \n"}); err == nil {
		t.Error("an empty draft should not be stored")
	}
}
