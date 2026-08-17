// Package casedef defines the YAML scenario format and the library that loads
// it from disk.
package casedef

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Expectation is what a step is predicted to do. Recording it in the scenario
// turns each case into a falsifiable claim: the UI shows expected vs actual so
// a wrong hypothesis is obvious.
type Expectation string

const (
	ExpectOK       Expectation = "ok"       // completes without error
	ExpectBlocks   Expectation = "blocks"   // does not complete; waiting on a lock
	ExpectError    Expectation = "error"    // completes with any error
	ExpectDeadlock Expectation = "deadlock" // errno 1213, chosen as deadlock victim
	ExpectTimeout  Expectation = "timeout"  // errno 1205, lock wait timeout
	ExpectAny      Expectation = ""         // no assertion
)

func (e Expectation) Valid() bool {
	switch e {
	case ExpectOK, ExpectBlocks, ExpectError, ExpectDeadlock, ExpectTimeout, ExpectAny:
		return true
	}
	return false
}

// Actor is one simulated client. Each actor gets its own dedicated MySQL
// connection for the whole run, so transaction state persists across steps.
type Actor struct {
	ID   string `yaml:"id" json:"id"`
	Name string `yaml:"name,omitempty" json:"name"`
	// Accent picks the swimlane colour: blue, amber, violet, teal, rose.
	Accent string `yaml:"accent,omitempty" json:"accent"`
}

// Step is a single statement issued by one actor.
type Step struct {
	Actor string `yaml:"actor" json:"actor"`
	Label string `yaml:"label,omitempty" json:"label"`
	SQL   string `yaml:"sql" json:"sql"`
	// Args are bound as query parameters. Without them the SQL is sent as-is.
	Args []any `yaml:"args,omitempty" json:"args,omitempty"`
	// Note is prose shown alongside the step, explaining what to watch for.
	Note   string      `yaml:"note,omitempty" json:"note,omitempty"`
	Expect Expectation `yaml:"expect,omitempty" json:"expect,omitempty"`
}

// MySQLConfig captures the server and session settings a scenario depends on.
type MySQLConfig struct {
	Image string `yaml:"image,omitempty" json:"image"`
	// Isolation is applied per session, e.g. "REPEATABLE READ",
	// "READ COMMITTED", "SERIALIZABLE", "READ UNCOMMITTED".
	Isolation string `yaml:"isolation,omitempty" json:"isolation"`
	// LockWaitTimeout sets innodb_lock_wait_timeout per session (seconds).
	LockWaitTimeout int `yaml:"lock_wait_timeout,omitempty" json:"lock_wait_timeout"`
	// DeadlockDetect toggles the global innodb_deadlock_detect. Turning it off
	// converts deadlocks into lock wait timeouts, which is instructive.
	DeadlockDetect *bool `yaml:"deadlock_detect,omitempty" json:"deadlock_detect,omitempty"`
	// Vars are extra session variables applied to every actor connection.
	Vars map[string]string `yaml:"vars,omitempty" json:"vars,omitempty"`
	// Prepared sends statements as real prepared statements
	// (COM_STMT_PREPARE / COM_STMT_EXECUTE) instead of interpolating arguments
	// into a plain COM_QUERY. Locking behaviour is unchanged; what changes is
	// the wire trace, which switches to the binary protocol.
	Prepared bool `yaml:"prepared,omitempty" json:"prepared,omitempty"`
}

// Case is one scenario.
type Case struct {
	ID          string      `yaml:"id,omitempty" json:"id"`
	Name        string      `yaml:"name" json:"name"`
	Category    string      `yaml:"category,omitempty" json:"category"`
	Description string      `yaml:"description,omitempty" json:"description"`
	Tags        []string    `yaml:"tags,omitempty" json:"tags,omitempty"`
	MySQL       MySQLConfig `yaml:"mysql,omitempty" json:"mysql"`
	Schema      []string    `yaml:"schema,omitempty" json:"schema"`
	Seed        []string    `yaml:"seed,omitempty" json:"seed"`
	Actors      []Actor     `yaml:"actors" json:"actors"`
	Steps       []Step      `yaml:"steps" json:"steps"`

	// Path is where the case was loaded from. Empty for ad-hoc playground cases.
	Path string `yaml:"-" json:"path,omitempty"`
	// Source is the raw YAML, kept so the playground editor round-trips
	// comments and formatting instead of re-marshalling.
	Source string `yaml:"-" json:"source,omitempty"`
	// Ephemeral marks a scenario that exists only for this session -- a draft
	// being built, run before it has been saved. Runs of it are real, but it
	// has no place in the library or the sidebar.
	Ephemeral bool `yaml:"-" json:"ephemeral,omitempty"`
}

