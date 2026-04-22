package editor

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/ionut-t/goeditor"
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
	// trailingNL remembers whether the most recent SetContent ended with '\n'.
	// goeditor's own buffer drops trailing newlines (SetContent only appends
	// the final rune slice if it is non-empty, and GetCurrentContent joins
	// with "\n" between lines). We restore the trailing newline in Content()
	// so disk writes and plugin round-trips preserve the user's file
	// convention. Updated on SetContent; not touched while the user types —
	// once the buffer is under the user's fingers, trailing-newline fidelity
	// is an editor-internal concern.
	trailingNL bool
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
func newGoeditorAdapter(path, content string, relativeNumbers bool) *goeditorAdapter {
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
	return &goeditorAdapter{inner: e, trailingNL: strings.HasSuffix(content, "\n")}
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
	a.trailingNL = strings.HasSuffix(text, "\n")
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

// Content returns the current buffer text. goeditor's GetCurrentContent joins
// lines with "\n" between but drops the trailing newline; we restore it when
// the most recent SetContent included one so flushNow writes and plugin
// round-trips preserve the original file convention.
func (a *goeditorAdapter) Content() string {
	s := a.inner.GetCurrentContent()
	if a.trailingNL && !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return s
}

// Mode maps goeditor's boolean accessors into our enum. We check Insert and
// Visual explicitly; anything else is reported as Normal (this includes
// goeditor's own command/search modes, which nistru does not surface).
// ModeCommand is stashed in modeHint and only reported when the caller
// explicitly asked for it via SetMode — goeditor itself has no concept.
func (a *goeditorAdapter) Mode() Mode {
	if a.inner.IsInsertMode() {
		return ModeInsert
	}
	if a.inner.IsVisualMode() || a.inner.IsVisualLineMode() {
		return ModeVisual
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

// interceptKey inspects a KeyPressMsg for nistru's bound Ctrl shortcuts and
// returns a tea.Cmd + true when the key should NOT reach goeditor. Returns
// (nil, false) for anything we want goeditor to handle natively.
//
// Keys matched here are matched by their String() form so user-configurable
// bindings (via km.Lookup) can be wired in by M.B — for now we match the
// built-in defaults (ctrl+z / ctrl+y / ctrl+x / ctrl+c / ctrl+v).
//
// Cut/Copy/Paste synth vim motions (dd / yy / p). The sequence first
// transitions to Normal mode to avoid "d" / "y" / "p" being treated as
// literal insertions when the user was in Insert mode.
func (a *goeditorAdapter) interceptKey(km tea.KeyPressMsg) (tea.Cmd, bool) {
	switch km.String() {
	case "ctrl+z":
		return a.Undo(), true
	case "ctrl+y":
		return a.Redo(), true
	case "ctrl+x":
		return a.synthVimMotion("d", "d"), true
	case "ctrl+c":
		return a.synthVimMotion("y", "y"), true
	case "ctrl+v":
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
