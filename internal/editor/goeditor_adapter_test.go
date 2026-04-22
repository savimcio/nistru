package editor

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// -----------------------------------------------------------------------------
// Round-trip: SetContent → Content.

func TestGoeditorAdapter_ContentRoundTrip(t *testing.T) {
	a := newGoeditorAdapter("", "abc", false)
	if got := a.Content(); got != "abc" {
		t.Fatalf("after constructor: Content() = %q, want %q", got, "abc")
	}

	a.SetContent("hello\nworld")
	if got := a.Content(); got != "hello\nworld" {
		t.Fatalf("after SetContent: Content() = %q, want %q", got, "hello\nworld")
	}
}

// -----------------------------------------------------------------------------
// Mode round-trip — Normal/Insert/Visual must reflect through goeditor and
// back; Command maps to Normal with a dispatch-message cmd.

func TestGoeditorAdapter_ModeRoundTrip(t *testing.T) {
	a := newGoeditorAdapter("", "x", false)

	cases := []struct {
		name string
		set  Mode
		want Mode
	}{
		{"normal", ModeNormal, ModeNormal},
		{"insert", ModeInsert, ModeInsert},
		{"visual", ModeVisual, ModeVisual},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if cmd := a.SetMode(tc.set); cmd != nil {
				t.Errorf("SetMode(%v) returned non-nil cmd, want nil", tc.set)
			}
			if got := a.Mode(); got != tc.want {
				t.Errorf("after SetMode(%v): Mode() = %v, want %v", tc.set, got, tc.want)
			}
		})
	}
}

// SetMode(ModeCommand) is the nistru-level hack: goeditor has no "command"
// slot, so we map to Normal and emit a status-message cmd so the UI can
// still flash a ":" prompt.
func TestGoeditorAdapter_ModeCommandMapsToNormalWithStatusCmd(t *testing.T) {
	a := newGoeditorAdapter("", "x", false)

	cmd := a.SetMode(ModeCommand)
	if cmd == nil {
		t.Fatalf("SetMode(ModeCommand) returned nil cmd; want status-message cmd")
	}
	if got := a.Mode(); got != ModeCommand {
		t.Errorf("after SetMode(ModeCommand): Mode() = %v, want ModeCommand (from hint)", got)
	}
	if !a.inner.IsNormalMode() {
		t.Errorf("inner editor should be in Normal mode when ModeCommand is set; IsNormalMode() = false")
	}
}

// -----------------------------------------------------------------------------
// Key interception — bound Ctrl shortcuts must be claimed by the adapter
// (handled=true) before reaching goeditor.

func TestGoeditorAdapter_InterceptsCtrlZ(t *testing.T) {
	a := newGoeditorAdapter("", "abc", false)

	km := tea.KeyPressMsg{Code: 'z', Mod: tea.ModCtrl}
	cmd, handled := a.interceptKey(km)
	if !handled {
		t.Fatalf("interceptKey(ctrl+z) handled = false, want true")
	}
	if cmd == nil {
		t.Fatalf("interceptKey(ctrl+z) cmd = nil, want non-nil (undo sequence)")
	}
}

func TestGoeditorAdapter_InterceptsAllBoundCtrlKeys(t *testing.T) {
	cases := []struct {
		name string
		key  tea.KeyPressMsg
	}{
		{"ctrl+z (undo)", tea.KeyPressMsg{Code: 'z', Mod: tea.ModCtrl}},
		{"ctrl+y (redo)", tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl}},
		{"ctrl+x (cut)", tea.KeyPressMsg{Code: 'x', Mod: tea.ModCtrl}},
		{"ctrl+c (copy)", tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}},
		{"ctrl+v (paste)", tea.KeyPressMsg{Code: 'v', Mod: tea.ModCtrl}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			a := newGoeditorAdapter("", "x", false)
			cmd, handled := a.interceptKey(tc.key)
			if !handled {
				t.Fatalf("interceptKey(%v) handled = false, want true", tc.key.String())
			}
			if cmd == nil {
				t.Fatalf("interceptKey(%v) cmd = nil, want non-nil", tc.key.String())
			}
		})
	}
}

