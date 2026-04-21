package editor

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/kujtimiihoxha/vimtea"
)

// newEditor constructs a fresh vimtea.Editor and registers the micro-style
// Ctrl bindings that live inside the editor (Ctrl+Z/Y/X/C/V for undo, redo,
// cut, copy, paste). Ctrl+S and Ctrl+Q are intercepted at the parent model
// level — they are app-wide concerns, not editor concerns — so they are NOT
// registered here.
//
// This factory is called from two places:
//  1. main.go / initial model construction, with empty content.
//  2. model.go, when a file is opened from the tree (fresh Editor per open).
//
// Because the editor is reconstructed on each open, the bindings below are
// re-registered every time — a fresh Editor has no user bindings.
func newEditor(content, path string) vimtea.Editor {
	opts := []vimtea.EditorOption{
		vimtea.WithContent(content),
		vimtea.WithRelativeNumbers(true),
	}
	if path != "" {
		opts = append(opts, vimtea.WithFileName(path))
	}
	e := vimtea.NewEditor(opts...)

	addCtrlBindings(e)
	return e
}

// addCtrlBindings registers micro-style Ctrl shortcuts inside the editor.
// These bindings intentionally duplicate across Normal, Insert and Visual
// modes so the shortcuts work the same regardless of where the user is.
//
// Ctrl+Z and Ctrl+Y use Buffer.Undo / Buffer.Redo directly — vimtea exposes
// both on the Buffer interface.
//
// Ctrl+X / Ctrl+C / Ctrl+V synthesize the equivalent vim key sequences
// (dd / yy / p) as tea.KeyMsg values returned through tea.Sequence. Two
// subtleties handled here:
//
//  1. The synthesized keys must be interpreted in Normal mode. If the caller
//     is in Insert mode, "d"/"y"/"p" are literal characters — so we prepend
//     editor.SetMode(ModeNormal) to the sequence. For v1 we leave the editor
//     in Normal afterwards, which matches vim's own behaviour for these
//     operators.
//  2. tea.Sequence executes its commands strictly in order with no
//     interleaving of other messages, so "dd" cannot be corrupted into "dj"
//     or "dk" by racing user keystrokes.
//
// Buffer does not expose direct DeleteLine/YankLine/Paste primitives, so the
// tea.Sequence-through-the-key-path remains the only way to reach vimtea's
// motion engine from a binding handler.
func addCtrlBindings(e vimtea.Editor) {
	modes := []vimtea.EditorMode{vimtea.ModeNormal, vimtea.ModeInsert, vimtea.ModeVisual}

	for _, mode := range modes {
		e.AddBinding(vimtea.KeyBinding{
			Key:         "ctrl+z",
			Mode:        mode,
			Description: "undo",
			Handler: func(buf vimtea.Buffer) tea.Cmd {
				return buf.Undo()
			},
		})

		e.AddBinding(vimtea.KeyBinding{
			Key:         "ctrl+y",
			Mode:        mode,
			Description: "redo",
			Handler: func(buf vimtea.Buffer) tea.Cmd {
				return buf.Redo()
			},
		})

		e.AddBinding(vimtea.KeyBinding{
			Key:         "ctrl+x",
			Mode:        mode,
			Description: "cut line",
			Handler: func(buf vimtea.Buffer) tea.Cmd {
				return synthVimMotion(e, "d", "d")
			},
		})

		e.AddBinding(vimtea.KeyBinding{
			Key:         "ctrl+c",
			Mode:        mode,
			Description: "copy line",
			Handler: func(buf vimtea.Buffer) tea.Cmd {
				return synthVimMotion(e, "y", "y")
			},
		})

		e.AddBinding(vimtea.KeyBinding{
			Key:         "ctrl+v",
			Mode:        mode,
			Description: "paste",
			Handler: func(buf vimtea.Buffer) tea.Cmd {
				return synthVimMotion(e, "p")
			},
		})
	}
}

// synthVimMotion returns a tea.Cmd that first switches the editor to Normal
// mode and then emits the given runes as key messages in order, so the
// editor's vim engine receives a well-formed motion regardless of the mode
// the user was in when the Ctrl shortcut fired.
//
// tea.Sequence guarantees strict ordering with no interleaved messages, so
// multi-key motions like "dd" / "yy" cannot be corrupted by concurrent user
// input.
func synthVimMotion(e vimtea.Editor, keys ...string) tea.Cmd {
	cmds := make([]tea.Cmd, 0, 1+len(keys))
	cmds = append(cmds, e.SetMode(vimtea.ModeNormal))
	for _, k := range keys {
		cmds = append(cmds, func() tea.Msg {
			return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		})
	}
	return tea.Sequence(cmds...)
}