// Marshal renders a case back to YAML. Comments and original formatting are
// lost, so this is for generated scenarios -- a minimised repro, say -- rather
// than for round-tripping a file someone wrote.
func Marshal(c *Case) ([]byte, error) {
	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(c); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}

// Parse decodes and validates a case from YAML.
func Parse(data []byte) (*Case, error) {
	var c Case
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("invalid YAML: %w", err)
	}
	c.Source = string(data)
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate checks structural invariants and fills in defaults.
func (c *Case) Validate() error {
	var problems []string

	if strings.TrimSpace(c.Name) == "" {
		problems = append(problems, "name is required")
	}
	if len(c.Actors) == 0 {
		problems = append(problems, "at least one actor is required")
	}
	if len(c.Steps) == 0 {
		problems = append(problems, "at least one step is required")
	}

	seen := map[string]bool{}
	accents := []string{"blue", "amber", "violet", "teal", "rose"}
	for i := range c.Actors {
		a := &c.Actors[i]
		if a.ID == "" {
			problems = append(problems, fmt.Sprintf("actor %d: id is required", i+1))
			continue
		}
		if seen[a.ID] {
			problems = append(problems, fmt.Sprintf("actor %q: duplicate id", a.ID))
		}
		seen[a.ID] = true
		if a.Name == "" {
			a.Name = a.ID
		}
		if a.Accent == "" {
			a.Accent = accents[i%len(accents)]
		}
	}

	for i := range c.Steps {
		s := &c.Steps[i]
		n := i + 1
		if s.Actor == "" {
			problems = append(problems, fmt.Sprintf("step %d: actor is required", n))
		} else if !seen[s.Actor] {
			problems = append(problems, fmt.Sprintf("step %d: unknown actor %q", n, s.Actor))
		}
		if strings.TrimSpace(s.SQL) == "" {
			problems = append(problems, fmt.Sprintf("step %d: sql is required", n))
		}
		if !s.Expect.Valid() {
			problems = append(problems, fmt.Sprintf("step %d: unknown expect %q (want ok, blocks, error, deadlock or timeout)", n, s.Expect))
		}
		if s.Label == "" {
			s.Label = summarise(s.SQL)
		}
	}

	if c.MySQL.Image == "" {
		c.MySQL.Image = "mysql:8.4"
	}
	if c.MySQL.LockWaitTimeout == 0 {
		// Generous by default: scenarios are stepped through by hand, and a
		// statement timing out while you read the explanation of why it is
		// blocked is the wrong lesson. Cases that want to demonstrate the
		// timeout set a short value explicitly.
		c.MySQL.LockWaitTimeout = 120
	}
	if iso := strings.TrimSpace(c.MySQL.Isolation); iso != "" && !validIsolation(iso) {
		problems = append(problems, fmt.Sprintf("mysql.isolation %q is not a valid isolation level", iso))
	}

	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func validIsolation(s string) bool {
	switch strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(s), "-", " ")) {
	case "READ UNCOMMITTED", "READ COMMITTED", "REPEATABLE READ", "SERIALIZABLE":
		return true
	}
	return false
}

// NormalisedIsolation returns the isolation level in the spelling MySQL's SET
// TRANSACTION statement expects, or "" when the scenario does not set one.
func (c *Case) NormalisedIsolation() string {
	iso := strings.TrimSpace(c.MySQL.Isolation)
	if iso == "" {
		return ""
	}
	return strings.ToUpper(strings.ReplaceAll(iso, "-", " "))
}

// Actor looks up an actor by id.
func (c *Case) Actor(id string) (Actor, bool) {
	for _, a := range c.Actors {
		if a.ID == id {
			return a, true
		}
	}
	return Actor{}, false
}

// summarise turns a statement into a short label for the timeline.
func summarise(sqlText string) string {
	s := strings.Join(strings.Fields(sqlText), " ")
	if len(s) > 64 {
		s = s[:63] + "…"
	}
	return s
}

// slugify derives a stable id from a file name.
func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// Library is the on-disk collection of cases under a root directory.
type Library struct {
	Root string

	mu    sync.RWMutex
	cases map[string]*Case
	order []string
	// broken records files that failed to parse, so the UI can show why
	// instead of silently omitting them.
	broken map[string]string
}

func NewLibrary(root string) *Library {
	return &Library{Root: root, cases: map[string]*Case{}, broken: map[string]string{}}
}

