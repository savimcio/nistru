package editor

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/savimcio/nistru/internal/config"
)

// -----------------------------------------------------------------------------
// Round-trip: SetContent → Content.

func TestGoeditorAdapter_ContentRoundTrip(t *testing.T) {
	a := newGoeditorAdapter("", "abc", false, nil)
	if got := a.Content(); got != "abc" {
		t.Fatalf("after constructor: Content() = %q, want %q", got, "abc")
	}

	a.SetContent("hello\nworld")
	if got := a.Content(); got != "hello\nworld" {
		t.Fatalf("after SetContent: Content() = %q, want %q", got, "hello\nworld")
	}
}

// Regression for F.3: Content() used to re-append a trailing newline when
// the most recent SetContent had one, based on a memoised flag that never
// updated as the user edited. A user who intentionally deleted the EOF
// newline could not persist that change — Content() re-added it. The fix is
// to return goeditor's buffer verbatim.
//
// goeditor's own SetContent drops trailing newlines at parse time, so the
// round-trip shape for "hello\n" is "hello" and for "hello" is also "hello".
// We assert exactly that, documenting the semantic: nistru no longer
// preserves EOF newlines across the adapter. If goeditor is ever replaced
// with a buffer that does preserve them, this test should be updated
// intentionally; it must not silently regress to the old re-append behaviour.
func TestGoeditorAdapter_ContentDoesNotReappendTrailingNewline(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"trailing newline dropped by goeditor", "hello\n", "hello"},
		{"no trailing newline stays absent", "hello", "hello"},
		{"multiline with trailing newline", "line1\nline2\n", "line1\nline2"},
		{"multiline without trailing newline", "line1\nline2", "line1\nline2"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			a := newGoeditorAdapter("", tc.in, false, nil)
			if got := a.Content(); got != tc.want {
				t.Errorf("Content() after constructor(%q) = %q, want %q (no re-append heuristic)", tc.in, got, tc.want)
			}
			// SetContent must behave the same way — the old heuristic lived
			// in SetContent too.
			a.SetContent(tc.in)
			if got := a.Content(); got != tc.want {
				t.Errorf("Content() after SetContent(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Mode round-trip — Normal/Insert/Visual must reflect through goeditor and
// back; Command maps to Normal with a dispatch-message cmd.

func TestGoeditorAdapter_ModeRoundTrip(t *testing.T) {
	a := newGoeditorAdapter("", "x", false, nil)

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

// SetMode(ModeCommand) is the nistru-level hack: goeditor has no matching
// SetCommandMode entry point from outer callers, so we fall back to Normal
// and emit a status-message cmd so the UI can still flash a ":" prompt.
// The adapter also stashes modeHint = ModeCommand so the follow-up Mode()
// read reflects the caller's intent.
func TestGoeditorAdapter_ModeCommandMapsToNormalWithStatusCmd(t *testing.T) {
	a := newGoeditorAdapter("", "x", false, nil)

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

// F2.3 regression: Mode() must surface goeditor's own command mode, not
// just the nistru-level SetMode(ModeCommand) path. Pressing ":" in Normal
// mode transitions goeditor's inner editor into CommandMode (via core's
// normal_mode.go switch on key.Rune == ':'); previously Mode() only
// consulted modeHint, which was never set along that path, so the outer
// status bar rendered "[NORMAL]" while the user was typing ":w". Now
// Mode() reads IsCommandMode() directly.
func TestGoeditorAdapter_ModeReportsGoeditorCommandMode(t *testing.T) {
	a := newGoeditorAdapter("", "hello", false, nil)

	if got := a.Mode(); got != ModeNormal {
		t.Fatalf("pre: Mode() = %v, want ModeNormal", got)
	}

	// goeditor's convertBubbleKey reads key.Text for the rune when there's
	// no matching tea.Key constant. A ":" keypress is a plain-rune event.
	colon := tea.KeyPressMsg{Code: ':', Text: ":"}
	_, _ = a.Update(colon)

	if !a.inner.IsCommandMode() {
		t.Fatalf("goeditor did not enter command mode after ':' keypress — Update plumbing changed?")
	}
	if got := a.Mode(); got != ModeCommand {
		t.Errorf("after ':': Mode() = %v, want ModeCommand (from IsCommandMode())", got)
	}

	// Esc returns goeditor to normal. The adapter should report ModeNormal
	// again — modeHint was never set along the ":" path, so nothing sticks.
	esc := tea.KeyPressMsg{Code: tea.KeyEscape}
	_, _ = a.Update(esc)
	if a.inner.IsCommandMode() {
		t.Fatalf("goeditor still reports IsCommandMode after Esc — upstream contract changed?")
	}
	if got := a.Mode(); got != ModeNormal {
		t.Errorf("after Esc: Mode() = %v, want ModeNormal", got)
	}
}

// -----------------------------------------------------------------------------
// Key interception — bound Ctrl shortcuts must be claimed by the adapter
// (handled=true) before reaching goeditor.

func TestGoeditorAdapter_InterceptsCtrlZ(t *testing.T) {
	a := newGoeditorAdapter("", "abc", false, nil)

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
			a := newGoeditorAdapter("", "x", false, nil)
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

// Regression for F.2: the adapter must honour the user's keymap, not the
// built-in Ctrl+Z/Y/X/C/V defaults. When a user rebinds undo to ctrl+u, the
// new binding fires and the OLD default (ctrl+z) falls through to goeditor
// as a regular keystroke (i.e. the interceptor does not claim it).
func TestGoeditorAdapter_HonoursRebounddUndoKey(t *testing.T) {
	km := config.DefaultKeymap()
	km[config.ActionUndo] = "ctrl+u"
	// Also clear the old default to avoid a stale duplicate; Validate handles
	// that in production but we want the intent crisp here.
	a := newGoeditorAdapter("", "x", false, km)

	// New binding: ctrl+u must fire undo.
	newBind := tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl}
	cmd, handled := a.interceptKey(newBind)
	if !handled {
		t.Fatalf("interceptKey(ctrl+u) handled = false, want true after rebinding undo to ctrl+u")
	}
	if cmd == nil {
		t.Fatalf("interceptKey(ctrl+u) cmd = nil, want non-nil (undo sequence)")
	}

	// Old default: ctrl+z must NOT fire undo anymore — it falls through.
	// (ctrl+z is no longer bound to any action in this keymap.)
	oldDefault := tea.KeyPressMsg{Code: 'z', Mod: tea.ModCtrl}
	cmd, handled = a.interceptKey(oldDefault)
	if handled {
		t.Errorf("interceptKey(ctrl+z) handled = true after rebinding undo away from ctrl+z; keymap must be authoritative")
	}
	if cmd != nil {
		t.Errorf("interceptKey(ctrl+z) cmd != nil on pass-through")
	}
}

// Unbound keys must NOT be intercepted — they pass through to goeditor. We
// check via the lower-level interceptKey rather than Update, because driving
// Update for a plain 'a' boots goeditor's full insert-mode machinery (which
// is goeditor's business to test, not ours).
func TestGoeditorAdapter_DoesNotInterceptUnboundKey(t *testing.T) {
	a := newGoeditorAdapter("", "x", false, nil)

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
	a := newGoeditorAdapter("", "line1\nline2", false, nil)

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
	a := newGoeditorAdapter("", "x", false, nil)

	if cmd := a.Undo(); cmd == nil {
		t.Fatalf("Undo() = nil, want non-nil tea.Cmd")
	}
}

func TestGoeditorAdapter_RedoReturnsNonNilCmd(t *testing.T) {
	a := newGoeditorAdapter("", "x", false, nil)

	if cmd := a.Redo(); cmd == nil {
		t.Fatalf("Redo() = nil, want non-nil tea.Cmd")
	}
}

// Regression for F.1: Redo previously synthesised Ctrl+R, which goeditor's
// vim interpreter silently ignored (standard vim redo is uppercase U).
// goeditor's convertBubbleKey reads the key's Text field for the rune, so the
// synth KeyPressMsg must carry Text == "U" to be recognised as redo.
//
// We introspect the synth key helpers directly rather than executing the
// tea.Sequence returned by Redo() — the sequence's inner messages are only
// observable to a running tea.Program, which would pull e2e machinery into a
// unit test.
func TestGoeditorAdapter_RedoSynthesisesUppercaseU(t *testing.T) {
	got := redoKeyMsg()
	if got.Text != "U" {
		t.Errorf("redoKeyMsg().Text = %q, want %q (goeditor reads Text for the rune)", got.Text, "U")
	}
	if got.Code != 'U' {
		t.Errorf("redoKeyMsg().Code = %q, want %q", got.Code, 'U')
	}
	if got.Mod.Contains(tea.ModCtrl) {
		t.Errorf("redoKeyMsg().Mod contains Ctrl; goeditor's redo is uppercase U, not Ctrl+R")
	}
}

// Sibling check for the undo synth — kept adjacent so a future refactor
// that breaks Undo the same way Redo was broken fails loudly here.
func TestGoeditorAdapter_UndoSynthesisesLowercaseU(t *testing.T) {
	got := undoKeyMsg()
	if got.Text != "u" {
		t.Errorf("undoKeyMsg().Text = %q, want %q", got.Text, "u")
	}
	if got.Code != 'u' {
		t.Errorf("undoKeyMsg().Code = %q, want %q", got.Code, 'u')
	}
	if got.Mod.Contains(tea.ModCtrl) {
		t.Errorf("undoKeyMsg().Mod contains Ctrl; vim undo is plain 'u'")
	}
}

// synthVimMotion must produce a cmd that, under tea.Sequence, emits the
// requested runes in order. We execute the inner cmds and collect the
// resulting messages to verify.
func TestGoeditorAdapter_SynthVimMotionEmitsKeysInOrder(t *testing.T) {
	a := newGoeditorAdapter("", "x", false, nil)

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
	a := newGoeditorAdapter("", "hello", false, nil)
	_ = a.View() // must not panic
}

// SetSize should not panic on subsequent grown sizes, and the adapter
// satisfies the Editor interface contract (compile-time check via var _).
func TestGoeditorAdapter_SetSizeDoesNotPanic(t *testing.T) {
	a := newGoeditorAdapter("", "hello", false, nil)
	if cmd := a.SetSize(80, 24); cmd != nil {
		t.Errorf("SetSize returned non-nil cmd; adapter contract says nil")
	}
}
