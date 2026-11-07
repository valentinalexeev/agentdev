package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// loadState reads the state file. A missing file is a fresh state (baseline). A state for another repo, owner or
// schema version is an error: mixing them would report nonsense.
func loadState(path, repo, owner string) (State, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return NewState(repo, owner), nil
	}
	if err != nil {
		return State{}, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return State{}, fmt.Errorf("state %s: %w", path, err)
	}
	if s.Version != SchemaVersion {
		return State{}, fmt.Errorf("state %s: schema version %d, want %d (move the file away to start a new baseline)", path, s.Version, SchemaVersion)
	}
	if s.Repo != repo || s.Owner != owner {
		return State{}, fmt.Errorf("state %s is for %s (owner %s), not %s (owner %s)", path, s.Repo, s.Owner, repo, owner)
	}
	if s.PRs == nil {
		s.PRs = map[string]PRState{}
	}
	if s.Issues == nil {
		s.Issues = map[string]IssueState{}
	}
	return s, nil
}

// saveState writes atomically: a crash leaves the old or the new file, never half of one.
func saveState(path string, s State) error {
	b, err := json.MarshalIndent(s, "", " ") // encoding/json sorts map keys: the file is stable
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".ghwatch-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
