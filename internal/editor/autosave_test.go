package editor

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// -----------------------------------------------------------------------------
// atomicWriteFile.

func TestAtomicWriteFile_HappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "note.txt")
	data := []byte("hello nistru")

	if err := atomicWriteFile(path, data); err != nil {
		t.Fatalf("atomicWriteFile: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("readback mismatch: got %q, want %q", got, data)
	}

	// File mode: new file defaults to 0644 per the function contract.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("new-file mode: got %o, want 0644", got)
	}

	// No leftover .tmp sibling.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("expected no .tmp sibling, stat err=%v", err)
	}
}

func TestAtomicWriteFile_OverwritePreservesMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pre-existing.txt")
	// Seed the destination with non-default perms; atomicWriteFile must
	// preserve them on rewrite.
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := atomicWriteFile(path, []byte("new contents")); err != nil {
		t.Fatalf("atomicWriteFile: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	if string(got) != "new contents" {
		t.Errorf("overwrite: got %q, want %q", got, "new contents")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode not preserved on overwrite: got %o, want 0600", got)
	}
	// And no stray .tmp.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("expected no .tmp sibling after overwrite, stat err=%v", err)
	}
}

func TestAtomicWriteFile_PermissionDenied(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX perm bits semantics only")
	}
	// Running as root skips perm-enforced errors.
	if os.Geteuid() == 0 {
		t.Skip("root bypasses 0555 perms")
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o555); err != nil {
		t.Fatalf("mkdir locked: %v", err)
	}
	// Restore perms on teardown so t.TempDir can clean up.
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	path := filepath.Join(locked, "denied.txt")
	err := atomicWriteFile(path, []byte("nope"))
	if err == nil {
		t.Fatalf("expected permission error, got nil")
	}
	// Assert no partial file at the target path.
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("expected no partial file at %q, stat err=%v", path, statErr)
	}
	// And no .tmp sibling either.
	if _, statErr := os.Stat(path + ".tmp"); !os.IsNotExist(statErr) {
		t.Errorf("expected no .tmp sibling after failure, stat err=%v", statErr)
	}
}

// -----------------------------------------------------------------------------
// scheduleSave / scheduleChange — assert the message type and generation
// stamp without running the debounce timer to completion is enough here. We
// invoke the returned cmd directly (it blocks on tea.Tick), so each test
// exercises the genuine timer but only for the short debounce window. Total
// runtime budget for this file: well under 500ms.

func TestScheduleSave_EmitsSaveTickMsgWithGen(t *testing.T) {
	if testing.Short() {
		t.Skip("relies on 250ms debounce timer; use full go test to run")
	}
	t.Parallel()
	cmd := scheduleSave(42, 250*time.Millisecond)
	if cmd == nil {
		t.Fatalf("scheduleSave returned nil cmd")
	}
	msg := runCmdWithTimeout(t, cmd, 2*time.Second)
	tick, ok := msg.(saveTickMsg)
	if !ok {
		t.Fatalf("expected saveTickMsg, got %T (%v)", msg, msg)
	}
	if tick.gen != 42 {
		t.Errorf("saveTickMsg.gen: got %d, want 42", tick.gen)
	}
}

func TestScheduleChange_EmitsChangeTickMsgWithGen(t *testing.T) {
	// 50ms debounce — fast enough to run under -short.
	t.Parallel()
	cmd := scheduleChange(7, 50*time.Millisecond)
	if cmd == nil {
		t.Fatalf("scheduleChange returned nil cmd")
	}
	msg := runCmdWithTimeout(t, cmd, 2*time.Second)
	tick, ok := msg.(changeTickMsg)
	if !ok {
		t.Fatalf("expected changeTickMsg, got %T (%v)", msg, msg)
	}
	if tick.gen != 7 {
		t.Errorf("changeTickMsg.gen: got %d, want 7", tick.gen)
	}
}

func TestScheduleSave_StaleVsCurrent(t *testing.T) {
	if testing.Short() {
		t.Skip("relies on 250ms debounce timer; use full go test to run")
	}
	t.Parallel()
	// Bump the generation between two scheduleSave calls; both still fire
	// and we can compare their gen stamps. The "current" gen in this test is
	// 11 — the model.go logic (saveTickMsg.gen != m.saveGen) would discard
	// any tick whose gen is not 11. Here we directly assert the stamps.
	//
	// Run both debounce waits concurrently so total wall-clock stays close
	// to a single 250ms window instead of two back-to-back windows.
	staleCh := make(chan tea.Msg, 1)
	freshCh := make(chan tea.Msg, 1)
	staleCmd := scheduleSave(10, 250*time.Millisecond)
	freshCmd := scheduleSave(11, 250*time.Millisecond)
	go func() { staleCh <- staleCmd() }()
	go func() { freshCh <- freshCmd() }()

	var stale, fresh saveTickMsg
	for range 2 {
		select {
		case m := <-staleCh:
			stale = m.(saveTickMsg)
		case m := <-freshCh:
			fresh = m.(saveTickMsg)
		case <-time.After(2 * time.Second):
			t.Fatalf("debounce timer did not fire within 2s")
		}
	}

	if stale.gen == fresh.gen {
		t.Fatalf("gen stamps collided: stale=%d fresh=%d", stale.gen, fresh.gen)
	}
	if stale.gen != 10 {
		t.Errorf("stale gen: got %d, want 10", stale.gen)
	}
	if fresh.gen != 11 {
		t.Errorf("fresh gen: got %d, want 11", fresh.gen)
	}
	// Sanity: a caller comparing against the current counter (11) would
	// treat `stale` as stale.
	current := 11
	if stale.gen == current {
		t.Errorf("stale must differ from current counter; stale=%d current=%d", stale.gen, current)
	}
	if fresh.gen != current {
		t.Errorf("fresh must equal current counter; fresh=%d current=%d", fresh.gen, current)
	}
}

// runCmdWithTimeout executes a tea.Cmd on a goroutine and returns the first
// message, failing the test if it doesn't arrive within timeout. scheduleSave
// / scheduleChange block until the debounce elapses (250ms / 50ms) so we need
// a bounded wait to keep test runtime deterministic under -race.
func runCmdWithTimeout(t *testing.T, cmd tea.Cmd, timeout time.Duration) tea.Msg {
	t.Helper()
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	select {
	case m := <-ch:
		return m
	case <-time.After(timeout):
		t.Fatalf("cmd did not produce a message within %s", timeout)
		return nil
	}
}