// Load rescans the root directory. It is safe to call repeatedly; the UI calls
// it on every listing so edits on disk show up without a restart.
func (l *Library) Load() error {
	cases := map[string]*Case{}
	broken := map[string]string{}
	var order []string

	err := filepath.WalkDir(l.Root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") && path != l.Root {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}
		rel, _ := filepath.Rel(l.Root, path)
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			broken[rel] = readErr.Error()
			return nil
		}
		c, parseErr := Parse(data)
		if parseErr != nil {
			broken[rel] = parseErr.Error()
			return nil
		}
		c.Path = rel
		if c.ID == "" {
			c.ID = slugify(strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
		}
		if c.Category == "" {
			if dir := filepath.Dir(rel); dir != "." {
				c.Category = strings.ReplaceAll(dir, string(filepath.Separator), " / ")
			} else {
				c.Category = "Uncategorised"
			}
		}
		if _, clash := cases[c.ID]; clash {
			broken[rel] = fmt.Sprintf("duplicate case id %q", c.ID)
			return nil
		}
		cases[c.ID] = c
		order = append(order, c.ID)
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	sort.Slice(order, func(i, j int) bool {
		a, b := cases[order[i]], cases[order[j]]
		if a.Category != b.Category {
			return a.Category < b.Category
		}
		return a.Name < b.Name
	})

	l.mu.Lock()
	l.cases, l.order, l.broken = cases, order, broken
	l.mu.Unlock()
	return nil
}

// List returns all cases in category-then-name order.
func (l *Library) List() []*Case {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]*Case, 0, len(l.order))
	for _, id := range l.order {
		out = append(out, l.cases[id])
	}
	return out
}

// Broken returns parse failures keyed by relative path.
func (l *Library) Broken() map[string]string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make(map[string]string, len(l.broken))
	for k, v := range l.broken {
		out[k] = v
	}
	return out
}

// Exists reports whether a file already occupies a library-relative path.
func (l *Library) Exists(relPath string) bool {
	clean := filepath.Clean(relPath)
	if ext := strings.ToLower(filepath.Ext(clean)); ext != ".yaml" && ext != ".yml" {
		clean += ".yaml"
	}
	_, err := os.Stat(filepath.Join(l.Root, clean))
	return err == nil
}

// IDForPath returns the id a file at relPath would be given, which is derived
// from its base name.
func IDForPath(relPath string) string {
	base := filepath.Base(relPath)
	return slugify(strings.TrimSuffix(base, filepath.Ext(base)))
}

func (l *Library) Get(id string) (*Case, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	c, ok := l.cases[id]
	return c, ok
}

// Categories groups cases for the sidebar, preserving sort order.
type Category struct {
	Name  string
	Cases []*Case
}

func (l *Library) Categories() []Category {
	var out []Category
	for _, c := range l.List() {
		if n := len(out); n > 0 && out[n-1].Name == c.Category {
			out[n-1].Cases = append(out[n-1].Cases, c)
			continue
		}
		out = append(out, Category{Name: c.Category, Cases: []*Case{c}})
	}
	return out
}

// Save writes a case to the library, creating or overwriting a file. relPath is
// relative to the library root and must stay inside it.
func (l *Library) Save(relPath string, data []byte) (*Case, error) {
	c, err := Parse(data)
	if err != nil {
		return nil, err
	}
	clean := filepath.Clean(relPath)
	if clean == "." || strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
		return nil, fmt.Errorf("invalid path %q: must be relative to the case library", relPath)
	}
	if ext := strings.ToLower(filepath.Ext(clean)); ext != ".yaml" && ext != ".yml" {
		clean += ".yaml"
	}
	full := filepath.Join(l.Root, clean)
	// Defence in depth: confirm the resolved path is still under the root.
	rootAbs, err := filepath.Abs(l.Root)
	if err != nil {
		return nil, err
	}
	fullAbs, err := filepath.Abs(full)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(fullAbs, rootAbs+string(filepath.Separator)) {
		return nil, fmt.Errorf("invalid path %q: escapes the case library", relPath)
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(full, data, 0o644); err != nil {
		return nil, err
	}
	if err := l.Load(); err != nil {
		return nil, err
	}

	// Return the reloaded case rather than the one just parsed: id and category
	// are derived from the file's location during Load, so the parsed copy has
	// neither. Callers rely on the id to address what they just wrote.
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, loaded := range l.cases {
		if loaded.Path == clean {
			return loaded, nil
		}
	}
	c.Path = clean
	return c, nil
}
