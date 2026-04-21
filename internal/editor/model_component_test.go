package editor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/savimcio/nistru/plugin"
)

// -----------------------------------------------------------------------------
// Helpers shared by the component tests. Building a real *Model through
// NewModel is preferred where possible — it wires up the plugin host and the
// in-proc treepane plugin exactly as production does. NewModel requires a
// real filesystem root, so we point it at a t.TempDir and seed it as each
// test needs.
//
// Tests do NOT call Model.Init(); that returns host.Recv() which blocks on an
// empty channel forever. Nothing in T4's scope needs Recv's blocking behaviour
// — the tests feed inbound messages directly to Update().

func newTestModel(t *testing.T, root string) *Model {
	t.Helper()
	m, err := NewModel(root)
	if err != nil {
		t.Fatalf("NewModel(%q): %v", root, err)
	}
	t.Cleanup(func() {
		// Host.Shutdown has a bounded timeout and logs its own errors. No
		// out-of-proc plugins spawn in these tests, so the call is effectively
		// a no-op, but it keeps the cleanup intent explicit.
		_ = m.host.Shutdown(100 * time.Millisecond)
	})
	return m
}

// writeFile writes contents to dir/name and returns its absolute path.
func writeFile(t *testing.T, dir, name, contents string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatalf("abs %s: %v", p, err)
	}
	return abs
}

// -----------------------------------------------------------------------------
// T4.1 Open-file flow.

func TestModel_OpenFileFlow(t *testing.T) {
	t.Run("happy path seeds editor and clears dirty", func(t *testing.T) {
		dir := t.TempDir()
		path := writeFile(t, dir, "hello.go", "package main\n\nfunc main() {}\n")
		m := newTestModel(t, dir)

		// Pre-condition: no file open, dirty is false, path is empty.
		if m.openPath != "" {
			t.Fatalf("pre: openPath should be empty, got %q", m.openPath)
		}

		newM, cmd := m.Update(openFileRequestMsg{path: path})
		got := newM.(*Model)
		if got.openPath != path {
			t.Errorf("openPath: got %q, want %q", got.openPath, path)
		}
		if got.dirty {
			t.Errorf("dirty should be false after successful open")
		}
		if got.lastText != "package main\n\nfunc main() {}\n" {
			t.Errorf("lastText not seeded from file: got %q", got.lastText)
		}
		// Buffer should contain the file contents so a subsequent View() or
		// change-tick would see them.
		if txt := got.editor.GetBuffer().Text(); txt != got.lastText {
			t.Errorf("editor buffer mismatch: got %q, want %q", txt, got.lastText)
		}
		// openFile returns tea.Batch(editor.Init(), ...) — non-nil cmd.
		if cmd == nil {
			t.Errorf("expected non-nil cmd from openFile; got nil")
		}
		// Focus flips to editor on open.
		if got.focus != focusEditor {
			t.Errorf("focus after open: got %v, want focusEditor", got.focus)
		}
	})

	t.Run("nonexistent file leaves state unchanged", func(t *testing.T) {
		dir := t.TempDir()
		m := newTestModel(t, dir)
		prevEditor := m.editor

		missing := filepath.Join(dir, "does-not-exist.go")
		newM, cmd := m.Update(openFileRequestMsg{path: missing})
		got := newM.(*Model)
		if got.openPath != "" {
			t.Errorf("openPath must NOT change on failed open; got %q", got.openPath)
		}
		if got.dirty {
			t.Errorf("dirty must remain false on failed open")
		}
		// Editor should be the same instance — no swap on failure.
		if got.editor != prevEditor {
			t.Errorf("editor should not have been swapped on failed open")
		}
		// openFile always returns a cmd on failure (the status-message cmd),
		// so nil would be a bug.
		if cmd == nil {
			t.Errorf("expected non-nil status-message cmd on failed open")
		}
	})

	t.Run("binary file refused", func(t *testing.T) {
		dir := t.TempDir()
		path := writeFile(t, dir, "blob.bin", "hello\x00world")
		m := newTestModel(t, dir)

		newM, cmd := m.Update(openFileRequestMsg{path: path})
		got := newM.(*Model)
		if got.openPath != "" {
			t.Errorf("binary file must not be opened; openPath=%q", got.openPath)
		}
		if cmd == nil {
			t.Errorf("expected a status-message cmd on binary refusal")
		}
	})
}

