// Package editor's local Editor interface. Abstracts the underlying vim
// editor so the Model is not coupled to any specific library. Currently
// implemented by goeditorAdapter (prod) and fakeEditor (tests).
//
// This interface is intentionally narrower than goeditor.Model — the outer
// Model only needs the pieces listed here. Keeping the surface small also
// keeps the fake/test double small.
package editor

import tea "charm.land/bubbletea/v2"

// Mode is the editor mode exposed to nistru. This is a superset of what
// goeditor tracks directly: ModeCommand is a nistru-level concept that the
// adapter maps to Normal + a status message (goeditor has its own command
// mode which we do not surface here).
type Mode int

const (
	ModeNormal Mode = iota
	ModeInsert
	ModeVisual
	// ModeCommand has no goeditor equivalent. The adapter's SetMode(ModeCommand)
	// falls back to SetNormalMode + a DispatchMessage call so the outer model
	// can still flash a ":" prompt.
	ModeCommand
)

// Editor is the narrow interface nistru's Model drives. We deliberately
// return Editor (not tea.Model) from Update so call sites don't have to
// type-assert on every tick — a small, intentional deviation from tea.Model's
// signature. The adapter re-asserts goeditor's tea.Model internally.
//
// View() returns a plain string because the pane it produces is composed
// into the outer v2 tea.View by Model.View() via lipgloss joins. We don't
// plumb tea.View through this interface.
//
// SetSize returns only tea.Cmd — the adapter mutates its inner model in
// place, which keeps call sites simpler than returning (tea.Model, tea.Cmd)
// and forcing callers to type-assert.
type Editor interface {
	Init() tea.Cmd
	Update(msg tea.Msg) (Editor, tea.Cmd)
	View() string

	SetSize(w, h int) tea.Cmd
	SetContent(text string)
	Content() string

	Mode() Mode
	SetMode(m Mode) tea.Cmd

	CursorRowCol() (row, col int)

	Undo() tea.Cmd
	Redo() tea.Cmd

	SetStatusMessage(msg string) tea.Cmd
}
