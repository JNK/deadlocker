package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Drafts are scenarios that are being written and have not earned a file yet.
//
// The editor used to hold the only copy: pressing Run navigated to the run page
// and the text was gone, so the one thing you always want after watching a test
// run — to go back and change a line — was the one thing you could not do. A
// draft is that copy, kept where it survives the navigation, the tab and the
// restart.
//
// Deliberately unversioned. Scenario history exists so a working example is
// never lost to an edit, and it starts the moment a draft becomes a file; while
// it is still being written every keystroke would be a revision, which is not
// history, it is noise. Saving to the library is what makes a version.

// Draft is one unsaved scenario.
type Draft struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Source string `json:"source"`
	// ScenarioID and Path are set when the draft holds unsaved edits to a
	// scenario that already exists on disk, so the editor can offer them back
	// and write to the right file.
	ScenarioID string    `json:"scenario_id,omitempty"`
	Path       string    `json:"path,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// maxDrafts caps the drawer. Drafts are cheap, but a list nobody can find
// anything in is its own kind of lost; the oldest untouched ones go first.
const maxDrafts = 60

// UntitledDraft is what a draft with no readable name is called.
const UntitledDraft = "Untitled scenario"

var bucketDrafts = []byte("drafts")

// newDraftID returns a short random identifier. It is random rather than
// sequential so a draft URL cannot be guessed into a different draft.
func newDraftID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "d" + hex.EncodeToString(b[:]), nil
}

// SaveDraft creates or replaces a draft. An empty ID means "create"; the stored
// draft is returned with its ID and timestamps filled in.
func (s *Store) SaveDraft(d Draft) (Draft, error) {
	if strings.TrimSpace(d.Source) == "" {
		return Draft{}, fmt.Errorf("a draft needs some source")
	}
	if d.Name == "" {
		d.Name = UntitledDraft
	}
	err := s.update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(bucketDrafts)
		if err != nil {
			return err
		}
		now := time.Now()
		if d.ID == "" {
			id, err := newDraftID()
			if err != nil {
				return err
			}
			d.ID = id
		}
		// An existing draft keeps the moment it was started: "created 20 minutes
		// ago, touched just now" is the useful pair.
		if raw := b.Get([]byte(d.ID)); raw != nil {
			var prev Draft
			if json.Unmarshal(raw, &prev) == nil && !prev.CreatedAt.IsZero() {
				d.CreatedAt = prev.CreatedAt
			}
		}
		if d.CreatedAt.IsZero() {
			d.CreatedAt = now
		}
		d.UpdatedAt = now
		payload, err := json.Marshal(d)
		if err != nil {
			return err
		}
		if err := b.Put([]byte(d.ID), payload); err != nil {
			return err
		}
		return pruneDrafts(b)
	})
	return d, err
}

// pruneDrafts drops the least recently touched drafts once there are too many.
func pruneDrafts(b *bolt.Bucket) error {
	all, err := readDrafts(b)
	if err != nil || len(all) <= maxDrafts {
		return err
	}
	for _, d := range all[maxDrafts:] {
		if err := b.Delete([]byte(d.ID)); err != nil {
			return err
		}
	}
	return nil
}

// readDrafts returns every draft in a bucket, most recently touched first.
func readDrafts(b *bolt.Bucket) ([]Draft, error) {
	var out []Draft
	if b == nil {
		return nil, nil
	}
	err := b.ForEach(func(_, payload []byte) error {
		var d Draft
		if err := json.Unmarshal(payload, &d); err != nil {
			return nil // skip anything unreadable rather than failing the list
		}
		out = append(out, d)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, err
}

// Drafts lists drafts, most recently touched first.
func (s *Store) Drafts(limit int) ([]Draft, error) {
	var out []Draft
	err := s.view(func(tx *bolt.Tx) error {
		var err error
		out, err = readDrafts(tx.Bucket(bucketDrafts))
		return err
	})
	if err != nil {
		return nil, err
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Draft returns one draft.
func (s *Store) Draft(id string) (Draft, bool, error) {
	var (
		d     Draft
		found bool
	)
	err := s.view(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketDrafts)
		if b == nil {
			return nil
		}
		raw := b.Get([]byte(id))
		if raw == nil {
			return nil
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			return err
		}
		found = true
		return nil
	})
	return d, found, err
}

// DraftForScenario returns the draft holding unsaved edits to a saved scenario,
// if there is one. There is at most one: a second editor on the same file is
// editing the same pending change, not starting a different one.
func (s *Store) DraftForScenario(scenarioID string) (Draft, bool, error) {
	if scenarioID == "" {
		return Draft{}, false, nil
	}
	all, err := s.Drafts(0)
	if err != nil {
		return Draft{}, false, err
	}
	for _, d := range all {
		if d.ScenarioID == scenarioID {
			return d, true, nil
		}
	}
	return Draft{}, false, nil
}

// DeleteDraft removes a draft. Deleting one that is already gone is not an
// error: saving to the library discards the draft, and so does the button, and
// either can happen twice.
func (s *Store) DeleteDraft(id string) error {
	return s.update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketDrafts)
		if b == nil {
			return nil
		}
		return b.Delete([]byte(id))
	})
}
