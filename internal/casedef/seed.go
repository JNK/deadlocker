package casedef

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// examples ships the scenario library inside the binary so a fresh checkout —
// or a `go install`ed binary run from an empty directory — still has something
// to explore.
//
//go:embed all:examples
var examples embed.FS

// builtIn is the set of relative paths that ship with the binary, computed once.
// It is what distinguishes a scenario from the library as shipped from one the
// user wrote — a distinction worth drawing in the UI, since the built-in ones
// are documentation and the custom ones are work.
var builtIn = func() map[string]bool {
	out := map[string]bool{}
	_ = fs.WalkDir(examples, "examples", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if rel, relErr := filepath.Rel("examples", path); relErr == nil {
			out[filepath.ToSlash(rel)] = true
		}
		return nil
	})
	return out
}()

// IsBuiltIn reports whether a library-relative path is one of the scenarios
// that ship with the binary.
//
// Note that this is about provenance, not content: editing a built-in scenario
// leaves it built in, which is the useful reading — it is still the one the
// documentation refers to.
func IsBuiltIn(relPath string) bool {
	return builtIn[filepath.ToSlash(relPath)]
}

// SeedResult reports what Seed did.
type SeedResult struct {
	Written []string
	Skipped int
}

// BuiltInStatus reports how many built-in scenarios there are and how many are
// already present in dir, so the UI can offer the import truthfully.
func BuiltInStatus(dir string) (total, present int) {
	for rel := range builtIn {
		total++
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err == nil {
			present++
		}
	}
	return total, present
}

// Seed copies the built-in scenarios into dir, skipping any file that already
// exists. Existing files are never overwritten: once a scenario is on disk it
// belongs to the user, who may well have edited it.
func Seed(dir string) (SeedResult, error) {
	var res SeedResult
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return res, err
	}

	err := fs.WalkDir(examples, "examples", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel("examples", path)
		if relErr != nil {
			return relErr
		}
		target := filepath.Join(dir, rel)
		if _, statErr := os.Stat(target); statErr == nil {
			res.Skipped++
			return nil
		}
		data, readErr := examples.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if mkErr := os.MkdirAll(filepath.Dir(target), 0o755); mkErr != nil {
			return mkErr
		}
		if writeErr := os.WriteFile(target, data, 0o644); writeErr != nil {
			return writeErr
		}
		res.Written = append(res.Written, rel)
		return nil
	})
	if err != nil {
		return res, fmt.Errorf("seed scenarios into %s: %w", dir, err)
	}
	return res, nil
}
