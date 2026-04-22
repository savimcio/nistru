package editor

import (
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/ionut-t/goeditor"
	"github.com/savimcio/nistru/internal/config"
)

// goeditorAdapter wraps a goeditor.Model and exposes it behind the narrow
// Editor interface. It also hosts the nistru-level key interception layer:
// micro-style Ctrl+Z/Y/X/C/V shortcuts are recognised here and translated
// into Editor-level effects (Undo/Redo cmds, synthesised vim motions) before
// the keystroke can reach goeditor's built-in key handling.
//
// goeditor.Model is stored by value because goeditor's Init/Update/View/
// GetCursorPosition use value receivers. All state-mutating methods on
// goeditor.Model take a *Model receiver, so we call those via &a.inner.
//
// Construction is cheap — we start with a zero-sized editor and let the
// outer model call SetSize on the first tea.WindowSizeMsg.
type goeditorAdapter struct {
	inner    goeditor.Model
	modeHint Mode // stashed ModeCommand since goeditor's normal mode doesn't track it
	// keymap is the user's resolved action→key binding. interceptKey uses it
	// to recognise user-configured Undo/Redo/Cut/Copy/Paste keys, not the
	// hard-coded ctrl+z/y/x/c/v strings. May be nil in hand-constructed test
	// adapters — the interceptor falls back to DefaultKeymap via km.Lookup in
	// that case (Lookup itself handles the nil receiver).
	keymap config.Keymap
}

// statusDispatchDuration is the default TTL for DispatchMessage. goeditor's
// own API takes a duration; 3s matches vimtea's prior feel for a transient
// status-line flash.
const statusDispatchDuration = 3 * time.Second

var _ Editor = (*goeditorAdapter)(nil)

// newGoeditorAdapter constructs a fresh adapter seeded with the given
// content. Size is 0x0 initially — the outer model drives SetSize on the
// first WindowSizeMsg. path is currently unused (goeditor has no filename
// concept exposed through its public API); we keep it in the signature to
// parallel the old newEditor shape so M.B can slot this in without churn.
//
// keymap supplies the user's Undo/Redo/Cut/Copy/Paste bindings. A nil keymap
// means "use the built-in defaults" — interceptKey calls config.Keymap.Lookup,
// which handles the nil receiver by returning DefaultKeymap entries.
func newGoeditorAdapter(path, content string, relativeNumbers bool, keymap config.Keymap) *goeditorAdapter {
	_ = path // reserved — goeditor has no SetFilename equivalent; kept for call-site parity
	e := goeditor.New(0, 0)
	e.ShowRelativeLineNumbers(relativeNumbers)
	e.SetContent(content)
	// goeditor.Model starts blurred; its Update drops all KeyMsg events when
	// !IsFocused(). nistru treats the editor pane as the default-focused
	// surface once a file is open, so we Focus() immediately and let the
	// outer Model swap focus to the tree via ad-hoc Blur/Focus calls if
	// needed (currently the tree's KeyEvent bypass keeps goeditor focused).
	e.Focus()
	return &goeditorAdapter{
		inner:  e,
		keymap: keymap,
	}
}

func (a *goeditorAdapter) Init() tea.Cmd { return a.inner.Init() }

// Update routes the message through the interception layer first. Anything
// the interceptor claims never reaches goeditor.
func (a *goeditorAdapter) Update(msg tea.Msg) (Editor, tea.Cmd) {
	if km, ok := msg.(tea.KeyPressMsg); ok {
		if cmd, handled := a.interceptKey(km); handled {
			return a, cmd
		}
	}
	next, cmd := a.inner.Update(msg)
	a.inner = next
	return a, cmd
}

func (a *goeditorAdapter) View() string { return a.inner.View() }

func (a *goeditorAdapter) SetSize(w, h int) tea.Cmd {
	a.inner.SetSize(w, h)
	// goeditor's SetSize recalculates visual metrics but does NOT push the
	// result into its viewport (renderVisibleSlice only runs inside Update).
	// Without this kick the editor pane stays blank after a WindowSizeMsg
	// resized it but before any key is pressed. Same safety pattern as
	// SetContent — see the renderKickMsg comment there.
	a.inner, _ = a.inner.Update(renderKickMsg{})
	return nil
}

