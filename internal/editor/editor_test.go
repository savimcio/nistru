package editor

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// -----------------------------------------------------------------------------
// fakeEditor — a minimal test double that implements the nistru-local Editor
// interface. It records the messages it receives via Update along with the
// calls made to Set* and Undo/Redo helpers, so component tests can assert
// "the Model handed the key to the editor" without depending on goeditor's
// real vim interpreter.
//
// Deliberately minimal: only the state that current tests observe is
// recorded. New tests can grow this as needed.

type fakeEditor struct {
	// Recording fields.
	updateMsgs   []tea.Msg
	setModeCalls []Mode
	setSize      struct {
		called bool
		w, h   int
	}
	statusMsgs []string
	undoCount  int
	redoCount  int

	// Stubbed state.
	content string
	mode    Mode
	row     int
	col     int
}

var _ Editor = (*fakeEditor)(nil)

func (f *fakeEditor) Init() tea.Cmd { return nil }

func (f *fakeEditor) Update(msg tea.Msg) (Editor, tea.Cmd) {
	f.updateMsgs = append(f.updateMsgs, msg)
	return f, nil
}

func (f *fakeEditor) View() string { return "" }

func (f *fakeEditor) SetSize(w, h int) tea.Cmd {
	f.setSize.called = true
	f.setSize.w = w
	f.setSize.h = h
	return nil
}

func (f *fakeEditor) SetContent(text string) { f.content = text }

func (f *fakeEditor) Content() string { return f.content }

func (f *fakeEditor) Mode() Mode { return f.mode }

func (f *fakeEditor) SetMode(m Mode) tea.Cmd {
	f.setModeCalls = append(f.setModeCalls, m)
	f.mode = m
	return nil
}

func (f *fakeEditor) CursorRowCol() (int, int) { return f.row, f.col }

func (f *fakeEditor) Undo() tea.Cmd {
	f.undoCount++
	return nil
}

func (f *fakeEditor) Redo() tea.Cmd {
	f.redoCount++
	return nil
}

// SetStatusMessage mirrors the real adapter's contract: always returns a
// non-nil tea.Cmd (the adapter backs this with goeditor's DispatchMessage,
// which is never nil). Tests distinguish "no-op" from "fired" via the
// statusMsgs recording, not via cmd-nilness.
func (f *fakeEditor) SetStatusMessage(msg string) tea.Cmd {
	f.statusMsgs = append(f.statusMsgs, msg)
	return func() tea.Msg { return nil }
}

// -----------------------------------------------------------------------------
// newEditor integration-lite: constructing a fresh editor (twice, as the
// open-file flow in model.go does) must not panic and must produce a usable
// Editor interface value. The adapter's own behavior is exercised in
// goeditor_adapter_test.go — this test only guards the factory path.

func TestNewEditor_ConstructsUsableEditor(t *testing.T) {
	e := newEditor("hello", "/tmp/sample.go")
	if e == nil {
		t.Fatalf("newEditor returned nil")
	}
	// Default mode on a freshly-built editor is Normal (goeditor starts in
	// its own Normal equivalent; the adapter maps that through to
	// ModeNormal).
	if got := e.Mode(); got != ModeNormal {
		t.Errorf("fresh editor mode: got %v, want ModeNormal", got)
	}
	// Rebuild (as happens on file-switch in model.openFile) — must not
	// panic and must return a distinct, usable instance.
	e2 := newEditor("world", "/tmp/other.go")
	if e2 == nil {
		t.Fatalf("second newEditor returned nil")
	}
}

// -----------------------------------------------------------------------------
// fakeEditor sanity: exercising a handful of operations should leave the
// expected traces. This covers the fake itself so downstream tests that rely
// on it aren't debugging the helper when they fail.

func TestFakeEditor_RecordsUpdatesAndStateChanges(t *testing.T) {
	f := &fakeEditor{}

	// Update records whatever tea.Msg flows through it.
	msg := tea.KeyPressMsg{Code: 'a', Text: "a"}
	if got, cmd := f.Update(msg); got != f || cmd != nil {
		t.Errorf("Update: got (%v, %v), want (self, nil)", got, cmd)
	}
	if len(f.updateMsgs) != 1 || f.updateMsgs[0] != msg {
		t.Errorf("updateMsgs: got %v, want [%v]", f.updateMsgs, msg)
	}

	// SetMode records and mutates the stubbed mode.
	f.SetMode(ModeInsert)
	if f.Mode() != ModeInsert {
		t.Errorf("Mode after SetMode(ModeInsert): got %v, want ModeInsert", f.Mode())
	}
	if len(f.setModeCalls) != 1 || f.setModeCalls[0] != ModeInsert {
		t.Errorf("setModeCalls: got %v", f.setModeCalls)
	}

	// SetSize records w/h.
	f.SetSize(80, 24)
	if !f.setSize.called || f.setSize.w != 80 || f.setSize.h != 24 {
		t.Errorf("setSize: got %+v, want {called:true w:80 h:24}", f.setSize)
	}

	// Undo/Redo bump counters.
	f.Undo()
	f.Undo()
	f.Redo()
	if f.undoCount != 2 || f.redoCount != 1 {
		t.Errorf("undo/redo counters: got undo=%d redo=%d, want undo=2 redo=1",
			f.undoCount, f.redoCount)
	}

	// SetStatusMessage records the message.
	f.SetStatusMessage("hello")
	if len(f.statusMsgs) != 1 || f.statusMsgs[0] != "hello" {
		t.Errorf("statusMsgs: got %v, want [%q]", f.statusMsgs, "hello")
	}
}
