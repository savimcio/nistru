package main

import (
	"reflect"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/kujtimiihoxha/vimtea"
)

// -----------------------------------------------------------------------------
// Fake vimtea.Editor that records the bindings registered against it. Just
// enough to satisfy the interface — most methods return zero values. This lets
// us assert addCtrlBindings' behaviour (which keys, which modes) without
// dragging a real editor through the test.

type recordingEditor struct {
	bindings []vimtea.KeyBinding
}

var _ vimtea.Editor = (*recordingEditor)(nil)

func (r *recordingEditor) Init() tea.Cmd                              { return nil }
func (r *recordingEditor) Update(tea.Msg) (tea.Model, tea.Cmd)        { return r, nil }
func (r *recordingEditor) View() string                               { return "" }
func (r *recordingEditor) AddBinding(b vimtea.KeyBinding)             { r.bindings = append(r.bindings, b) }
func (r *recordingEditor) AddCommand(string, vimtea.CommandFn)        {}
func (r *recordingEditor) GetBuffer() vimtea.Buffer                   { return nil }
func (r *recordingEditor) GetMode() vimtea.EditorMode                 { return vimtea.ModeNormal }
func (r *recordingEditor) SetMode(m vimtea.EditorMode) tea.Cmd {
	return func() tea.Msg { return setModeSentinel{mode: m} }
}
func (r *recordingEditor) SetStatusMessage(string) tea.Cmd            { return nil }
func (r *recordingEditor) SetSize(int, int) (tea.Model, tea.Cmd)      { return r, nil }

// setModeSentinel is emitted by the fake's SetMode so tests can verify the
// SetMode cmd is the first entry in the sequence produced by synthVimMotion.
type setModeSentinel struct {
	mode vimtea.EditorMode
}

// -----------------------------------------------------------------------------
// addCtrlBindings — the five Ctrl shortcuts must be registered across all
// three supported modes (Normal, Insert, Visual), because the keys are
// expected to work identically regardless of the user's current mode. The
// regression fixed in commit 0ee96ea was not "insert mode treats them as
// literal input" — it was the opposite: before the fix, synthVimMotion would
// emit "dd" / "yy" / "p" while the user was still in Insert mode, causing
// the vim engine to insert literal characters. The fix switches to Normal
// first; the test below asserts both halves of that contract.

func TestAddCtrlBindings_RegistersFiveKeysAcrossThreeModes(t *testing.T) {
	rec := &recordingEditor{}
	addCtrlBindings(rec)

	wantKeys := []string{"ctrl+z", "ctrl+y", "ctrl+x", "ctrl+c", "ctrl+v"}
	wantModes := []vimtea.EditorMode{vimtea.ModeNormal, vimtea.ModeInsert, vimtea.ModeVisual}

	if got, want := len(rec.bindings), len(wantKeys)*len(wantModes); got != want {
		t.Fatalf("binding count: got %d, want %d", got, want)
	}

	// Index by (mode, key) for easy lookup.
	byModeKey := make(map[vimtea.EditorMode]map[string]bool)
	for _, b := range rec.bindings {
		if byModeKey[b.Mode] == nil {
			byModeKey[b.Mode] = map[string]bool{}
		}
		byModeKey[b.Mode][b.Key] = true
	}
	for _, mode := range wantModes {
		for _, key := range wantKeys {
			if !byModeKey[mode][key] {
				t.Errorf("missing binding: mode=%v key=%q", mode, key)
			}
		}
	}

	// Every binding must carry a non-nil Handler.
	for _, b := range rec.bindings {
		if b.Handler == nil {
			t.Errorf("nil handler on binding mode=%v key=%q", b.Mode, b.Key)
		}
	}
}

// -----------------------------------------------------------------------------
// synthVimMotion — the regression-fix contract: the returned tea.Cmd's
// sequence begins with SetMode(ModeNormal), then emits the supplied key
// runes in order. In Insert mode, this prevents the synthesised keys from
// being treated as literal characters by the vim engine.