func (a *goeditorAdapter) SetContent(text string) {
	a.inner.SetContent(text)
	// goeditor populates its viewport content only from inside Update
	// (renderVisibleSlice runs after the Update switch). Callers that reach
	// the editor via SetContent → View (test goldens, the initial openFile
	// path under -short-skipping tests) would otherwise see an empty pane.
	// Feed a cheap no-op message to force one render pass; discard the cmd
	// because it contains the blocking listenForEditorUpdate and is not
	// safe to execute synchronously here.
	a.inner, _ = a.inner.Update(renderKickMsg{})
}

// renderKickMsg is a private tea.Msg used by SetContent to trigger one
// renderVisibleSlice pass inside goeditor.Update without matching any of its
// switch cases. The default Update path still runs (calculateVisualMetrics
// was already called by SetContent; we only need the final render step).
type renderKickMsg struct{}

// Content returns the current buffer text verbatim from goeditor.
// goeditor's GetCurrentContent joins lines with "\n" between but does NOT
// append a trailing newline — that is the convention we surface to the rest
// of nistru. Previously we memoised whether the initial SetContent ended
// with '\n' and re-added it here, but that heuristic never updated as the
// user edited: a user who intentionally deleted the EOF newline could not
// persist that change because Content() would silently put it back, and
// flushNow would write the old newline to disk.
//
// Dropping the heuristic means: files loaded with a trailing newline round-
// trip through goeditor and come out without one (goeditor's own buffer
// does not preserve it). Users who want a trailing newline can add one with
// a keystroke; files without a trailing newline stay that way.
func (a *goeditorAdapter) Content() string {
	return a.inner.GetCurrentContent()
}

// Mode maps goeditor's boolean accessors into our enum. We check Insert and
// Visual explicitly; anything else is reported as Normal.
//
// ModeCommand has two entry paths we need to report. The first is the
// nistru-level SetMode(ModeCommand) path: outer callers (palette, synthetic
// command dispatch) set modeHint = ModeCommand, and Mode() reports it while
// goeditor itself sits in Normal. The second is goeditor's own command
// mode — pressing ":" from Normal inside goeditor transitions its inner
// editor state into CommandMode, reported via IsCommandMode(). Previously
// we only surfaced the first path; the second is what the user actually
// hits when they type ":w" to save, and the status bar read "[NORMAL]"
// while they were typing the command. F2.3 fixes that by reading
// IsCommandMode() directly.
func (a *goeditorAdapter) Mode() Mode {
	if a.inner.IsInsertMode() {
		return ModeInsert
	}
	if a.inner.IsVisualMode() || a.inner.IsVisualLineMode() {
		return ModeVisual
	}
	if a.inner.IsCommandMode() {
		return ModeCommand
	}
	if a.modeHint == ModeCommand && a.inner.IsNormalMode() {
		return ModeCommand
	}
	return ModeNormal
}

// SetMode drives goeditor to the requested mode. ModeCommand has no direct
// goeditor equivalent — we fall back to Normal and emit a status message so
// the outer UI can still surface the intent. The hint is remembered so the
// subsequent Mode() read reflects the caller's request.
func (a *goeditorAdapter) SetMode(m Mode) tea.Cmd {
	switch m {
	case ModeInsert:
		a.modeHint = ModeInsert
		a.inner.SetInsertMode()
		return nil
	case ModeVisual:
		a.modeHint = ModeVisual
		a.inner.SetVisualMode()
		return nil
	case ModeCommand:
		a.modeHint = ModeCommand
		a.inner.SetNormalMode()
		return a.inner.DispatchMessage(":", statusDispatchDuration)
	default: // ModeNormal
		a.modeHint = ModeNormal
		a.inner.SetNormalMode()
		return nil
	}
}

func (a *goeditorAdapter) CursorRowCol() (row, col int) {
	p := a.inner.GetCursorPosition()
	return p.Row, p.Col
}

// undoKeyMsg is the synth KeyPressMsg that represents a vim 'u' keypress as
// goeditor's convertBubbleKey expects it (Text carries the rune). Extracted
// for test introspection.
func undoKeyMsg() tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: 'u', Text: "u"}
}