// -----------------------------------------------------------------------------
// T4.2 Autosave loop end-to-end with injected clock.
//
// Flow: open a file, mutate its buffer, feed changeTickMsg (change-debounce),
// then saveTickMsg (save-debounce), and verify the file on disk matches the
// buffer text and lastSavedAt == the stubbed time.

func TestModel_AutosaveLoopEndToEnd(t *testing.T) {
	t.Run("change-tick + save-tick writes file with stubbed clock", func(t *testing.T) {
		// Pin nowFunc for this subtest only; restore on cleanup.
		stubTime := time.Date(2099, 1, 2, 3, 4, 5, 0, time.UTC)
		prev := nowFunc
		nowFunc = func() time.Time { return stubTime }
		t.Cleanup(func() { nowFunc = prev })

		dir := t.TempDir()
		path := writeFile(t, dir, "a.txt", "one\n")
		m := newTestModel(t, dir)

		newM, _ := m.Update(openFileRequestMsg{path: path})
		m = newM.(*Model)

		// Simulate an edit: mutate the buffer + mark dirty + bump gens the
		// same way forwardToFocused would. Record the new change/save gens
		// as the "live" ones.
		m.editor.GetBuffer().InsertAt(0, 0, "x")
		m.dirty = true
		m.changeGen++
		m.saveGen++
		changeGen := m.changeGen
		saveGen := m.saveGen

		// Change-tick: fires host.Emit(DidChange{...}) and returns an effect
		// cmd. No assertion on the cmd itself — we only need to verify the
		// model accepted the tick and left state ready for a save.
		newM, _ = m.Update(changeTickMsg{gen: changeGen})
		m = newM.(*Model)

		// Save-tick: honoured because gen matches. Triggers flushNow.
		newM, _ = m.Update(saveTickMsg{gen: saveGen})
		m = newM.(*Model)

		// Assert disk contents equal the mutated buffer.
		wantOnDisk := m.editor.GetBuffer().Text()
		gotOnDisk, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("readback: %v", err)
		}
		if string(gotOnDisk) != wantOnDisk {
			t.Errorf("disk: got %q, want %q", gotOnDisk, wantOnDisk)
		}
		if m.dirty {
			t.Errorf("dirty should be false after flush")
		}
		if !m.lastSavedAt.Equal(stubTime) {
			t.Errorf("lastSavedAt: got %v, want %v (stubbed)", m.lastSavedAt, stubTime)
		}
	})

	t.Run("stale save-tick ignored by generation mismatch", func(t *testing.T) {
		dir := t.TempDir()
		path := writeFile(t, dir, "b.txt", "seed\n")
		m := newTestModel(t, dir)

		newM, _ := m.Update(openFileRequestMsg{path: path})
		m = newM.(*Model)

		// Two "edits" bump saveGen twice. The stale tick carries the earlier
		// gen and must be discarded.
		m.editor.GetBuffer().InsertAt(0, 0, "A")
		m.dirty = true
		m.saveGen++
		staleGen := m.saveGen

		m.editor.GetBuffer().InsertAt(0, 1, "B")
		m.saveGen++ // bump again — staleGen is now outdated

		// Record the disk contents before the stale tick so we can assert no
		// write happened.
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("pre-readback: %v", err)
		}

		_, _ = m.Update(saveTickMsg{gen: staleGen})

		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("post-readback: %v", err)
		}
		if string(before) != string(after) {
			t.Errorf("stale tick triggered a write: before=%q after=%q", before, after)
		}
		if !m.dirty {
			t.Errorf("dirty must remain true after ignored stale tick")
		}
	})

	t.Run("save-tick with no open file is no-op", func(t *testing.T) {
		m := newTestModel(t, t.TempDir())
		// No file opened. Tick with zeroed gen.
		newM, cmd := m.Update(saveTickMsg{gen: 0})
		if newM != m {
			t.Errorf("Update should return the same model")
		}
		if cmd != nil {
			t.Errorf("save-tick with no open file should produce nil cmd")
		}
	})

	t.Run("change-tick with no open file is no-op", func(t *testing.T) {
		m := newTestModel(t, t.TempDir())
		newM, cmd := m.Update(changeTickMsg{gen: 0})
		if newM != m {
			t.Errorf("Update should return the same model")
		}
		if cmd != nil {
			t.Errorf("change-tick with no open file should produce nil cmd")
		}
	})
}

