package autoupdate

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStatePathIsUnderUserConfigDir(t *testing.T) {
	got, err := StatePath()
	if err != nil {
		t.Fatalf("StatePath: %v", err)
	}
	base, err := os.UserConfigDir()
	if err != nil {
		t.Fatalf("os.UserConfigDir: %v", err)
	}
	if !strings.HasPrefix(got, base) {
		t.Errorf("StatePath %q is not under UserConfigDir %q", got, base)
	}
	want := filepath.Join("nistru", "autoupdate", "state.json")
	if !strings.HasSuffix(got, want) {
		t.Errorf("StatePath %q does not end with %q", got, want)
	}
}

func TestLoadStateMissingReturnsZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist", "state.json")
	s, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState(missing): err=%v", err)
	}
	if s != (State{}) {
		t.Errorf("LoadState(missing): want zero State, got %+v", s)
	}
}

func TestLoadStateCorruptReturnsZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}
	s, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState(corrupt): err=%v", err)
	}
	if s != (State{}) {
		t.Errorf("LoadState(corrupt): want zero State, got %+v", s)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	// Pin timestamp precision by round-tripping through JSON once. Go's
	// time.Time JSON marshaling uses RFC3339Nano, so a raw time.Now() may
	// carry monotonic clock bits / ns precision that don't survive the
	// round trip.
	now := time.Now().UTC()
	raw, err := json.Marshal(now)
	if err != nil {
		t.Fatalf("marshal now: %v", err)
	}
	var pinned time.Time
	if err := json.Unmarshal(raw, &pinned); err != nil {
		t.Fatalf("unmarshal pinned: %v", err)
	}

	want := State{
		LastChecked:           pinned,
		LastSeenVersion:       "v1.2.3",
		Channel:               "release",
		PendingRestartVersion: "v1.2.4",
		PrevBinaryPath:        "/tmp/nistru.prev",
	}
	if err := SaveState(path, want); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	got, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if !got.LastChecked.Equal(want.LastChecked) {
		t.Errorf("LastChecked: want %v, got %v", want.LastChecked, got.LastChecked)
	}
	if got.LastSeenVersion != want.LastSeenVersion {
		t.Errorf("LastSeenVersion: want %q, got %q", want.LastSeenVersion, got.LastSeenVersion)
	}
	if got.Channel != want.Channel {
		t.Errorf("Channel: want %q, got %q", want.Channel, got.Channel)
	}
	if got.PendingRestartVersion != want.PendingRestartVersion {
		t.Errorf("PendingRestartVersion: want %q, got %q", want.PendingRestartVersion, got.PendingRestartVersion)
	}
	if got.PrevBinaryPath != want.PrevBinaryPath {
		t.Errorf("PrevBinaryPath: want %q, got %q", want.PrevBinaryPath, got.PrevBinaryPath)
	}
}

func TestSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	// First write: a good state.
	first := State{LastSeenVersion: "v1.0.0", Channel: "release"}
	if err := SaveState(path, first); err != nil {
		t.Fatalf("SaveState #1: %v", err)
	}
	assertNoTmpSiblings(t, dir, "state.json")

	// Simulate a prior crashed write: a stale .tmp sibling file.
	stale := filepath.Join(dir, "state.json.stale.tmp")
	if err := os.WriteFile(stale, []byte("garbage"), 0o644); err != nil {
		t.Fatalf("seed stale tmp: %v", err)
	}

	// Second write: a new good state. This should leave the real file valid
	// and should not touch the stale tmp we planted (we clean it up manually
	// afterwards to keep the invariant check meaningful).
	second := State{LastSeenVersion: "v2.0.0", Channel: "beta"}
	if err := SaveState(path, second); err != nil {
		t.Fatalf("SaveState #2: %v", err)
	}

	// The final state.json must parse and reflect the second write.
	got, err := LoadState(path)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got.LastSeenVersion != "v2.0.0" || got.Channel != "beta" {
		t.Errorf("final state: want {v2.0.0, beta}, got %+v", got)
	}

	// SaveState must not have left its own tmp file behind. The only tmp in
	// the dir should be the one we manually planted.
	if _, err := os.Stat(stale); err != nil {
		t.Errorf("stale tmp was unexpectedly removed: %v", err)
	}
	if err := os.Remove(stale); err != nil {
		t.Fatalf("remove stale tmp: %v", err)
	}
	assertNoTmpSiblings(t, dir, "state.json")
}

func TestSaveCreatesParentDir(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "a", "b", "c")
	path := filepath.Join(nested, "state.json")

	// Sanity: parent must not exist yet.
	if _, err := os.Stat(nested); !errorsIsNotExist(err) {
		t.Fatalf("precondition: nested dir should not exist yet, stat err=%v", err)
	}

	if err := SaveState(path, State{Channel: "release"}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	info, err := os.Stat(nested)
	if err != nil {
		t.Fatalf("stat nested: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("nested %q is not a directory", nested)
	}
	// On Unix the created dir should be mode 0o755 (subject to umask). Rather
	// than asserting an exact mode (umask-dependent), assert the dir is at
	// least user-rwx and that the file inside exists.
	if info.Mode().Perm()&0o700 != 0o700 {
		t.Errorf("nested dir perm %v missing user rwx", info.Mode().Perm())
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("state file missing after save: %v", err)
	}
}

// assertNoTmpSiblings asserts there are no "<base>.*.tmp" files in dir. A
// stray tmp means SaveState leaked a partial write.
func assertNoTmpSiblings(t *testing.T, dir, base string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, base+".") && strings.HasSuffix(name, ".tmp") {
			t.Errorf("stray tmp sibling left behind: %s", name)
		}
	}
}

func errorsIsNotExist(err error) bool {
	if err == nil {
		return false
	}
	return os.IsNotExist(err) || err == fs.ErrNotExist
}
