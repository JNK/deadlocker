package store

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Scenarios are versioned for the same reason the configuration is: a scenario
// that used to reproduce a lock is a working example, and editing it — by hand,
// from the assistant, or from an MCP client — should never be the thing that
// loses it. Every write appends a revision; restoring copies an old one forward
// rather than truncating history.
//
// The YAML file on disk stays the source of truth. This is a record of what it
// used to say, not a second place to read the current scenario from.

// ScenarioVersion is one saved revision of a scenario's YAML.
type ScenarioVersion struct {
	ScenarioID string    `json:"scenario_id"`
	Version    uint64    `json:"version"`
	SavedAt    time.Time `json:"saved_at"`
	Note       string    `json:"note,omitempty"`
	// Name and Path are stored alongside the source so a revision still reads
	// sensibly after the scenario has been renamed or moved.
	Name   string `json:"name,omitempty"`
	Path   string `json:"path,omitempty"`
	Source string `json:"source"`
	// IsCurrent marks the revision whose source matches the file on disk.
	IsCurrent bool `json:"is_current"`
}

// scenarioBucket returns the per-scenario sub-bucket, creating it when write is
// set. A missing bucket is not an error on read: it just means no revision has
// been recorded yet.
func scenarioBucket(tx *bolt.Tx, id string, write bool) (*bolt.Bucket, error) {
	root := tx.Bucket(bucketScenarios)
	if root == nil {
		if !write {
			return nil, nil
		}
		var err error
		if root, err = tx.CreateBucketIfNotExists(bucketScenarios); err != nil {
			return nil, err
		}
	}
	if write {
		return root.CreateBucketIfNotExists([]byte(id))
	}
	return root.Bucket([]byte(id)), nil
}

func latestIn(b *bolt.Bucket) (ScenarioVersion, bool) {
	if b == nil {
		return ScenarioVersion{}, false
	}
	k, payload := b.Cursor().Last()
	if k == nil {
		return ScenarioVersion{}, false
	}
	var v ScenarioVersion
	if err := json.Unmarshal(payload, &v); err != nil {
		return ScenarioVersion{}, false
	}
	return v, true
}

// RecordScenario appends a revision of a scenario's source.
//
// A write whose source is identical to the newest revision is dropped, so
// re-saving an unchanged file — which the editor does readily — does not fill
// the history with duplicates. The returned bool reports whether a new revision
// was actually written.
func (s *Store) RecordScenario(id, name, path, source, note string) (ScenarioVersion, bool, error) {
	if id == "" {
		return ScenarioVersion{}, false, fmt.Errorf("a scenario id is required")
	}
	var (
		saved   ScenarioVersion
		written bool
	)
	err := s.update(func(tx *bolt.Tx) error {
		b, err := scenarioBucket(tx, id, true)
		if err != nil {
			return err
		}
		next := uint64(1)
		if prev, ok := latestIn(b); ok {
			if prev.Source == source {
				saved = prev
				return nil
			}
			next = prev.Version + 1
		}
		saved = ScenarioVersion{
			ScenarioID: id,
			Version:    next,
			SavedAt:    time.Now(),
			Note:       note,
			Name:       name,
			Path:       path,
			Source:     source,
		}
		payload, err := json.Marshal(saved)
		if err != nil {
			return err
		}
		written = true
		return b.Put(encodeVersion(next), payload)
	})
	saved.IsCurrent = true
	return saved, written, err
}

// ScenarioVersions lists a scenario's revisions, newest first. current is the
// source presently on disk, used to mark which revision is live; pass an empty
// string when that is not known.
func (s *Store) ScenarioVersions(id, current string, limit int) ([]ScenarioVersion, error) {
	var out []ScenarioVersion
	err := s.view(func(tx *bolt.Tx) error {
		b, err := scenarioBucket(tx, id, false)
		if err != nil || b == nil {
			return err
		}
		return b.ForEach(func(_, payload []byte) error {
			var v ScenarioVersion
			if err := json.Unmarshal(payload, &v); err != nil {
				return nil // skip anything unreadable rather than failing the list
			}
			out = append(out, v)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version > out[j].Version })
	// Only the newest matching revision is marked current: an older identical
	// one is history, not the live version.
	if current != "" {
		for i := range out {
			if out[i].Source == current {
				out[i].IsCurrent = true
				break
			}
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ScenarioVersion returns one revision.
func (s *Store) ScenarioVersion(id string, version uint64) (ScenarioVersion, error) {
	var out ScenarioVersion
	err := s.view(func(tx *bolt.Tx) error {
		b, err := scenarioBucket(tx, id, false)
		if err != nil {
			return err
		}
		if b == nil {
			return fmt.Errorf("scenario %q has no saved versions", id)
		}
		payload := b.Get(encodeVersion(version))
		if payload == nil {
			return fmt.Errorf("scenario %q has no version %d", id, version)
		}
		return json.Unmarshal(payload, &out)
	})
	return out, err
}

// ScenarioVersionCounts returns how many revisions each scenario has, so a
// listing can show the count without reading every revision.
func (s *Store) ScenarioVersionCounts() (map[string]int, error) {
	out := map[string]int{}
	err := s.view(func(tx *bolt.Tx) error {
		root := tx.Bucket(bucketScenarios)
		if root == nil {
			return nil
		}
		return root.ForEach(func(id, v []byte) error {
			if v != nil {
				return nil // a value here would be a key, not a sub-bucket
			}
			if b := root.Bucket(id); b != nil {
				out[string(id)] = b.Stats().KeyN
			}
			return nil
		})
	})
	return out, err
}