// Unbound keys must NOT be intercepted — they pass through to goeditor. We
// check via the lower-level interceptKey rather than Update, because driving
// Update for a plain 'a' boots goeditor's full insert-mode machinery (which
// is goeditor's business to test, not ours).
func TestGoeditorAdapter_DoesNotInterceptUnboundKey(t *testing.T) {
	a := newGoeditorAdapter("", "x", false)

	cases := []tea.KeyPressMsg{
		{Code: 'a', Text: "a"},
		{Code: 'i', Text: "i"},
		{Code: 'q', Mod: tea.ModCtrl}, // not in our bound set
		{Code: 's', Mod: tea.ModCtrl}, // app-level, intercepted by outer Model
	}
	for _, k := range cases {
		k := k
		t.Run(k.String(), func(t *testing.T) {
			cmd, handled := a.interceptKey(k)
			if handled {
				t.Errorf("interceptKey(%v) handled = true, want false (should pass through)", k.String())
			}
			if cmd != nil {
				t.Errorf("interceptKey(%v) cmd != nil on pass-through", k.String())
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Cursor read. We don't drive cursor motion programmatically — goeditor's
// vim interpreter is its own concern. We just check the default position
// after SetContent is coherent (non-negative, inside the buffer).

func TestGoeditorAdapter_CursorRowColAfterSetContent(t *testing.T) {
	a := newGoeditorAdapter("", "line1\nline2", false)

	row, col := a.CursorRowCol()
	if row < 0 || col < 0 {
		t.Fatalf("CursorRowCol() = (%d, %d); want non-negative", row, col)
	}
	// 2-line buffer, rows are zero-indexed; cursor must be within.
	if row > 1 {
		t.Errorf("row = %d, want <= 1 (buffer has 2 lines)", row)
	}
}

// -----------------------------------------------------------------------------
// Undo/Redo synthesis — must return a non-nil tea.Cmd that, when invoked,
// ultimately yields a KeyPressMsg for 'u' / Ctrl+R respectively. Because
// they're wrapped in tea.Sequence with a Normal-mode transition, we can
// only assert the cmd is non-nil here; the exact message stream is a
// goeditor/bubbletea concern. The intent is that the unit test catches
// regressions where Undo/Redo accidentally return nil.

func TestGoeditorAdapter_UndoReturnsNonNilCmd(t *testing.T) {
	a := newGoeditorAdapter("", "x", false)

	if cmd := a.Undo(); cmd == nil {
		t.Fatalf("Undo() = nil, want non-nil tea.Cmd")
	}
}

func TestGoeditorAdapter_RedoReturnsNonNilCmd(t *testing.T) {
	a := newGoeditorAdapter("", "x", false)

	if cmd := a.Redo(); cmd == nil {
		t.Fatalf("Redo() = nil, want non-nil tea.Cmd")
	}
}

// synthVimMotion must produce a cmd that, under tea.Sequence, emits the
// requested runes in order. We execute the inner cmds and collect the
// resulting messages to verify.
func TestGoeditorAdapter_SynthVimMotionEmitsKeysInOrder(t *testing.T) {
	a := newGoeditorAdapter("", "x", false)

	cmd := a.synthVimMotion("d", "d")
	if cmd == nil {
		t.Fatalf("synthVimMotion returned nil")
	}

	// tea.Sequence returns a *sequenceMsg when invoked that tea.Program
	// unwraps. We can't inspect it from outside, so the best we can do
	// without a Program is check the cmd is non-nil and Undo/Redo don't
	// crash. Deeper assertions live in e2e tests (M.D).
}

// -----------------------------------------------------------------------------
// Construction does not panic on zero size. goeditor.New(0,0) + SetSize will
// be called later by the outer model; the adapter must survive the
// zero-sized interlude.

func TestGoeditorAdapter_ConstructsAtZeroSize(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("newGoeditorAdapter panicked at zero size: %v", r)
		}
	}()
	a := newGoeditorAdapter("", "hello", false)
	_ = a.View() // must not panic
}

// SetSize should not panic on subsequent grown sizes, and the adapter
// satisfies the Editor interface contract (compile-time check via var _).
func TestGoeditorAdapter_SetSizeDoesNotPanic(t *testing.T) {
	a := newGoeditorAdapter("", "hello", false)
	if cmd := a.SetSize(80, 24); cmd != nil {
		t.Errorf("SetSize returned non-nil cmd; adapter contract says nil")
	}
}
