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

// SeedResult reports what Seed did.
type SeedResult struct {
	Written []string
	Skipped int
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
