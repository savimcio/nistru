package editor

import (
	"strings"
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/savimcio/nistru/internal/config"
	"github.com/savimcio/nistru/plugin"
)

// -----------------------------------------------------------------------------
// langFromPath — lowercase extension, no leading dot, empty when extension
// absent. Note: the real function is named langFromPath (not
// languageFromPath) and does NOT special-case filenames like "Makefile" or
// "Dockerfile" — they resolve to "" because filepath.Ext returns "".

func TestLangFromPath_Table(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"main.go", "go"},
		{"README.md", "md"},
		{"notes.rs", "rs"},
		{"Makefile", ""}, // no extension
		{"Dockerfile", ""},
		{"no-ext", ""},
		{"/abs/path/to/file.py", "py"},
		{"nested/dir/script.sh", "sh"},
		{"HEADER.GO", "go"},       // uppercase normalised to lowercase
		{".hidden", "hidden"},     // dotfiles: filepath.Ext returns ".hidden"; TrimPrefix gives "hidden"
		{"archive.tar.gz", "gz"},  // only the last extension
		{"", ""},                  // empty path
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := langFromPath(tc.in); got != tc.want {
				t.Errorf("langFromPath(%q): got %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// upsertStatusSegment behaviour: insert new, update existing (same
// plugin+segment), preserve order on update, empty-text removal.

func TestUpsertStatusSegment_InsertNew(t *testing.T) {
	m := &Model{}
	m.upsertStatusSegment("gofmt", "status", "ok", "42")
	if len(m.statusSegments) != 1 {
		t.Fatalf("want 1 segment, got %d", len(m.statusSegments))
	}
	seg := m.statusSegments[0]
	if seg.Plugin != "gofmt" || seg.Name != "status" || seg.Text != "ok" || seg.Color != "42" {
		t.Errorf("wrong segment inserted: %+v", seg)
	}
}

func TestUpsertStatusSegment_UpdateExistingPreservesOrder(t *testing.T) {
	m := &Model{}
	m.upsertStatusSegment("p1", "a", "first", "1")
	m.upsertStatusSegment("p2", "b", "second", "2")
	m.upsertStatusSegment("p3", "c", "third", "3")

	// Update the middle one; position must stay at index 1.
	m.upsertStatusSegment("p2", "b", "second-updated", "22")

	if len(m.statusSegments) != 3 {
		t.Fatalf("want 3 segments, got %d", len(m.statusSegments))
	}
	if m.statusSegments[0].Plugin != "p1" {
		t.Errorf("order disturbed at index 0: %+v", m.statusSegments[0])
	}
	if m.statusSegments[1].Plugin != "p2" || m.statusSegments[1].Text != "second-updated" || m.statusSegments[1].Color != "22" {
		t.Errorf("update dropped at index 1: %+v", m.statusSegments[1])
	}
	if m.statusSegments[2].Plugin != "p3" {
		t.Errorf("order disturbed at index 2: %+v", m.statusSegments[2])
	}
}

func TestUpsertStatusSegment_DistinctSegmentsSamePlugin(t *testing.T) {
	// Plugin contract: a single plugin may own multiple segments, keyed by
	// Name. upsert must not collapse them.
	m := &Model{}
	m.upsertStatusSegment("p1", "lhs", "L", "1")
	m.upsertStatusSegment("p1", "rhs", "R", "2")
	if len(m.statusSegments) != 2 {
		t.Fatalf("want 2 segments, got %d: %+v", len(m.statusSegments), m.statusSegments)
	}
}

func TestUpsertStatusSegment_EmptyTextRemoves(t *testing.T) {
	m := &Model{}
	m.upsertStatusSegment("p1", "a", "x", "")
	m.upsertStatusSegment("p1", "b", "y", "")
	m.upsertStatusSegment("p1", "a", "", "") // remove
	if len(m.statusSegments) != 1 {
		t.Fatalf("after remove: want 1, got %d: %+v", len(m.statusSegments), m.statusSegments)
	}
	if m.statusSegments[0].Name != "b" {
		t.Errorf("wrong survivor after remove: %+v", m.statusSegments[0])
	}
}

func TestUpsertStatusSegment_EmptyTextOnMissingNoOp(t *testing.T) {
	m := &Model{}
	m.upsertStatusSegment("never", "seen", "", "")
	if len(m.statusSegments) != 0 {
		t.Errorf("empty-text upsert on missing key must be no-op; got %+v", m.statusSegments)
	}
}

// -----------------------------------------------------------------------------
// filterSegments — drops segments owned by the given plugin, preserves order
// for the rest.

func TestFilterSegments_DropsPluginPreservingOrder(t *testing.T) {
	in := []statusSegment{
		{Plugin: "a", Name: "1"},
		{Plugin: "b", Name: "2"},
		{Plugin: "a", Name: "3"},
		{Plugin: "c", Name: "4"},
		{Plugin: "a", Name: "5"},
	}
	out := filterSegments(in, "a")
	if len(out) != 2 {
		t.Fatalf("want 2 survivors, got %d: %+v", len(out), out)
	}
	if out[0].Plugin != "b" || out[0].Name != "2" {
		t.Errorf("wrong first survivor: %+v", out[0])
	}
	if out[1].Plugin != "c" || out[1].Name != "4" {
		t.Errorf("wrong second survivor: %+v", out[1])
	}
}

func TestFilterSegments_NoMatchReturnsEquivalent(t *testing.T) {
	in := []statusSegment{
		{Plugin: "a", Name: "1"},
		{Plugin: "b", Name: "2"},
	}
	out := filterSegments(in, "nonexistent")
	if len(out) != 2 {
		t.Fatalf("want all preserved, got %d", len(out))
	}
	if out[0].Name != "1" || out[1].Name != "2" {
		t.Errorf("order not preserved: %+v", out)
	}
}

func TestFilterSegments_AllDroppedReturnsEmpty(t *testing.T) {
	in := []statusSegment{
		{Plugin: "a", Name: "1"},
		{Plugin: "a", Name: "2"},
	}
	out := filterSegments(in, "a")
	if len(out) != 0 {
		t.Errorf("want empty, got %+v", out)
	}
}

// -----------------------------------------------------------------------------
// renderStatusBar — width invariants. renderStatusBar reads mode via
// m.editor.Mode(); a fakeEditor stub is enough because we never dispatch a
// message through it — the status-bar render path is all Model state plus a
// single Mode() call.

func TestRenderStatusBar_WidthInvariants(t *testing.T) {
	newModel := func(width int, segs []statusSegment, openPath, root string) *Model {
		return &Model{
			editor:         &fakeEditor{},
			width:          width,
			height:         24,
			openPath:       openPath,
			root:           root,
			statusSegments: segs,
		}
	}

	segs := func(n int) []statusSegment {
		out := make([]statusSegment, 0, n)
		for i := range n {
			out = append(out, statusSegment{
				Plugin: "p",
				Name:   "s",
				Text:   "seg" + string(rune('A'+i)),
			})
		}
		return out
	}

	cases := []struct {
		name     string
		width    int
		segs     []statusSegment
		openPath string
	}{
		{"narrow-empty", 40, nil, ""},
		{"narrow-two-segs", 40, segs(2), "short.go"},
		{"medium-three-segs", 80, segs(3), "pkg/subpkg/file.go"},
		{"wide-five-segs", 120, segs(5), "deeply/nested/module/path/to/some-file.go"},
		{"very-long-path-triggers-ellipsis", 40, nil, strings.Repeat("dir/", 20) + "file.go"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newModel(tc.width, tc.segs, tc.openPath, "")
			got := m.renderStatusBar()

			// Invariant 1: rendered width (cell width) must not exceed input
			// width. lipgloss.Width is the source of truth for on-screen
			// width because it strips ANSI and counts runes correctly.
			if w := lipgloss.Width(got); w > tc.width {
				t.Errorf("width=%d: rendered width %d exceeds input", tc.width, w)
			}

			// Invariant 2: the output must be valid UTF-8. This is a
			// necessary condition for "no segment cut mid-rune".
			// Strip ANSI first — lipgloss emits escape sequences, and while
			// those are valid UTF-8 on their own, the check is about the
			// visible text being well-formed after renderStatusBar does its
			// truncation arithmetic on runes.
			if !utf8.ValidString(got) {
				t.Errorf("width=%d: output contains invalid UTF-8", tc.width)
			}

			// Invariant 3: render must not panic or produce an empty string
			// at any tested width. (Content sanity.)
			if got == "" {
				t.Errorf("width=%d: unexpected empty render", tc.width)
			}
		})
	}
}

func TestRenderStatusBar_LongPathShowsLeadingEllipsis(t *testing.T) {
	// Force the path through the leading-ellipsis branch: width is small,
	// path is long and absolute.
	m := &Model{
		editor:   &fakeEditor{},
		width:    40,
		height:   24,
		openPath: "/absolute/" + strings.Repeat("very-long-segment/", 10) + "file.go",
		root:     "/unrelated", // so filepath.Rel bails (HasPrefix "..")
	}
	out := m.renderStatusBar()
	// The ellipsis rune is "…" (U+2026). Its presence is the observable
	// signal that the path was truncated from the left.
	if !strings.Contains(out, "…") {
		t.Errorf("expected leading ellipsis in truncated path; got %q", out)
	}
	if w := lipgloss.Width(out); w > 40 {
		t.Errorf("width overflow: got %d, want <=40", w)
	}
}

// -----------------------------------------------------------------------------
// Small pure helpers on Model.

func TestEditorWidth_WithAndWithoutLeftPane(t *testing.T) {
	// Without a left pane, editor fills the full width.
	m := &Model{width: 100}
	if got := m.editorWidth(); got != 100 {
		t.Errorf("no leftPane: got %d, want 100", got)
	}

	// With a left pane, the editor's width is width - tree-pane width.
	// m.treeWidth() falls back to config.Defaults().UI.TreeWidth when
	// m.cfg is nil — matching these hand-built Models.
	m2 := &Model{width: 100, leftPane: stubPane{}}
	want := 100 - config.Defaults().UI.TreeWidth
	if got := m2.editorWidth(); got != want {
		t.Errorf("with leftPane: got %d, want %d", got, want)
	}

	// Clamped to >= 1 when width is very small.
	m3 := &Model{width: 1, leftPane: stubPane{}}
	if got := m3.editorWidth(); got != 1 {
		t.Errorf("tiny width: got %d, want 1 (clamp)", got)
	}
}

// stubPane is a zero-behaviour plugin.Pane for tests that only need
// editorWidth to observe a non-nil leftPane.
type stubPane struct{}

func (stubPane) Render(int, int) string                    { return "" }
func (stubPane) OnResize(int, int)                         {}
func (stubPane) OnFocus(bool)                              {}
func (stubPane) Slot() string                              { return "left" }
func (stubPane) HandleKey(plugin.KeyEvent) []plugin.Effect { return nil }

func TestModeName_AllKnownModes(t *testing.T) {
	cases := []struct {
		in   Mode
		want string
	}{
		{ModeNormal, "NORMAL"},
		{ModeInsert, "INSERT"},
		{ModeVisual, "VISUAL"},
		{ModeCommand, "COMMAND"},
		{Mode(42), "?"}, // unknown
	}
	for _, tc := range cases {
		if got := modeName(tc.in); got != tc.want {
			t.Errorf("modeName(%v): got %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestKeyEventFromTea_CopiesFields(t *testing.T) {
	// In v2, printable keystrokes arrive with Text set and Mod carrying any
	// modifiers (Alt included). We build an Alt+"hi" chord to exercise both
	// the Text → Runes copy and the Alt detection branch.
	km := tea.KeyPressMsg{Text: "hi", Mod: tea.ModAlt}
	got := keyEventFromTea(km)
	if got.Key != km.String() {
		t.Errorf("Key: got %q, want %q", got.Key, km.String())
	}
	if string(got.Runes) != "hi" {
		t.Errorf("Runes: got %q, want %q", string(got.Runes), "hi")
	}
	if !got.Alt {
		t.Errorf("Alt: want true, got false")
	}
}

// applyEffects: empty-slice fast path, Invalidate (no-op), and Focus-without-
// leftPane path. All three paths are pure — they don't touch the editor or
// the plugin host.

func TestApplyEffects_EmptySliceReturnsNil(t *testing.T) {
	m := &Model{}
	if cmd := m.applyEffects(nil); cmd != nil {
		t.Errorf("empty effects should return nil cmd, got %T", cmd)
	}
	if cmd := m.applyEffects([]plugin.Effect{}); cmd != nil {
		t.Errorf("zero-length effects should return nil cmd, got %T", cmd)
	}
}

func TestApplyEffects_InvalidateIsNoOp(t *testing.T) {
	m := &Model{}
	if cmd := m.applyEffects([]plugin.Effect{plugin.Invalidate{}}); cmd != nil {
		t.Errorf("Invalidate should produce no cmd, got %T", cmd)
	}
}

func TestApplyEffects_FocusWithoutLeftPaneUpdatesFocusField(t *testing.T) {
	// Without a leftPane the OnFocus call is skipped, so we can observe the
	// focus field change without needing a pane fake.
	m := &Model{focus: focusEditor}
	cmd := m.applyEffects([]plugin.Effect{plugin.Focus{Pane: "left"}})
	if cmd != nil {
		t.Errorf("Focus alone should produce no cmd, got %T", cmd)
	}
	if m.focus != focusTree {
		t.Errorf("focus should flip to tree, got %v", m.focus)
	}

	// Flip back via Pane: "editor".
	cmd = m.applyEffects([]plugin.Effect{plugin.Focus{Pane: "editor"}})
	if cmd != nil {
		t.Errorf("Focus alone should produce no cmd, got %T", cmd)
	}
	if m.focus != focusEditor {
		t.Errorf("focus should flip to editor, got %v", m.focus)
	}

	// "right" is an alias for focusEditor per model.go.
	m.focus = focusTree
	_ = m.applyEffects([]plugin.Effect{plugin.Focus{Pane: "right"}})
	if m.focus != focusEditor {
		t.Errorf("Pane=\"right\" should alias to focusEditor; got %v", m.focus)
	}
}

func TestApplyEffects_OpenFileProducesOpenFileRequestMsg(t *testing.T) {
	m := &Model{}
	cmd := m.applyEffects([]plugin.Effect{plugin.OpenFile{Path: "/some/file.go"}})
	if cmd == nil {
		t.Fatalf("OpenFile effect should produce a cmd")
	}
	msg := cmd()
	req, ok := msg.(openFileRequestMsg)
	if !ok {
		t.Fatalf("expected openFileRequestMsg, got %T (%v)", msg, msg)
	}
	if req.path != "/some/file.go" {
		t.Errorf("path: got %q, want %q", req.path, "/some/file.go")
	}
}

func TestApplyEffects_NotifyEmptyMessageIsNoOp(t *testing.T) {
	// Notify with empty Message should not produce a cmd. This exercises
	// the early-skip branch without needing a real editor.
	m := &Model{editor: &fakeEditor{}}
	cmd := m.applyEffects([]plugin.Effect{plugin.Notify{Message: ""}})
	if cmd != nil {
		t.Errorf("empty Notify should be no-op; got cmd %T", cmd)
	}
}

func TestApplyEffects_InvalidateAndOpenFile_BatchedWhenMixed(t *testing.T) {
	// Invalidate produces no cmd; OpenFile does. Mixing both must not drop
	// the OpenFile cmd.
	m := &Model{}
	cmd := m.applyEffects([]plugin.Effect{
		plugin.Invalidate{},
		plugin.OpenFile{Path: "/x"},
		plugin.Invalidate{},
	})
	if cmd == nil {
		t.Fatalf("expected cmd from OpenFile; got nil")
	}
}

// Update dispatch: a small slice of the state machine that needs neither
// plugin host nor tea.Program. The WindowSizeMsg path is pure state mutation
// on m.width / m.height (plus editor.SetSize, which is recorded by the
// fakeEditor).

func TestUpdate_WindowSizeMsgSetsDimsAndForwardsToEditor(t *testing.T) {
	fe := &fakeEditor{}
	m := &Model{editor: fe}
	newM, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	got, ok := newM.(*Model)
	if !ok {
		t.Fatalf("Update should return *Model; got %T", newM)
	}
	if got.width != 120 || got.height != 40 {
		t.Errorf("dims not stored: width=%d height=%d", got.width, got.height)
	}
	if !fe.setSize.called {
		t.Errorf("expected WindowSizeMsg to trigger editor.SetSize")
	}
}

// Default-case Update dispatch (unknown tea.Msg) must call forwardToFocused
// without panicking. With no leftPane and focusTree, non-KeyMsg traffic is
// dropped silently — exercise that early return.

type unknownMsg struct{}

func TestUpdate_UnknownMsgWithNoLeftPaneIsNoOp(t *testing.T) {
	m := &Model{
		editor:   &fakeEditor{},
		focus:    focusTree,
		leftPane: nil,
	}
	newM, cmd := m.Update(unknownMsg{})
	// Compare via the concrete type — Update returns tea.Model, but the
	// underlying value is our *Model pointer.
	got, ok := newM.(*Model)
	if !ok || got != m {
		t.Errorf("Update should return the same *Model pointer; got %T (%v)", newM, newM)
	}
	if cmd != nil {
		t.Errorf("unknown msg with focusTree+no leftPane should produce nil cmd")
	}
}

// handleKey: pure sub-paths that don't require a plugin host.
//
//  - Ctrl+P opens the palette (pure state mutation).
//  - Tab toggles focus (pure, with no leftPane nothing side-effects).
//  - Ctrl+S with no open file short-circuits through flushNow (early return
//    when openPath=="") and produces only an editor status message.

func TestHandleKey_CtrlPOpensPalette(t *testing.T) {
	m := &Model{
		editor:   &fakeEditor{},
		commands: map[string]plugin.CommandRef{"a": {Title: "A"}},
	}
	newM, cmd := m.handleKey(tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
	if cmd != nil {
		t.Errorf("Ctrl+P should produce no cmd; got %T", cmd)
	}
	got, ok := newM.(*Model)
	if !ok {
		t.Fatalf("expected *Model; got %T", newM)
	}
	if !got.palette.open {
		t.Errorf("palette should be open after Ctrl+P")
	}
}

func TestHandleKey_TabTogglesFocus(t *testing.T) {
	m := &Model{
		editor: &fakeEditor{},
		focus:  focusTree,
	}
	newM, _ := m.handleKey(tea.KeyPressMsg{Code: tea.KeyTab})
	got := newM.(*Model)
	if got.focus != focusEditor {
		t.Errorf("Tab from tree should move focus to editor; got %v", got.focus)
	}
	newM, _ = got.handleKey(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	got = newM.(*Model)
	if got.focus != focusTree {
		t.Errorf("Shift+Tab from editor should move focus back to tree; got %v", got.focus)
	}
}

func TestHandleKey_CtrlSWithNoOpenFileIsNoOp(t *testing.T) {
	// flushNow early-returns when openPath=="" — no file write, just a
	// status message cmd.
	m := &Model{editor: &fakeEditor{}, openPath: ""}
	_, cmd := m.handleKey(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	if cmd == nil {
		t.Errorf("Ctrl+S should produce a status-message cmd even with no open file")
	}
}

// handlePaletteKey: dismissing via esc is a pure close; activating isn't
// reachable without a plugin host, so skip that branch here (T4 territory).

func TestHandlePaletteKey_EscClosesPalette(t *testing.T) {
	m := &Model{
		editor:   &fakeEditor{},
		commands: map[string]plugin.CommandRef{"a": {Title: "A"}},
	}
	m.palette.Open(m.commands)
	if !m.palette.open {
		t.Fatalf("precondition: palette should be open")
	}
	newM, cmd := m.handlePaletteKey(tea.KeyPressMsg{Code: tea.KeyEsc})
	if cmd != nil {
		t.Errorf("esc in palette should produce no cmd; got %T", cmd)
	}
	got := newM.(*Model)
	if got.palette.open {
		t.Errorf("palette should be closed after esc")
	}
}

// renderFrame early-return path: before any WindowSizeMsg, width/height are
// zero and renderFrame must produce an empty string so the initial frame is
// blank. We call renderFrame() directly to inspect the body independently of
// the tea.View wrapper View() returns in v2.

func TestRenderFrame_EarlyReturnWhenUnsized(t *testing.T) {
	m := &Model{editor: &fakeEditor{}, width: 0, height: 0}
	if got := m.renderFrame(); got != "" {
		t.Errorf("unsized renderFrame should return empty string, got %q", got)
	}
}

// paletteStatus: footer hint varies with filter emptiness.

func TestPaletteStatus_EmptyVsPopulated(t *testing.T) {
	empty := &paletteModel{}
	if got := paletteStatus(empty); !strings.Contains(got, "esc") {
		t.Errorf("empty palette status should mention esc; got %q", got)
	}

	populated := &paletteModel{filtered: []paletteEntry{{ID: "a"}}}
	got := paletteStatus(populated)
	if !strings.Contains(got, "up") || !strings.Contains(got, "enter") {
		t.Errorf("populated palette status should mention nav keys; got %q", got)
	}
}

func TestRenderStatusBar_DropsSegmentsWhenMiddleOverflows(t *testing.T) {
	// Enough segments to force right-to-left drop when the budget is tight.
	// Segment text is short so the path is the main eater of width.
	segsIn := []statusSegment{
		{Plugin: "p", Name: "a", Text: "AAAA"},
		{Plugin: "p", Name: "b", Text: "BBBB"},
		{Plugin: "p", Name: "c", Text: "CCCC"},
		{Plugin: "p", Name: "d", Text: "DDDD"},
		{Plugin: "p", Name: "e", Text: "EEEE"},
	}
	m := &Model{
		editor:         &fakeEditor{},
		width:          40,
		height:         24,
		openPath:       "a/path.go",
		statusSegments: segsIn,
	}
	out := m.renderStatusBar()
	// At width=40 with mode "[NORMAL]" + a path + 5 chunky segments, not
	// every segment fits. Confirm right-to-left drop: earlier segments are
	// more likely to survive than later ones. We don't assert an exact
	// count (depends on lipgloss padding math), but we assert that if
	// "EEEE" survives then "AAAA" must too (monotonic drop from the right).
	if strings.Contains(out, "EEEE") && !strings.Contains(out, "AAAA") {
		t.Errorf("drop order should be right-to-left; output=%q", out)
	}
	if w := lipgloss.Width(out); w > 40 {
		t.Errorf("width overflow: got %d, want <=40", w)
	}
}
