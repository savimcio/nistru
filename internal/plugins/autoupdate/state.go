package autoupdate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// State is the persisted on-disk state for the auto-update plugin. Fields are
// omitempty so a zero-value state serializes to "{}" and forward-compatible
// additions don't pollute existing files.
type State struct {
	LastChecked           time.Time `json:"lastChecked,omitzero"`
	LastSeenVersion       string    `json:"lastSeenVersion,omitempty"`
	Channel               string    `json:"channel,omitempty"`
	PendingRestartVersion string    `json:"pendingRestartVersion,omitempty"`
	PrevBinaryPath        string    `json:"prevBinaryPath,omitempty"`
}

// DefaultChannel is the MVP default release channel.
func DefaultChannel() string { return "release" }

// StatePath returns the canonical path for the auto-update state file:
// <UserConfigDir>/nistru/autoupdate/state.json. It does not create any
// directories; callers (SaveState) handle mkdir lazily.
func StatePath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("autoupdate: user config dir: %w", err)
	}
	return filepath.Join(dir, "nistru", "autoupdate", "state.json"), nil
}

// LoadState reads and decodes the state file at path. It is intentionally
// forgiving: a missing file yields a zero-value state with no error, and a
// corrupt or malformed file logs a warning to stderr and also yields a
// zero-value state with no error — the plugin must never refuse to start
// because its state is bad. Only genuinely unexpected I/O failures (e.g. a
// permission-denied on an existing file) are surfaced.
func LoadState(path string) (State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return State{}, nil
		}
		return State{}, fmt.Errorf("autoupdate: read state: %w", err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		fmt.Fprintf(os.Stderr, "autoupdate: state file %q is corrupt, ignoring: %v\n", path, err)
		return State{}, nil
	}
	return s, nil
}

// SaveState atomically writes s to path. It creates the parent directory
// (0o755) if needed, marshals to indented JSON, writes to a sibling tmpfile
// in the same directory, fsyncs, and renames into place. On any error the
// tmpfile is removed so no ".tmp" siblings accumulate.
func SaveState(path string, s State) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("autoupdate: mkdir state dir: %w", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("autoupdate: marshal state: %w", err)
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("autoupdate: create tmp: %w", err)
	}
	tmp := f.Name()
	// cleanup on any error path below
	cleanup := func() { _ = os.Remove(tmp) }

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("autoupdate: write tmp: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("autoupdate: sync tmp: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return fmt.Errorf("autoupdate: close tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		cleanup()
		return fmt.Errorf("autoupdate: rename tmp: %w", err)
	}
	return nil
}