// -----------------------------------------------------------------------------
// T4.3 Dirty-flag + quit safety regressions (commit 0ee96ea).

func TestModel_DirtyAndQuitSafety(t *testing.T) {
	t.Run("switching files flushes prior dirty buffer to disk", func(t *testing.T) {
		dir := t.TempDir()
		pathA := writeFile(t, dir, "a.txt", "original A\n")
		pathB := writeFile(t, dir, "b.txt", "original B\n")
		m := newTestModel(t, dir)

		// Open A and dirty it.
		newM, _ := m.Update(openFileRequestMsg{path: pathA})
		m = newM.(*Model)
		m.editor.GetBuffer().InsertAt(0, 0, "DIRTY-")
		m.dirty = true

		// Capture what the A buffer now contains — that's what openFile's
		// flush must persist to disk before swapping editors.
		wantA := m.editor.GetBuffer().Text()

		// Switch to B.
		newM, _ = m.Update(openFileRequestMsg{path: pathB})
		m = newM.(*Model)

		if m.openPath != pathB {
			t.Errorf("openPath after switch: got %q, want %q", m.openPath, pathB)
		}
		if m.dirty {
			t.Errorf("dirty must be false after switching files")
		}

		gotA, err := os.ReadFile(pathA)
		if err != nil {
			t.Fatalf("readback A: %v", err)
		}
		if string(gotA) != wantA {
			t.Errorf("A not flushed on switch: got %q, want %q", gotA, wantA)
		}
	})

	t.Run("guardedQuit flushes dirty buffer before quitting", func(t *testing.T) {
		dir := t.TempDir()
		path := writeFile(t, dir, "q.txt", "initial\n")
		m := newTestModel(t, dir)

		newM, _ := m.Update(openFileRequestMsg{path: path})
		m = newM.(*Model)
		m.editor.GetBuffer().InsertAt(0, 0, "DIRTY-")
		m.dirty = true
		wantOnDisk := m.editor.GetBuffer().Text()

		// Ctrl+Q is the user-facing trigger for guardedQuit. forceQuitMsg is
		// the internal sentinel handled at model.go:165.
		newM, cmd := m.Update(forceQuitMsg{})
		m = newM.(*Model)

		if m.dirty {
			t.Errorf("dirty must be false after guardedQuit's flush")
		}
		gotOnDisk, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("readback: %v", err)
		}
		if string(gotOnDisk) != wantOnDisk {
			t.Errorf("dirty buffer not flushed on quit: got %q, want %q", gotOnDisk, wantOnDisk)
		}
		// guardedQuit returns tea.Quit on success; executing the cmd should
		// yield tea's internal quit message. We assert cmd is non-nil (the
		// exact type is an unexported tea sentinel).
		if cmd == nil {
			t.Errorf("expected non-nil cmd (tea.Quit) from guardedQuit")
		}
	})

	t.Run("guardedQuit with no open file is safe", func(t *testing.T) {
		m := newTestModel(t, t.TempDir())
		newM, cmd := m.Update(forceQuitMsg{})
		if newM == nil {
			t.Fatalf("Update returned nil model")
		}
		// Even with no open file we still return tea.Quit.
		if cmd == nil {
			t.Errorf("expected non-nil cmd from guardedQuit")
		}
	})
}

// -----------------------------------------------------------------------------
// T4.4 Palette open/close/execute.