func TestSynthVimMotion_PrependsSetModeNormalThenEmitsKeys(t *testing.T) {
	cases := []struct {
		name string
		keys []string
	}{
		{"cut (ctrl+x)", []string{"d", "d"}},
		{"copy (ctrl+c)", []string{"y", "y"}},
		{"paste (ctrl+v)", []string{"p"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recordingEditor{}
			cmd := synthVimMotion(rec, tc.keys...)
			if cmd == nil {
				t.Fatalf("synthVimMotion returned nil cmd")
			}
			msg := cmd()
			cmds := extractSequenceCmds(t, msg)

			// Total count: 1 (SetMode) + len(keys).
			if got, want := len(cmds), 1+len(tc.keys); got != want {
				t.Fatalf("sequence length: got %d, want %d (keys=%v)", got, want, tc.keys)
			}

			// First cmd must resolve to our SetMode sentinel with ModeNormal.
			first := cmds[0]()
			sent, ok := first.(setModeSentinel)
			if !ok {
				t.Fatalf("first cmd in sequence: expected setModeSentinel, got %T (%v)", first, first)
			}
			if sent.mode != vimtea.ModeNormal {
				t.Errorf("first cmd: mode=%v, want ModeNormal", sent.mode)
			}

			// Remaining cmds must be key messages in the declared order.
			for i, want := range tc.keys {
				gotMsg := cmds[1+i]()
				km, ok := gotMsg.(tea.KeyMsg)
				if !ok {
					t.Fatalf("cmd %d: expected tea.KeyMsg, got %T (%v)", 1+i, gotMsg, gotMsg)
				}
				if km.Type != tea.KeyRunes {
					t.Errorf("cmd %d: KeyMsg.Type=%v, want KeyRunes", 1+i, km.Type)
				}
				if string(km.Runes) != want {
					t.Errorf("cmd %d: runes=%q, want %q", 1+i, string(km.Runes), want)
				}
			}
		})
	}
}

// extractSequenceCmds pulls the underlying []tea.Cmd out of a tea.Sequence's
// sequenceMsg. The type is unexported in bubbletea, so we reach in via
// reflection — acceptable here because this is a test-only helper and the
// bubbletea API of Sequence is stable.
func extractSequenceCmds(t *testing.T, msg tea.Msg) []tea.Cmd {
	t.Helper()
	v := reflect.ValueOf(msg)
	if v.Kind() != reflect.Slice {
		t.Fatalf("expected sequenceMsg (a []tea.Cmd slice), got %T (kind=%v)", msg, v.Kind())
	}
	out := make([]tea.Cmd, v.Len())
	for i := 0; i < v.Len(); i++ {
		item := v.Index(i).Interface()
		c, ok := item.(tea.Cmd)
		if !ok {
			t.Fatalf("sequence element %d: not a tea.Cmd (%T)", i, item)
		}
		out[i] = c
	}
	return out
}

// -----------------------------------------------------------------------------
// newEditor integration-lite: constructing a fresh editor and re-opening
// (two distinct newEditor calls back-to-back) must not panic and must
// produce a usable Editor interface. This exercises the "rebuilt on every
// file open" path from model.go without needing a tea.Program.

func TestNewEditor_ConstructsUsableEditor(t *testing.T) {
	e := newEditor("hello", "/tmp/sample.go")
	if e == nil {
		t.Fatalf("newEditor returned nil")
	}
	// Sanity: default mode on a freshly-built editor is Normal.
	if e.GetMode() != vimtea.ModeNormal {
		t.Errorf("fresh editor mode: got %v, want ModeNormal", e.GetMode())
	}
	// Rebuild (as happens on file-switch in model.go openFile) — must not
	// panic or drop bindings.
	e2 := newEditor("world", "/tmp/other.go")
	if e2 == nil {
		t.Fatalf("second newEditor returned nil")
	}
}
