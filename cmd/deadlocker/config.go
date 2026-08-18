package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
)

// Where configuration lives.
//
// It used to sit in `.deadlocker/state.db` beside the case directory, which
// made it a property of whichever folder you happened to be standing in: run
// the tool from somewhere else and your model endpoint, your API key and your
// scenario revisions were simply not there. It is per-user state, so it now
// lives where per-user state goes and follows you between projects.

// configDir returns the directory holding the state file, honouring
// XDG_CONFIG_HOME. The directory is created if it does not exist.
func configDir() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	dir := filepath.Join(base, "deadlocker")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("config directory %s: %w", dir, err)
	}
	return dir, nil
}

// legacyStatePath is where the state file used to live for a given case
// directory.
func legacyStatePath(absCases string) string {
	return filepath.Join(filepath.Dir(absCases), ".deadlocker", "state.db")
}

// migrateState moves a state file from the old per-project location to the new
// per-user one, once. It is deliberately a move: two files that both look
// current is a worse problem than the one this solves.
func migrateState(absCases, newPath string) {
	old := legacyStatePath(absCases)
	if old == newPath {
		return
	}
	if _, err := os.Stat(newPath); err == nil {
		return // already migrated, or a new install
	}
	if _, err := os.Stat(old); err != nil {
		return // nothing to bring over
	}
	if err := os.Rename(old, newPath); err != nil {
		// Across filesystems Rename fails; copying is the fallback, and the
		// original is left alone so nothing is lost either way.
		if err := copyFile(old, newPath); err != nil {
			log.Printf("could not move the old configuration from %s: %v", old, err)
			return
		}
		log.Printf("copied configuration from %s (the original is untouched)", old)
		return
	}
	log.Printf("moved configuration from %s", old)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// resolveState works out where the state file belongs: an explicit -state wins,
// otherwise the per-user config directory, with a one-time migration from the
// old per-project path. It returns the state file and the directory holding it.
func resolveState(explicit, absCases string) (path string, dir string, err error) {
	if explicit != "" {
		abs, err := filepath.Abs(explicit)
		if err != nil {
			return "", "", err
		}
		dir := filepath.Dir(abs)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", "", err
		}
		return abs, dir, nil
	}
	dir, err = configDir()
	if err != nil {
		return "", "", err
	}
	path = filepath.Join(dir, "state.db")
	migrateState(absCases, path)
	return path, dir, nil
}