func TestModel_PaletteFlow(t *testing.T) {
	t.Run("Ctrl+P opens palette", func(t *testing.T) {
		m := newTestModel(t, t.TempDir())
		if m.palette.open {
			t.Fatalf("pre: palette should be closed")
		}
		newM, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlP})
		got := newM.(*Model)
		if !got.palette.open {
			t.Errorf("palette should be open after Ctrl+P")
		}
	})

	t.Run("every keystroke refilters palette", func(t *testing.T) {
		m := newTestModel(t, t.TempDir())
		// Seed a deterministic command set so refilter has predictable output.
		m.commands = map[string]plugin.CommandRef{
			"go.open":     {Title: "Open Go file", Plugin: "gofmt"},
			"format.file": {Title: "Format buffer", Plugin: "gofmt"},
			"tree.expand": {Title: "Expand node", Plugin: "tree"},
		}
		_, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlP})

		// Type "g" — should keep only entries containing "g".
		_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
		if m.palette.query != "g" {
			t.Errorf("after 'g': query=%q, want %q", m.palette.query, "g")
		}
		gotLen := len(m.palette.filtered)
		if gotLen == 0 || gotLen > 3 {
			t.Errorf("after 'g': filtered count suspicious: %d", gotLen)
		}

		// Type "o" — filter becomes "go".
		_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
		if m.palette.query != "go" {
			t.Errorf("after 'go': query=%q", m.palette.query)
		}

		// Typing "z" should drop results to zero.
		_, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'z'}})
		if m.palette.query != "goz" {
			t.Errorf("after 'goz': query=%q", m.palette.query)
		}
		if len(m.palette.filtered) != 0 {
			t.Errorf("after 'goz': expected 0 matches, got %d", len(m.palette.filtered))
		}
	})

	t.Run("Esc closes palette without executing", func(t *testing.T) {
		m := newTestModel(t, t.TempDir())
		m.commands = map[string]plugin.CommandRef{"a": {Title: "A"}}
		_, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlP})
		if !m.palette.open {
			t.Fatalf("pre: palette should be open")
		}
		_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
		if m.palette.open {
			t.Errorf("Esc should close palette")
		}
	})

	t.Run("Enter with registered command closes palette and dispatches to host", func(t *testing.T) {
		// The palette-to-execute flow is: handlePaletteKey invokes
		// host.ExecuteCommand(entry.ID, nil). ExecuteCommand requires the
		// command to appear in host.Commands(), which is populated via
		// host.handleInternal on a commands/register notif. The registration
		// is in an unexported method on an unexported channel in the plugin
		// package, and the plan constrains us from touching plugin/. So we
		// exercise the palette-close-on-Enter contract with a command that is
		// visible in the palette's snapshot but NOT in host.Commands(): the
		// palette still calls ExecuteCommand, ExecuteCommand returns an
		// "unknown command" error (in-proc fast path), handlePaletteKey
		// surfaces that via m.statusErr, and the palette closes regardless.
		// A deeper round-trip assertion awaits T5 / Recv-pushing infra.
		m := newTestModel(t, t.TempDir())
		m.commands = map[string]plugin.CommandRef{
			"do.it": {Title: "Do It", Plugin: "faker"},
		}
		_, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlP})
		if len(m.palette.filtered) != 1 {
			t.Fatalf("palette filtered count: got %d, want 1", len(m.palette.filtered))
		}
		_, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		if m.palette.open {
			t.Errorf("palette should be closed after Enter")
		}
		// ExecuteCommand("do.it") should have failed with "unknown command"
		// because host.Commands() was never populated. handlePaletteKey
		// records the error on m.statusErr.
		if !strings.Contains(m.statusErr, "unknown command") {
			t.Errorf("expected statusErr to mention unknown command; got %q", m.statusErr)
		}
	})
}

// -----------------------------------------------------------------------------
// T4.5 Plugin message plumbing into Update. We feed the inbound plugin
// messages (as if they arrived from Host.Recv) directly into Update and
// assert the model's response. Because Update always re-arms via host.Recv()
// on these paths, every returned cmd should be non-nil — but we don't execute
// it (that would block on the inbound channel).

