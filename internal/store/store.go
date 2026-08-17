// Package store persists configuration in a local bbolt database.
//
// Configuration is versioned: every save appends a new revision rather than
// overwriting, so a setting that used to work can always be restored. That
// matters most for the LLM endpoint, where a wrong base URL or model name
// silently breaks the chat and the previous value is the fastest fix.
package store

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	bucketConfig    = []byte("config_versions")
	bucketMeta      = []byte("meta")
	keyCurrent      = []byte("current_version")
	bucketScenarios = []byte("scenario_versions")
)

// LLMConfig points the built-in chat at an OpenAI-compatible endpoint.
type LLMConfig struct {
	Enabled bool `json:"enabled"`
	// BaseURL is the OpenAI-compatible root, e.g. http://localhost:11434/v1
	// or http://localhost:1234/v1 for LM Studio.
	BaseURL string `json:"base_url"`
	// APIKey is optional; many local servers ignore it entirely.
	APIKey string `json:"api_key,omitempty"`
	Model  string `json:"model"`

	// Sampling settings are all optional. Left unset, nothing is sent and the
	// server's own defaults apply — which is what you want with a local model
	// that already behaves, and avoids overriding a good default with a
	// worse guess.
	Temperature      *float64 `json:"temperature,omitempty"`
	MaxTokens        *int     `json:"max_tokens,omitempty"`
	TopP             *float64 `json:"top_p,omitempty"`
	TopK             *int64   `json:"top_k,omitempty"`
	MinP             *float64 `json:"min_p,omitempty"`
	RepeatPenalty    *float64 `json:"repeat_penalty,omitempty"`
	PresencePenalty  *float64 `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64 `json:"frequency_penalty,omitempty"`
	Seed             *int64   `json:"seed,omitempty"`
	// Effort is free text (low, medium, high, or whatever a given server
	// accepts) sent as reasoning_effort.
	Effort string `json:"effort,omitempty"`
	// Extra is a JSON object of additional request body fields, for anything a
	// particular server supports that has no field of its own here.
	Extra string `json:"extra,omitempty"`

	// MaxSteps caps agent tool-calling rounds so a confused model cannot loop
	// forever against a real MySQL server. Unset falls back to DefaultMaxSteps
	// rather than to "unlimited".
	MaxSteps *int `json:"max_steps,omitempty"`
}

// DefaultMaxSteps is the tool-loop guard used when none is configured.
const DefaultMaxSteps = 24

// Steps returns the effective tool-round cap.
func (c LLMConfig) Steps() int {
	if c.MaxSteps != nil && *c.MaxSteps > 0 {
		return *c.MaxSteps
	}
	return DefaultMaxSteps
}

// Config is everything the UI can change at runtime.
type Config struct {
	LLM LLMConfig `json:"llm"`
}

// DefaultConfig is what a fresh install starts from. The default base URL
// targets Ollama, which is the most common local setup; LM Studio, llama.cpp
// and vLLM all speak the same API on a different port.
func DefaultConfig() Config {
	return Config{
		LLM: LLMConfig{
			Enabled: false,
			BaseURL: "http://localhost:11434/v1",
			Model:   "",
		},
	}
}

// Normalise fills in only what genuinely cannot be left empty. Sampling
// settings are deliberately not defaulted: absent means "let the model server
// decide".
func (c *Config) Normalise() {
	if c.LLM.BaseURL == "" {
		c.LLM.BaseURL = DefaultConfig().LLM.BaseURL
	}
}

// ExtraParams parses the free-form kwargs object. An empty value is not an
// error; malformed JSON is, so it can be reported when saving rather than
// silently dropped at request time.
func (c LLMConfig) ExtraParams() (map[string]any, error) {
	if strings.TrimSpace(c.Extra) == "" {
		return nil, nil
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(c.Extra), &out); err != nil {
		return nil, fmt.Errorf("extra parameters must be a JSON object: %w", err)
	}
	return out, nil
}

// Ready reports whether the chat has enough configuration to run.
func (c Config) Ready() bool {
	return c.LLM.Enabled && c.LLM.BaseURL != "" && c.LLM.Model != ""
}

// Version is one saved revision of the configuration.
type Version struct {
	Version   uint64    `json:"version"`
	SavedAt   time.Time `json:"saved_at"`
	Note      string    `json:"note,omitempty"`
	Config    Config    `json:"config"`
	IsCurrent bool      `json:"is_current"`
}

// Redacted returns a copy with the API key masked, for anything that leaves
// the process.
func (v Version) Redacted() Version {
	if v.Config.LLM.APIKey != "" {
		v.Config.LLM.APIKey = "••••••••"
	}
	return v
}

// Store is the bbolt-backed configuration store.
type Store struct {
	db   *bolt.DB
	path string
}

// Open creates or opens the database at path, creating parent directories.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create state directory: %w", err)
		}
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	s := &Store{db: db, path: path}

	if err := db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bucketConfig, bucketMeta, bucketScenarios} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		// Seed version 1 so there is always something to read and to roll back
		// to.
		if tx.Bucket(bucketConfig).Stats().KeyN == 0 {
			return writeVersion(tx, Version{
				Version: 1,
				SavedAt: time.Now(),
				Note:    "initial defaults",
				Config:  DefaultConfig(),
			})
		}
		return nil
	}); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Path is the database location, shown in the settings UI.
func (s *Store) Path() string { return s.path }

func encodeVersion(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

func writeVersion(tx *bolt.Tx, v Version) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := tx.Bucket(bucketConfig).Put(encodeVersion(v.Version), payload); err != nil {
		return err
	}
	return tx.Bucket(bucketMeta).Put(keyCurrent, encodeVersion(v.Version))
}

// Current returns the active configuration.
func (s *Store) Current() (Config, uint64, error) {
	var (
		cfg Config
		ver uint64
	)
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bucketMeta).Get(keyCurrent)
		if raw == nil {
			cfg = DefaultConfig()
			return nil
		}
		ver = binary.BigEndian.Uint64(raw)
		payload := tx.Bucket(bucketConfig).Get(raw)
		if payload == nil {
			cfg = DefaultConfig()
			return nil
		}
		var v Version
		if err := json.Unmarshal(payload, &v); err != nil {
			return err
		}
		cfg = v.Config
		return nil
	})
	cfg.Normalise()
	return cfg, ver, err
}

// Save appends a new configuration revision and makes it current.
//
// An empty APIKey in the incoming config is treated as "unchanged" rather than
// "cleared", because the UI never receives the real key back and would
// otherwise wipe it on every save. ClearAPIKey exists for deliberate removal.
func (s *Store) Save(cfg Config, note string) (Version, error) {
	cfg.Normalise()
	var saved Version
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketConfig)
		next := uint64(1)
		if raw := tx.Bucket(bucketMeta).Get(keyCurrent); raw != nil {
			next = binary.BigEndian.Uint64(raw) + 1
		}
		saved = Version{Version: next, SavedAt: time.Now(), Note: note, Config: cfg}
		_ = b
		return writeVersion(tx, saved)
	})
	saved.IsCurrent = true
	return saved, err
}

// Versions lists saved revisions, newest first.
func (s *Store) Versions(limit int) ([]Version, error) {
	var out []Version
	var current uint64
	err := s.db.View(func(tx *bolt.Tx) error {
		if raw := tx.Bucket(bucketMeta).Get(keyCurrent); raw != nil {
			current = binary.BigEndian.Uint64(raw)
		}
		return tx.Bucket(bucketConfig).ForEach(func(_, payload []byte) error {
			var v Version
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
	for i := range out {
		out[i].IsCurrent = out[i].Version == current
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version > out[j].Version })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Restore copies an old revision forward as a new version. History is
// append-only: restoring never rewrites or deletes what came after it.
func (s *Store) Restore(version uint64) (Version, error) {
	var restored Version
	err := s.db.Update(func(tx *bolt.Tx) error {
		payload := tx.Bucket(bucketConfig).Get(encodeVersion(version))
		if payload == nil {
			return fmt.Errorf("configuration version %d does not exist", version)
		}
		var old Version
		if err := json.Unmarshal(payload, &old); err != nil {
			return err
		}
		next := uint64(1)
		if raw := tx.Bucket(bucketMeta).Get(keyCurrent); raw != nil {
			next = binary.BigEndian.Uint64(raw) + 1
		}
		restored = Version{
			Version: next,
			SavedAt: time.Now(),
			Note:    fmt.Sprintf("restored from version %d", version),
			Config:  old.Config,
		}
		return writeVersion(tx, restored)
	})
	restored.IsCurrent = true
	return restored, err
}
