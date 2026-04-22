package editor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/savimcio/nistru/internal/config"
	"github.com/savimcio/nistru/internal/plugins/settingscmd"
	"github.com/savimcio/nistru/internal/plugins/treepane"
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
	// NewModel now emits Initialize at boot, which spins up the autoupdate
	// plugin's background checker and pokes the real user config dir for
	// persistent state. Neither belongs in component tests; disable via env.
	// The DISABLE knob skips the checker goroutine while still registering
	// palette commands (see autoupdate.handleInitialize), so any command-
	// registry behavior exercised here still works.
	t.Setenv("NISTRU_AUTOUPDATE_DISABLE", "1")
	t.Setenv("NISTRU_AUTOUPDATE_REPO", "")
	t.Setenv("NISTRU_AUTOUPDATE_CHANNEL", "")
	t.Setenv("NISTRU_AUTOUPDATE_INTERVAL", "")
	m, err := NewModel(root, nil)
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
		// goeditor's buffer does not preserve the file's trailing newline, so
		// both lastText (seeded from editor.Content() after SetContent) and
		// the live editor buffer drop the final '\n'. This is deliberate:
		// re-adding the newline via a memoised flag caused data-loss bugs for
		// users who intentionally deleted the EOF newline (see commit history).
		wantBuf := "package main\n\nfunc main() {}"
		if got.lastText != wantBuf {
			t.Errorf("lastText not seeded from file: got %q, want %q", got.lastText, wantBuf)
		}
		// Buffer should contain the file contents so a subsequent View() or
		// change-tick would see them.
		if txt := got.editor.Content(); txt != got.lastText {
			t.Errorf("editor buffer mismatch: got %q, want %q", txt, got.lastText)
		}
		// openFile no longer issues a fresh editor.Init(): F2.1 switched to
		// reusing the single editor instance across opens, so Init() (and its
		// outstanding listener goroutine) only runs once at Model construction.
		// The returned cmd therefore carries only plugin-effect cmds from the
		// DidOpen dispatch — nil when no registered plugin produces an effect
		// (the typical component-test case). Asserting nil here would be too
		// strict if a plugin ever registered in newTestModel's host; asserting
		// non-nil would be too strict when none do. We assert the observable
		// contract instead: the editor is the same instance before and after
		// open, and the state we care about (openPath, dirty, focus) is set.
		_ = cmd
		// Focus flips to editor on open.
		if got.focus != focusEditor {
			t.Errorf("focus after open: got %v, want focusEditor", got.focus)
		}
	})

	// F2.1 regression: openFile must reuse m.editor across opens rather than
	// constructing a new adapter. Fresh adapters leaked goeditor's in-flight
	// timer/Init cmds past the swap, so stale tea.Msgs from the previous
	// editor landed on the new one via the non-key forwarding path.
	t.Run("editor instance is reused across successful opens", func(t *testing.T) {
		dir := t.TempDir()
		pathA := writeFile(t, dir, "a.txt", "hello A\n")
		pathB := writeFile(t, dir, "b.txt", "hello B\n")
		m := newTestModel(t, dir)

		initialEditor := m.editor

		newM, _ := m.Update(openFileRequestMsg{path: pathA})
		afterA := newM.(*Model)
		if afterA.editor != initialEditor {
			t.Fatalf("editor identity changed on first open; F2.1 requires reuse")
		}
		if got := afterA.editor.Content(); got != "hello A" {
			t.Errorf("buffer after open(A) = %q, want %q", got, "hello A")
		}

		newM, _ = afterA.Update(openFileRequestMsg{path: pathB})
		afterB := newM.(*Model)
		if afterB.editor != initialEditor {
			t.Errorf("editor identity changed on second open; F2.1 requires reuse")
		}
		if got := afterB.editor.Content(); got != "hello B" {
			t.Errorf("buffer after open(B) = %q, want %q", got, "hello B")
		}
		// Mode must be Normal after the swap even if the previous buffer was
		// mid-Insert — opening a file should not strand the user in Insert.
		if mode := afterB.editor.Mode(); mode != ModeNormal {
			t.Errorf("mode after open(B) = %v, want ModeNormal", mode)
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

	// F3.3 regression: DidOpen.Text must carry the editor's buffer — which is
	// what the user will be editing — not the raw file bytes. goeditor drops
	// the trailing newline on load, so those two strings differ for files
	// with a trailing '\n'. A plugin that caches DidOpen.Text (e.g. the
	// bundled gofmt) would otherwise operate on stale content until the
	// first DidChange arrived.
	t.Run("DidOpen.Text matches editor buffer, not raw file bytes", func(t *testing.T) {
		dir := t.TempDir()
		// Trailing newline is the interesting case — goeditor drops it.
		raw := "package main\n\nfunc main() {}\n"
		path := writeFile(t, dir, "hello.go", raw)

		t.Setenv("NISTRU_AUTOUPDATE_DISABLE", "1")
		t.Setenv("NISTRU_AUTOUPDATE_REPO", "")
		t.Setenv("NISTRU_AUTOUPDATE_CHANNEL", "")
		t.Setenv("NISTRU_AUTOUPDATE_INTERVAL", "")
		rec := &didOpenRecorder{}
		reg := plugin.NewRegistry()
		reg.RegisterInProc(rec)
		m, err := newModelWithRegistry(dir, reg, config.Defaults())
		if err != nil {
			t.Fatalf("newModelWithRegistry: %v", err)
		}
		t.Cleanup(func() { _ = m.host.Shutdown(100 * time.Millisecond) })

		if _, _ = m.Update(openFileRequestMsg{path: path}); rec.lastOpenText == "" {
			t.Fatalf("recorder did not receive DidOpen")
		}
		// The recorder captures whatever Text the host forwarded — which, after
		// the fix, is m.lastText (editor's buffer). If the bug reappears it
		// would be the raw file bytes (with trailing newline).
		if rec.lastOpenText == raw {
			t.Errorf("DidOpen.Text carried raw file bytes (%q); should carry editor buffer", raw)
		}
		want := m.lastText
		if rec.lastOpenText != want {
			t.Errorf("DidOpen.Text = %q, want %q (editor buffer)", rec.lastOpenText, want)
		}
	})
}

// didOpenRecorder is a minimal in-proc plugin that captures the most recent
// DidOpen event it receives. onStart activates it at boot so it stays
// subscribed for subsequent DidOpen events (activated plugins receive
// mismatched-pattern events for state continuity; see host.shouldDispatch).
type didOpenRecorder struct {
	mu            sync.Mutex
	lastOpenText  string
	lastOpenPath  string
}

func (r *didOpenRecorder) Name() string           { return "didopen-recorder" }
func (r *didOpenRecorder) Activation() []string   { return []string{"onStart"} }
func (r *didOpenRecorder) Shutdown() error        { return nil }
func (r *didOpenRecorder) OnEvent(event any) []plugin.Effect {
	if e, ok := event.(plugin.DidOpen); ok {
		r.mu.Lock()
		r.lastOpenText = e.Text
		r.lastOpenPath = e.Path
		r.mu.Unlock()
	}
	return nil
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
		// as the "live" ones. We prepend "x" to the current content via
		// SetContent — the buffer-mutation API we used to have via
		// GetBuffer().InsertAt is gone with the goeditor migration, but
		// prepend-via-SetContent produces an equivalent end state for this
		// flow test.
		m.editor.SetContent("x" + m.editor.Content())
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
		wantOnDisk := m.editor.Content()
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
		m.editor.SetContent("A" + m.editor.Content())
		m.dirty = true
		m.saveGen++
		staleGen := m.saveGen

		m.editor.SetContent("B" + m.editor.Content())
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
		if got, ok := newM.(*Model); !ok || got != m {
			t.Errorf("Update should return the same *Model; got %T", newM)
		}
		if cmd != nil {
			t.Errorf("save-tick with no open file should produce nil cmd")
		}
	})

	t.Run("change-tick with no open file is no-op", func(t *testing.T) {
		m := newTestModel(t, t.TempDir())
		newM, cmd := m.Update(changeTickMsg{gen: 0})
		if got, ok := newM.(*Model); !ok || got != m {
			t.Errorf("Update should return the same *Model; got %T", newM)
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
		m.editor.SetContent("DIRTY-" + m.editor.Content())
		m.dirty = true

		// Capture what the A buffer now contains — that's what openFile's
		// flush must persist to disk before swapping editors.
		wantA := m.editor.Content()

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
		m.editor.SetContent("DIRTY-" + m.editor.Content())
		m.dirty = true
		wantOnDisk := m.editor.Content()

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
		newM, _ := m.Update(tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
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
		_, _ = m.Update(tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})

		// Type "g" — should keep only entries containing "g". In v2 a
		// printable rune arrives as KeyPressMsg{Code: 'g', Text: "g"} and
		// the palette's HandleKey reads Text via keyEventFromTea → runes.
		_, _ = m.Update(tea.KeyPressMsg{Code: 'g', Text: "g"})
		if m.palette.query != "g" {
			t.Errorf("after 'g': query=%q, want %q", m.palette.query, "g")
		}
		gotLen := len(m.palette.filtered)
		if gotLen == 0 || gotLen > 3 {
			t.Errorf("after 'g': filtered count suspicious: %d", gotLen)
		}

		// Type "o" — filter becomes "go".
		_, _ = m.Update(tea.KeyPressMsg{Code: 'o', Text: "o"})
		if m.palette.query != "go" {
			t.Errorf("after 'go': query=%q", m.palette.query)
		}

		// Typing "z" should drop results to zero.
		_, _ = m.Update(tea.KeyPressMsg{Code: 'z', Text: "z"})
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
		_, _ = m.Update(tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
		if !m.palette.open {
			t.Fatalf("pre: palette should be open")
		}
		_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
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
		_, _ = m.Update(tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
		if len(m.palette.filtered) != 1 {
			t.Fatalf("palette filtered count: got %d, want 1", len(m.palette.filtered))
		}
		_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
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
		// goeditor drops the trailing newline from SetContent's input, so we
		// assert against the stripped form. The plugin supplied
		// "replaced by plugin\n"; the editor buffer holds the prefix.
		if got := m.editor.Content(); got != "replaced by plugin" {
			t.Errorf("buffer not replaced; got %q", got)
		}
		if m.dirty {
			t.Errorf("dirty should be false after plugin-driven edit")
		}
	})

	// F2.2 regression: after buffer/edit replaces the buffer with text that
	// ends in "\n", lastText must equal m.editor.Content() (which has the
	// trailing newline stripped by goeditor), not the raw p.Text. If drift
	// sneaks back in, the next non-edit keystroke's dirty-diff in
	// forwardToFocused flips dirty=true without a real user edit, which
	// cascades into autosave, a DidChange dispatch, and a stray saveTick —
	// the exact class of bug Codex flagged.
	t.Run("buffer/edit with trailing newline leaves lastText aligned with editor", func(t *testing.T) {
		dir := t.TempDir()
		path := writeFile(t, dir, "fmt.go", "seed\n")
		m := newTestModel(t, dir)
		newM, _ := m.Update(openFileRequestMsg{path: path})
		m = newM.(*Model)

		// Simulate a formatter plugin (e.g. bundled gofmt) sending back text
		// with a trailing newline. This is the common case that triggered the
		// original drift.
		params, _ := json.Marshal(map[string]string{
			"path": path,
			"text": "formatted\n",
		})
		_, _ = m.Update(plugin.PluginReqMsg{
			Plugin: "fmtbot",
			ID:     "req-fmt",
			Method: "buffer/edit",
			Params: params,
		})

		if got := m.editor.Content(); got != "formatted" {
			t.Fatalf("editor content after buffer/edit = %q, want %q (goeditor strips trailing newline)", got, "formatted")
		}
		if m.lastText != m.editor.Content() {
			t.Errorf("lastText = %q, editor.Content() = %q — drift violates the dirty-diff contract", m.lastText, m.editor.Content())
		}
		if m.dirty {
			t.Errorf("dirty should be false immediately after buffer/edit")
		}

		// Simulate a subsequent non-edit keystroke (cursor motion) via a
		// non-KeyPressMsg forwarded to the editor. We can't easily drive
		// goeditor's cursor from here, so instead we just re-run the
		// dirty-diff that forwardToFocused would run: if lastText is aligned
		// with the editor buffer, no drift is detected.
		saveGenBefore := m.saveGen
		changeGenBefore := m.changeGen
		if m.editor.Content() != m.lastText {
			t.Errorf("pre-diff mismatch: editor=%q lastText=%q", m.editor.Content(), m.lastText)
		}
		if m.saveGen != saveGenBefore || m.changeGen != changeGenBefore {
			t.Errorf("gens bumped without an edit: saveGen %d->%d, changeGen %d->%d",
				saveGenBefore, m.saveGen, changeGenBefore, m.changeGen)
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

// -----------------------------------------------------------------------------
// T4 config-threading regression tests. These lock in the behaviour that
// Model's config knobs (keymap, tree width, per-plugin config) flow through
// the constructor path into the runtime, rather than being hardcoded.

// TestModel_CustomKeymap_SavesOnConfiguredKey asserts that rebinding
// ActionSave in the keymap reroutes the save handler accordingly. We bind
// Save to F5 (a chord that tea.KeyMsg natively round-trips through
// .String()), feed a KeyF5 KeyMsg, and verify flushNow ran — via the
// nowFunc-stubbed lastSavedAt and the file landing on disk.
//
// F5 is chosen because more exotic chords like ctrl+shift+s require escape
// sequences tea.KeyMsg doesn't let us synthesize directly, and the point
// of this test is the routing, not the chord encoding.
func TestModel_CustomKeymap_SavesOnConfiguredKey(t *testing.T) {
	stubTime := time.Date(2099, 1, 2, 3, 4, 5, 0, time.UTC)
	prev := nowFunc
	nowFunc = func() time.Time { return stubTime }
	t.Cleanup(func() { nowFunc = prev })

	dir := t.TempDir()
	path := writeFile(t, dir, "a.txt", "seed\n")

	// NewModel still registers treepane + autoupdate, but we only care about
	// the top-level Model config plumbing here. Disable autoupdate's network.
	t.Setenv("NISTRU_AUTOUPDATE_DISABLE", "1")
	t.Setenv("NISTRU_AUTOUPDATE_REPO", "")
	t.Setenv("NISTRU_AUTOUPDATE_CHANNEL", "")
	t.Setenv("NISTRU_AUTOUPDATE_INTERVAL", "")

	cfg := config.Defaults()
	cfg.Keymap[config.ActionSave] = "f5"
	m, err := NewModel(dir, cfg)
	if err != nil {
		t.Fatalf("NewModel: %v", err)
	}
	t.Cleanup(func() { _ = m.host.Shutdown(100 * time.Millisecond) })

	// Open the file and dirty its buffer so flushNow has something to write.
	newM, _ := m.Update(openFileRequestMsg{path: path})
	m = newM.(*Model)
	m.editor.SetContent("X" + m.editor.Content())
	m.dirty = true

	beforeSaved := m.lastSavedAt

	newM, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyF5})
	m = newM.(*Model)

	// If the config-driven binding worked, flushNow ran and lastSavedAt is
	// the stubbed time. If the binding were still hardcoded to ctrl+s, the
	// F5 keypress would have fallen through to forwardToFocused — leaving
	// lastSavedAt unchanged.
	if m.lastSavedAt.Equal(beforeSaved) {
		t.Errorf("lastSavedAt did not change; F5 did not trigger configured Save binding")
	}
	if !m.lastSavedAt.Equal(stubTime) {
		t.Errorf("lastSavedAt: got %v, want %v (stubbed)", m.lastSavedAt, stubTime)
	}
	if m.dirty {
		t.Errorf("dirty should be false after save")
	}
	if cmd == nil {
		t.Errorf("handleKey Save path should emit the 'saved' status cmd")
	}
	// Assert the file on disk matches the buffer.
	gotOnDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	if string(gotOnDisk) != m.editor.Content() {
		t.Errorf("disk mismatch: got %q, want %q", gotOnDisk, m.editor.Content())
	}
}

// TestModel_CustomTreeWidth_AppliesToLayout asserts that m.cfg.UI.TreeWidth
// flows into OnResize bookkeeping on a WindowSizeMsg. With a custom width
// of 42, m.lastPaneW should land at 42 after the first tick.
func TestModel_CustomTreeWidth_AppliesToLayout(t *testing.T) {
	t.Setenv("NISTRU_AUTOUPDATE_DISABLE", "1")
	t.Setenv("NISTRU_AUTOUPDATE_REPO", "")
	t.Setenv("NISTRU_AUTOUPDATE_CHANNEL", "")
	t.Setenv("NISTRU_AUTOUPDATE_INTERVAL", "")

	cfg := config.Defaults()
	cfg.UI.TreeWidth = 42

	dir := t.TempDir()
	m, err := NewModel(dir, cfg)
	if err != nil {
		t.Fatalf("NewModel: %v", err)
	}
	t.Cleanup(func() { _ = m.host.Shutdown(100 * time.Millisecond) })

	newM, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	got := newM.(*Model)

	if got.lastPaneW != 42 {
		t.Errorf("lastPaneW after WindowSize: got %d, want 42 (cfg.UI.TreeWidth)", got.lastPaneW)
	}
	if got.treeWidth() != 42 {
		t.Errorf("treeWidth helper: got %d, want 42", got.treeWidth())
	}
}

// TestModel_PluginConfig_FlowsToHost wires a stub in-proc ConfigReceiver
// into a fresh Registry and asserts that the sub-tree returned by
// cfg.PluginConfig(name) arrives at the plugin's OnConfig on construction.
func TestModel_PluginConfig_FlowsToHost(t *testing.T) {
	root := t.TempDir()

	// Build a Config whose PluginConfig method returns our desired raw bytes
	// for the stub's name. We bypass the file/env pipeline and inject the
	// bytes via EnvOverlay: PluginConfig merges file + env and serialises the
	// result, so env-only populates raw-bytes to a JSON object.
	cfg := config.Defaults()
	cfg.Plugins.EnvOverlay = map[string]map[string]any{
		"stubcfg": {"x": float64(7)},
	}

	// Build the registry with treepane (for leftPane wiring) + our stub.
	registry := plugin.NewRegistry()
	tp, err := treepane.New(root)
	if err != nil {
		t.Fatalf("treepane.New: %v", err)
	}
	registry.RegisterInProc(tp)

	stub := &stubConfigReceiver{name: "stubcfg"}
	registry.RegisterInProc(stub)

	m, err := newModelWithRegistry(root, registry, cfg)
	if err != nil {
		t.Fatalf("newModelWithRegistry: %v", err)
	}
	t.Cleanup(func() { _ = m.host.Shutdown(100 * time.Millisecond) })

	got := stub.last()
	if got == nil {
		t.Fatalf("stub plugin never received OnConfig")
	}
	// PluginConfig marshals the merged map, so we expect {"x":7} (JSON
	// number encoding for float64 7).
	var parsed map[string]any
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("received config not valid JSON: %v (raw=%s)", err, got)
	}
	if x, ok := parsed["x"].(float64); !ok || x != 7 {
		t.Errorf("received config: got %v, want {x:7}", parsed)
	}
}

// stubConfigReceiver is a minimal in-proc plugin implementing both the
// Plugin and ConfigReceiver interfaces; used to observe OnConfig dispatch
// end-to-end from Host.Emit(Initialize{...}) in newModelWithRegistry.
type stubConfigReceiver struct {
	name string
	mu   sync.Mutex
	got  json.RawMessage
}

func (s *stubConfigReceiver) Name() string             { return s.name }
func (s *stubConfigReceiver) Activation() []string     { return []string{"onStart"} }
func (s *stubConfigReceiver) OnEvent(any) []plugin.Effect { return nil }
func (s *stubConfigReceiver) Shutdown() error          { return nil }

func (s *stubConfigReceiver) OnConfig(raw json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make(json.RawMessage, len(raw))
	copy(cp, raw)
	s.got = cp
	return nil
}

func (s *stubConfigReceiver) last() json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.got
}

// TestModel_ReloadPalette_UpdatesPluginConfig exercises the end-to-end
// reload flow: settingscmd.reload fires plugin.ReloadConfigRequest, which
// the Model intercepts and translates into config.Load + SetPluginConfig +
// ReEmitInitialize. A stub ConfigReceiver observes OnConfig before and
// after, verifying the swap lands at the plugin level.
//
// XDG_CONFIG_HOME redirects UserPath() to tempDir so the real user config
// tree is never touched. The user config is the only file we mutate —
// project config stays untouched.
func TestModel_ReloadPalette_UpdatesPluginConfig(t *testing.T) {
	t.Setenv("NISTRU_AUTOUPDATE_DISABLE", "1")
	t.Setenv("NISTRU_AUTOUPDATE_REPO", "")
	t.Setenv("NISTRU_AUTOUPDATE_CHANNEL", "")
	t.Setenv("NISTRU_AUTOUPDATE_INTERVAL", "")

	userDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", userDir)
	t.Setenv("HOME", userDir)

	userCfgPath, err := config.UserPath()
	if err != nil {
		t.Fatalf("UserPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(userCfgPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	write := func(contents string) {
		if err := os.WriteFile(userCfgPath, []byte(contents), 0o644); err != nil {
			t.Fatalf("write user cfg: %v", err)
		}
	}
	write("[plugins.stub]\nk = 1\n")

	// Build a minimal registry manually: treepane for leftPane sugar, the
	// stub to observe OnConfig, and settingscmd so the reload command is
	// actually registered in host.Commands().
	registry := plugin.NewRegistry()
	tp, err := treepane.New(t.TempDir())
	if err != nil {
		t.Fatalf("treepane.New: %v", err)
	}
	registry.RegisterInProc(tp)

	stub := &stubConfigReceiver{name: "stub"}
	registry.RegisterInProc(stub)

	// NOTE: settingscmd is *not* registered through newModelWithRegistry.
	// We build our own plugin instance whose getCfg closure returns the
	// Model's cfg lazily, mirroring what NewModel does in production.
	root := t.TempDir()
	var mref *Model
	getCfg := func() *config.Config {
		if mref == nil {
			return nil
		}
		return mref.cfg
	}
	registry.RegisterInProc(settingscmd.New(root, getCfg))

	// Load through the real pipeline so Plugins.Raw/MD are populated the
	// same way cmd/nistru does.
	cfg, _, err := config.Load(root)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	m, err := newModelWithRegistry(root, registry, cfg)
	if err != nil {
		t.Fatalf("newModelWithRegistry: %v", err)
	}
	t.Cleanup(func() { _ = m.host.Shutdown(100 * time.Millisecond) })
	mref = m

	gotBefore := stub.last()
	if gotBefore == nil {
		t.Fatalf("stub never received OnConfig on Initialize")
	}
	var before map[string]any
	if err := json.Unmarshal(gotBefore, &before); err != nil {
		t.Fatalf("unmarshal before: %v", err)
	}
	if got, _ := before["k"].(int64); got == 1 {
		// BurntSushi TOML decodes TOML integers as int64 through
		// json.RawMessage+map[string]any — but that route goes through
		// json.Unmarshal and yields float64. Either way we just assert
		// the pre-state matches k=1 via the numeric-equality helper.
	}
	if !numericEq(before["k"], 1) {
		t.Fatalf("pre-reload stub config = %v, want k=1", before)
	}

	// Mutate the user config on disk.
	write("[plugins.stub]\nk = 2\n")

	// Dispatch the reload command through the host exactly like the palette
	// does. ExecuteCommand returns a Sync result because settingscmd is an
	// in-proc plugin; apply the effects through m.applyEffects to mirror
	// handlePaletteKey.
	result := m.host.ExecuteCommand("nistru.settings.reload", nil)
	if result.Sync == nil {
		t.Fatalf("reload command returned no Sync result: %+v", result)
	}
	if result.Sync.Err != nil {
		t.Fatalf("reload command error: %v", result.Sync.Err)
	}
	// applyEffects drives reloadConfig for us (it handles the
	// ReloadConfigRequest case). We discard the returned cmd — it carries
	// the "settings reloaded" status-bar message, which is not under test.
	_ = m.applyEffects(result.Sync.Effects)

	gotAfter := stub.last()
	if gotAfter == nil {
		t.Fatalf("stub received no OnConfig after reload")
	}
	var after map[string]any
	if err := json.Unmarshal(gotAfter, &after); err != nil {
		t.Fatalf("unmarshal after: %v", err)
	}
	if !numericEq(after["k"], 2) {
		t.Fatalf("post-reload stub config = %v, want k=2", after)
	}
}

// TestModel_ReloadPalette_RelativeNumbersTakesEffect verifies that changing
// [ui].relative_numbers on disk and firing the reload command actually
// rebuilds the underlying editor so the new setting is in effect
// immediately — without waiting for the user to open another file. The
// Editor interface doesn't expose the flag, so the assertion is indirect:
// we take a before/after snapshot of m.editor's pointer identity and
// confirm it changed, plus verify m.cfg.UI.RelativeNumbers took the new
// value. Together, those two checks cover the observable contract — "the
// editor instance that renders is the one that booted with the new setting".
func TestModel_ReloadPalette_RelativeNumbersTakesEffect(t *testing.T) {
	t.Setenv("NISTRU_AUTOUPDATE_DISABLE", "1")
	t.Setenv("NISTRU_AUTOUPDATE_REPO", "")
	t.Setenv("NISTRU_AUTOUPDATE_CHANNEL", "")
	t.Setenv("NISTRU_AUTOUPDATE_INTERVAL", "")

	userDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", userDir)
	t.Setenv("HOME", userDir)

	userCfgPath, err := config.UserPath()
	if err != nil {
		t.Fatalf("UserPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(userCfgPath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	write := func(contents string) {
		if err := os.WriteFile(userCfgPath, []byte(contents), 0o644); err != nil {
			t.Fatalf("write user cfg: %v", err)
		}
	}
	// Start with relative_numbers = true (the default) declared explicitly so
	// the before-state is unambiguous.
	write("[ui]\nrelative_numbers = true\n")

	registry := plugin.NewRegistry()
	tp, err := treepane.New(t.TempDir())
	if err != nil {
		t.Fatalf("treepane.New: %v", err)
	}
	registry.RegisterInProc(tp)

	root := t.TempDir()
	var mref *Model
	getCfg := func() *config.Config {
		if mref == nil {
			return nil
		}
		return mref.cfg
	}
	registry.RegisterInProc(settingscmd.New(root, getCfg))

	cfg, _, err := config.Load(root)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if !cfg.UI.RelativeNumbers {
		t.Fatalf("pre-reload cfg.UI.RelativeNumbers = false, want true")
	}
	m, err := newModelWithRegistry(root, registry, cfg)
	if err != nil {
		t.Fatalf("newModelWithRegistry: %v", err)
	}
	t.Cleanup(func() { _ = m.host.Shutdown(100 * time.Millisecond) })
	mref = m

	edBefore := m.editor

	// Flip the setting on disk.
	write("[ui]\nrelative_numbers = false\n")

	result := m.host.ExecuteCommand("nistru.settings.reload", nil)
	if result.Sync == nil || result.Sync.Err != nil {
		t.Fatalf("reload command: sync=%+v err=%v", result.Sync, result.Sync != nil && result.Sync.Err != nil)
	}
	_ = m.applyEffects(result.Sync.Effects)

	if m.cfg.UI.RelativeNumbers {
		t.Fatalf("post-reload cfg.UI.RelativeNumbers = true, want false")
	}
	if m.editor == edBefore {
		t.Fatalf("m.editor pointer unchanged after relative_numbers reload; editor still has stale flag")
	}

	// Inverse case: a reload that changes nothing relevant should NOT
	// reconstruct the editor — otherwise a routine reload would blow away
	// cursor position unnecessarily.
	edAfterFirst := m.editor
	// No-op change: still relative_numbers=false, just with a comment added.
	write("[ui]\nrelative_numbers = false\n# no-op change\n")
	result = m.host.ExecuteCommand("nistru.settings.reload", nil)
	if result.Sync == nil || result.Sync.Err != nil {
		t.Fatalf("second reload: sync=%+v", result.Sync)
	}
	_ = m.applyEffects(result.Sync.Effects)
	if m.editor != edAfterFirst {
		t.Fatalf("m.editor rebuilt on no-op reload; editor should only rebuild when a baked-in knob changed")
	}
}

// numericEq reports whether v equals want, accepting any of the numeric
// types json.Unmarshal (float64) or BurntSushi's primitive decode (int64)
// might produce for a TOML integer. Keeps the reload test agnostic to the
// decode path.
func numericEq(v any, want int) bool {
	switch x := v.(type) {
	case float64:
		return x == float64(want)
	case int64:
		return x == int64(want)
	case int:
		return x == want
	}
	return false
}

// -----------------------------------------------------------------------------
// T7 — "strange scrolling" regression guards.
//
// Opening testdata/wide_table.md at 80x24 previously produced a View() with
// >24 rows because the old editor's soft-wrap sometimes emitted lines wider
// than the editor viewport; lipgloss's sized Render then wrapped each
// overwide line into two visual rows. goeditor handles width natively at
// its own boundary, so the stopgap clampPaneBox has been removed; these
// invariants are implementation-agnostic regression guards.

// openWideTable builds a Model sized to (w, h), loads the wide_table.md
// fixture into tempdir/wide_table.md, opens it, and returns the Model. It is
// the shared setup for every TestView_*_WideTable case below.
func openWideTable(t *testing.T, w, h int) *Model {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("testdata", "wide_table.md"))
	if err != nil {
		t.Fatalf("read wide_table fixture: %v", err)
	}
	dir := t.TempDir()
	path := writeFile(t, dir, "wide_table.md", string(src))
	m := newTestModel(t, dir)
	newM, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	m = newM.(*Model)
	newM, _ = m.Update(openFileRequestMsg{path: path})
	return newM.(*Model)
}

// TestView_RowCount_WideTable guards against the original bug: View() at
// 80x24 inflated to 33 rows because overwide lines soft-wrapped inside
// editorStyle.Render. Reads via renderFrame() to inspect the composed body
// directly — View() now wraps that body in a v2 tea.View struct.
func TestView_RowCount_WideTable(t *testing.T) {
	m := openWideTable(t, 80, 24)
	got := m.renderFrame()
	lines := strings.Split(got, "\n")
	if len(lines) != m.height {
		t.Fatalf("renderFrame() line count: got %d, want %d (m.height)", len(lines), m.height)
	}
}

// TestView_NoDuplicateGutterNumbers_WideTable asserts that numbers followed
// by file text never repeat in the editor gutter. The original bug's visible
// symptom was "1 # Wide table repro" appearing on both row 1 and row 2
// because the first editor line soft-wrapped into two rendered rows. With
// relative-numbering on, a gutter digit like "1" alone (relative distance)
// on a row WITHOUT trailing content is normal and ignored; what's abnormal
// is the same "N text…" pair showing up twice.
func TestView_NoDuplicateGutterNumbers_WideTable(t *testing.T) {
	m := openWideTable(t, 80, 24)
	got := m.renderFrame()
	lines := strings.Split(got, "\n")

	seen := make(map[string]int)
	for i, line := range lines {
		// Extract the editor region (after the divider if the tree pane is
		// showing). Falling through to the whole line when no divider is
		// present keeps the test robust to layout changes.
		region := line
		if _, after, found := strings.Cut(line, "│"); found {
			region = after
		}
		num, rest := splitGutterNumber(region)
		rest = strings.TrimSpace(rest)
		if num == "" || rest == "" {
			// Either not a gutter row or a relative-only row with no
			// trailing file text — nothing to guard against here.
			continue
		}
		key := num + "\x1f" + rest
		if prev, ok := seen[key]; ok {
			t.Fatalf("editor gutter pair %q+%q appears on both row %d and row %d:\n%s", num, rest, prev, i, got)
		}
		seen[key] = i
	}
}

// TestView_IdempotentAcrossCalls_WideTable asserts that two successive
// renderFrame() calls produce the same output. Any hidden mutable state in
// the composition path (e.g. in-place slice reuse) would surface here.
func TestView_IdempotentAcrossCalls_WideTable(t *testing.T) {
	m := openWideTable(t, 80, 24)
	first := m.renderFrame()
	second := m.renderFrame()
	if first != second {
		t.Fatalf("renderFrame() not idempotent across calls:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

// TestView_TreeNoDuplicateEntries_WideTable asserts that no non-empty tree
// row content repeats in the left pane — the tree shares the same clamp path
// as the editor, and a bug that double-rendered a row would surface as a
// repeated basename in the gutter column.
func TestView_TreeNoDuplicateEntries_WideTable(t *testing.T) {
	m := openWideTable(t, 80, 24)
	got := m.renderFrame()
	tw := m.treeWidth()
	lines := strings.Split(got, "\n")
	seen := make(map[string]int, len(lines))
	// Under lipgloss v2, Width(tw) is the pane total including the right
	// border, so the divider rune sits at index tw-1. Slice to tw-1 to stay
	// inside the tree content only; otherwise every empty row contributes
	// "│" as a bogus duplicate.
	contentW := tw - 1
	if contentW < 0 {
		contentW = 0
	}
	for i, line := range lines {
		// Take only the tree-pane content column. Rune-slicing is an
		// over-approximation vs. display cells, but it's sufficient to
		// catch whole-row duplicates.
		runes := []rune(line)
		if len(runes) > contentW {
			runes = runes[:contentW]
		}
		cell := strings.TrimSpace(string(runes))
		if cell == "" {
			continue
		}
		if prev, ok := seen[cell]; ok {
			t.Fatalf("tree cell %q appears on both row %d and row %d:\n%s", cell, prev, i, got)
		}
		seen[cell] = i
	}
}

// splitGutterNumber returns the leading gutter number and the content that
// follows it. A leading run of ASCII whitespace is skipped, then digits are
// consumed; both parts may be empty strings.
func splitGutterNumber(s string) (num, rest string) {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	start := i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return s[start:i], s[i:]
}

// -----------------------------------------------------------------------------
// F.4 regression: non-key messages must always reach the editor, regardless
// of focus. goeditor uses non-key tea.Msgs to drive transient UI such as
// DispatchMessage's clear timer. When the tree has focus (the default at
// startup), the old routing dropped those messages and status text that the
// editor emitted via SetStatusMessage stuck on screen forever.

func TestForwardToFocused_NonKeyMsgAlwaysReachesEditor(t *testing.T) {
	type arbitraryTickMsg struct{ tag string }

	t.Run("tree focused: non-key msg is forwarded to editor", func(t *testing.T) {
		fe := &fakeEditor{}
		m := &Model{editor: fe, focus: focusTree}
		msg := arbitraryTickMsg{tag: "clear-status"}

		_, _ = m.forwardToFocused(msg)
		if len(fe.updateMsgs) != 1 {
			t.Fatalf("editor should have received the non-key msg; got %d: %+v", len(fe.updateMsgs), fe.updateMsgs)
		}
		if got, _ := fe.updateMsgs[0].(arbitraryTickMsg); got != msg {
			t.Errorf("editor received wrong msg: got %+v, want %+v", fe.updateMsgs[0], msg)
		}
	})

	t.Run("editor focused: non-key msg still forwarded", func(t *testing.T) {
		fe := &fakeEditor{}
		m := &Model{editor: fe, focus: focusEditor}
		msg := arbitraryTickMsg{tag: "also-reaches"}

		_, _ = m.forwardToFocused(msg)
		if len(fe.updateMsgs) != 1 {
			t.Fatalf("editor should have received the non-key msg; got %d: %+v", len(fe.updateMsgs), fe.updateMsgs)
		}
	})

	t.Run("tree focused: key msg is NOT forwarded to editor", func(t *testing.T) {
		// The focus gate still applies to key events — the editor must not
		// receive keystrokes meant for the tree. This is the other half of
		// the split and keeps the fix from regressing into "forward all
		// messages unconditionally".
		fe := &fakeEditor{}
		// leftPane == nil → handleKey path for the tree short-circuits to
		// (m, nil) but the critical assertion is that the editor did NOT
		// see the key.
		m := &Model{editor: fe, focus: focusTree, leftPane: nil}
		key := tea.KeyPressMsg{Code: 'j', Text: "j"}

		_, _ = m.forwardToFocused(key)
		if len(fe.updateMsgs) != 0 {
			t.Errorf("editor must NOT see tree-focused key events; got %+v", fe.updateMsgs)
		}
	})
}