func TestModel_PluginMessagePlumbing(t *testing.T) {
	t.Run("PluginStartedMsg is a no-op", func(t *testing.T) {
		m := newTestModel(t, t.TempDir())
		preSegs := len(m.statusSegments)
		newM, cmd := m.Update(plugin.PluginStartedMsg{Name: "plg"})
		got := newM.(*Model)
		// model.go deliberately treats PluginStartedMsg as a v1 no-op: it
		// does NOT add a status segment. Encode that contract.
		if len(got.statusSegments) != preSegs {
			t.Errorf("PluginStartedMsg should not mutate status segments; got %+v", got.statusSegments)
		}
		if cmd == nil {
			t.Errorf("PluginStartedMsg should return the Recv re-arm cmd; got nil")
		}
	})

	t.Run("PluginNotifMsg OpenFile effect not applicable via notif path", func(t *testing.T) {
		// The plumbing for "plugin asks to open a file" is the PluginReqMsg
		// openFile RPC, not a notification. A notif with method "openFile"
		// is just an unknown-method no-op in handlePluginNotif's switch.
		m := newTestModel(t, t.TempDir())
		newM, cmd := m.Update(plugin.PluginNotifMsg{
			Plugin: "p",
			Method: "openFile",
			Params: json.RawMessage(`{"path":"/x"}`),
		})
		got := newM.(*Model)
		if got.openPath != "" {
			t.Errorf("unknown notif method must not change openPath; got %q", got.openPath)
		}
		if cmd == nil {
			t.Errorf("notif path should always re-arm host.Recv")
		}
	})

	t.Run("PluginNotifMsg statusBar/set upserts segment, empty text removes", func(t *testing.T) {
		m := newTestModel(t, t.TempDir())
		params, _ := json.Marshal(map[string]string{
			"segment": "lint",
			"text":    "0 errors",
			"color":   "42",
		})
		_, _ = m.Update(plugin.PluginNotifMsg{
			Plugin: "linter",
			Method: "statusBar/set",
			Params: params,
		})
		if len(m.statusSegments) != 1 {
			t.Fatalf("after set: want 1 segment, got %d", len(m.statusSegments))
		}
		seg := m.statusSegments[0]
		if seg.Plugin != "linter" || seg.Name != "lint" || seg.Text != "0 errors" {
			t.Errorf("segment mismatch: %+v", seg)
		}

		// Upsert same plugin+segment with new text — in-place update.
		params2, _ := json.Marshal(map[string]string{
			"segment": "lint",
			"text":    "3 errors",
			"color":   "196",
		})
		_, _ = m.Update(plugin.PluginNotifMsg{
			Plugin: "linter",
			Method: "statusBar/set",
			Params: params2,
		})
		if len(m.statusSegments) != 1 {
			t.Fatalf("after upsert: want 1, got %d", len(m.statusSegments))
		}
		if m.statusSegments[0].Text != "3 errors" {
			t.Errorf("upsert did not update text; got %q", m.statusSegments[0].Text)
		}

		// Empty text removes.
		paramsRm, _ := json.Marshal(map[string]string{
			"segment": "lint",
			"text":    "",
		})
		_, _ = m.Update(plugin.PluginNotifMsg{
			Plugin: "linter",
			Method: "statusBar/set",
			Params: paramsRm,
		})
		if len(m.statusSegments) != 0 {
			t.Errorf("empty-text upsert should remove segment; got %+v", m.statusSegments)
		}
	})

	t.Run("PluginNotifMsg ui/notify produces a status-message cmd", func(t *testing.T) {
		m := newTestModel(t, t.TempDir())
		params, _ := json.Marshal(map[string]string{
			"level":   "info",
			"message": "hi",
		})
		_, cmd := m.Update(plugin.PluginNotifMsg{
			Plugin: "p",
			Method: "ui/notify",
			Params: params,
		})
		// The batched cmd includes the status-message cmd + host.Recv. Non-nil
		// is the observable signal.
		if cmd == nil {
			t.Errorf("ui/notify with non-empty message should produce a cmd")
		}
	})

	t.Run("PluginNotifMsg pane/invalidate is a no-op re-arm", func(t *testing.T) {
		m := newTestModel(t, t.TempDir())
		_, cmd := m.Update(plugin.PluginNotifMsg{
			Plugin: "p",
			Method: "pane/invalidate",
			Params: json.RawMessage(`{}`),
		})
		if cmd == nil {
			t.Errorf("pane/invalidate should still return host.Recv re-arm")
		}
	})

	t.Run("PluginExitedMsg drops plugin's segments and surfaces error", func(t *testing.T) {
		m := newTestModel(t, t.TempDir())
		m.statusSegments = []statusSegment{
			{Plugin: "gone", Name: "a", Text: "x"},
			{Plugin: "keep", Name: "b", Text: "y"},
			{Plugin: "gone", Name: "c", Text: "z"},
		}
		_, _ = m.Update(plugin.PluginExitedMsg{Name: "gone"})
		if len(m.statusSegments) != 1 {
			t.Fatalf("want 1 survivor, got %d: %+v", len(m.statusSegments), m.statusSegments)
		}
		if m.statusSegments[0].Plugin != "keep" {
			t.Errorf("wrong survivor: %+v", m.statusSegments[0])
		}
	})

	t.Run("PluginExitedMsg with Err sets statusErr", func(t *testing.T) {
		m := newTestModel(t, t.TempDir())
		_, _ = m.Update(plugin.PluginExitedMsg{Name: "crashed", Err: errSentinel{}})
		if !strings.Contains(m.statusErr, "crashed") {
			t.Errorf("statusErr should mention plugin name; got %q", m.statusErr)
		}
	})

	t.Run("PluginReqMsg buffer/edit on open file updates editor", func(t *testing.T) {
		dir := t.TempDir()
		path := writeFile(t, dir, "re.txt", "seed\n")
		m := newTestModel(t, dir)
		newM, _ := m.Update(openFileRequestMsg{path: path})
		m = newM.(*Model)

		params, _ := json.Marshal(map[string]string{
			"path": path,
			"text": "replaced by plugin\n",
		})
		_, cmd := m.Update(plugin.PluginReqMsg{
			Plugin: "editor-bot",
			ID:     "req-1",
			Method: "buffer/edit",
			Params: params,
		})
		if cmd == nil {
			t.Errorf("buffer/edit should batch editor.Init + host.Recv; got nil")
		}
		if got := m.editor.GetBuffer().Text(); got != "replaced by plugin\n" {
			t.Errorf("buffer not replaced; got %q", got)
		}
		if m.dirty {
			t.Errorf("dirty should be false after plugin-driven edit")
		}
	})

	t.Run("PluginReqMsg buffer/edit on wrong path responds invalidParams", func(t *testing.T) {
		m := newTestModel(t, t.TempDir())
		// No open file — any path is "wrong".
		params, _ := json.Marshal(map[string]string{
			"path": "/nope",
			"text": "x",
		})
		newM, cmd := m.Update(plugin.PluginReqMsg{
			Plugin: "p",
			ID:     "req-x",
			Method: "buffer/edit",
			Params: params,
		})
		got := newM.(*Model)
		if got.openPath != "" {
			t.Errorf("openPath must not change on rejected buffer/edit; got %q", got.openPath)
		}
		if cmd == nil {
			t.Errorf("expected Recv re-arm cmd even on error path")
		}
	})

	t.Run("PluginReqMsg unknown method returns host.Recv re-arm", func(t *testing.T) {
		m := newTestModel(t, t.TempDir())
		_, cmd := m.Update(plugin.PluginReqMsg{
			Plugin: "p",
			ID:     "req-unknown",
			Method: "not-a-real-method",
		})
		if cmd == nil {
			t.Errorf("unknown request method should still re-arm Recv")
		}
	})

	t.Run("PluginResponseMsg is a no-op that re-arms", func(t *testing.T) {
		m := newTestModel(t, t.TempDir())
		_, cmd := m.Update(plugin.PluginResponseMsg{
			Plugin: "p",
			ID:     int64(1),
		})
		if cmd == nil {
			t.Errorf("PluginResponseMsg should re-arm Recv; got nil cmd")
		}
	})
}

// errSentinel is a tiny error type used where we need a non-nil error value
// in a PluginExitedMsg without pulling in errors.New on every call.
type errSentinel struct{}

func (errSentinel) Error() string { return "crashed: simulated" }