// redoKeyMsg is the synth KeyPressMsg that represents a vim 'U' (uppercase)
// keypress. goeditor's vim interpreter maps redo to uppercase U — not Ctrl+R
// — and its convertBubbleKey reads key.Text for the rune, so Text must be "U".
// ShiftedCode/BaseCode mirror what a real keyboard press of Shift+u produces
// and keeps the event shape consistent with upstream expectations.
func redoKeyMsg() tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: 'U', Text: "U", ShiftedCode: 'U', BaseCode: 'u'}
}

// Undo synthesises the vim 'u' keypress in Normal mode. goeditor has no
// public Undo() method, but its built-in vim interpreter handles 'u' as
// undo. We ensure Normal mode first so 'u' is not treated as literal text
// in Insert mode.
func (a *goeditorAdapter) Undo() tea.Cmd {
	return tea.Sequence(
		a.cmdEnterNormal(),
		func() tea.Msg { return undoKeyMsg() },
	)
}

// Redo synthesises the vim 'U' (uppercase) keypress in Normal mode.
func (a *goeditorAdapter) Redo() tea.Cmd {
	return tea.Sequence(
		a.cmdEnterNormal(),
		func() tea.Msg { return redoKeyMsg() },
	)
}

func (a *goeditorAdapter) SetStatusMessage(msg string) tea.Cmd {
	return a.inner.DispatchMessage(msg, statusDispatchDuration)
}

// cmdEnterNormal returns a tea.Cmd that switches the editor to Normal mode
// on execution (not at cmd-construction time). Exposed as a helper so synth
// sequences can guarantee ordering via tea.Sequence.
func (a *goeditorAdapter) cmdEnterNormal() tea.Cmd {
	return func() tea.Msg {
		a.inner.SetNormalMode()
		a.modeHint = ModeNormal
		return nil
	}
}

// interceptKey inspects a KeyPressMsg for nistru's bound editor shortcuts
// and returns a tea.Cmd + true when the key should NOT reach goeditor.
// Returns (nil, false) for anything we want goeditor to handle natively.
//
// Keys are matched by their String() form against the user's keymap
// (a.keymap). Users who rebind e.g. undo to ctrl+u get that binding honored
// here; the old ctrl+z/y/x/c/v defaults are NO LONGER hard-coded — they take
// effect only when the resolved keymap happens to point at them.
//
// Cut/Copy/Paste synth vim motions (dd / yy / p). The sequence first
// transitions to Normal mode to avoid "d" / "y" / "p" being treated as
// literal insertions when the user was in Insert mode.
func (a *goeditorAdapter) interceptKey(km tea.KeyPressMsg) (tea.Cmd, bool) {
	key := km.String()
	// config.Keymap.Lookup handles a nil receiver by falling back to the
	// built-in DefaultKeymap, so test adapters constructed without a keymap
	// still get sensible defaults.
	switch key {
	case a.keymap.Lookup(config.ActionUndo):
		return a.Undo(), true
	case a.keymap.Lookup(config.ActionRedo):
		return a.Redo(), true
	case a.keymap.Lookup(config.ActionCutLine):
		return a.synthVimMotion("d", "d"), true
	case a.keymap.Lookup(config.ActionCopyLine):
		return a.synthVimMotion("y", "y"), true
	case a.keymap.Lookup(config.ActionPaste):
		return a.synthVimMotion("p"), true
	}
	return nil, false
}

// synthVimMotion emits a Normal-mode transition followed by the given
// single-rune keystrokes as KeyPressMsg values, in order, via tea.Sequence.
// tea.Sequence guarantees strict ordering so multi-rune motions like "dd"
// and "yy" cannot be corrupted by concurrent user input.
func (a *goeditorAdapter) synthVimMotion(keys ...string) tea.Cmd {
	cmds := make([]tea.Cmd, 0, 1+len(keys))
	cmds = append(cmds, a.cmdEnterNormal())
	for _, k := range keys {
		k := k
		cmds = append(cmds, func() tea.Msg {
			runes := []rune(k)
			if len(runes) == 0 {
				return nil
			}
			return tea.KeyPressMsg{Code: runes[0], Text: k}
		})
	}
	return tea.Sequence(cmds...)
}
