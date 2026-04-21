package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/kujtimiihoxha/vimtea"

	"github.com/savimcio/nistru/plugin"
	"github.com/savimcio/nistru/plugins/treepane"
)

// App-level key constants. Inlining these rather than splitting into keys.go
// because there are only a handful.
const (
	keyTab      = "tab"
	keyShiftTab = "shift+tab"
	keyCtrlS    = "ctrl+s"
	keyCtrlQ    = "ctrl+q"
	keyCtrlC    = "ctrl+c"
	keyCtrlP    = "ctrl+p"
)

// Layout constants.
const (
	treeWidth      = 30
	statusBarLines = 1
	maxFileSize    = 1 << 20 // 1 MiB — refuse to open files larger than this
	savedFadeAfter = 3 * time.Second
)

type focus int

const (
	focusTree focus = iota
	focusEditor
)

// nowFunc returns the current time. Tests override this to pin lastSavedAt to
// a deterministic value when exercising the autosave loop end-to-end without a
// real tea.Program. Intentionally a bare var rather than a Clock interface —
// the seam only needs to support a single call site (flushNow) and a single
// override point.
var nowFunc = time.Now

// statusSegment is one plugin-contributed piece of the status bar. The
// (Plugin, Name) pair is the identity key used for upserts; Name is the
// wire-level segment identifier so one plugin can own multiple distinct
// segments.
type statusSegment struct {
	Plugin string
	Name   string
	Text   string
	Color  string
}

// Model is the top-level app model. It owns the plugin host (which owns the
// left-pane plugin), the vimtea editor, focus routing, window size, and the
// autosave/change-debounce bookkeeping.
type Model struct {
	root string

	host     *plugin.Host
	registry *plugin.Registry
	leftPane plugin.Pane

	// commands is a cached snapshot of the host's command registry. Refreshed
	// whenever a plugin registers/unregisters a command.
	commands map[string]plugin.CommandRef

	// statusSegments are plugin-contributed status bar fragments, keyed by
	// plugin name for in-place updates.
	statusSegments []statusSegment

	editor vimtea.Editor
	focus  focus

	width  int
	height int

	// lastPaneW/H track the last dims we passed to leftPane.OnResize so we
	// only notify on change.
	lastPaneW int
	lastPaneH int

	openPath    string
	lastText    string
	lastSavedAt time.Time
	saveGen     int
	changeGen   int
	dirty       bool

	// statusErr is a transient error shown in the status bar.
	statusErr string

	// palette is the Ctrl+P command palette overlay. When palette.open is
	// true, it intercepts all key events until dismissed.
	palette paletteModel
}

// NewModel constructs the initial app model rooted at root. The editor starts
// empty; opening a file through the tree replaces it.
func NewModel(root string) (*Model, error) {
	registry := plugin.NewRegistry()

	tp, err := treepane.New(root)
	if err != nil {
		return nil, fmt.Errorf("build tree plugin: %w", err)
	}
	registry.RegisterInProc(tp)

	host := plugin.NewHost(registry)
	if err := host.Start(root); err != nil {
		return nil, fmt.Errorf("start plugin host: %w", err)
	}

	m := &Model{
		root:     root,
		host:     host,
		registry: registry,
		leftPane: host.Pane("left"),
		commands: host.Commands(),
		editor:   newEditor("", ""),
		focus:    focusTree,
	}
	return m, nil
}

func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.editor.Init(), m.host.Recv())
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		contentH := m.height - statusBarLines
		contentH = max(contentH, 1)
		editorW := m.editorWidth()
		if m.leftPane != nil {
			if treeWidth != m.lastPaneW || contentH != m.lastPaneH {
				m.leftPane.OnResize(treeWidth, contentH)
				m.lastPaneW = treeWidth
				m.lastPaneH = contentH
			}
		}
		newEd, cmd := m.editor.SetSize(editorW, contentH)
		if e, ok := newEd.(vimtea.Editor); ok {
			m.editor = e
		}
		return m, cmd

	case openFileRequestMsg:
		return m.openFile(msg.path)

	case forceSaveMsg:
		if err := m.flushNow(); err != nil {
			m.statusErr = err.Error()
			return m, m.editor.SetStatusMessage("save failed: " + err.Error())
		}
		return m, m.editor.SetStatusMessage("saved")

	case forceQuitMsg:
		return m.guardedQuit()

	case saveTickMsg:
		if msg.gen != m.saveGen || !m.dirty || m.openPath == "" {
			return m, nil
		}
		if err := m.flushNow(); err != nil {
			m.statusErr = err.Error()
			return m, m.editor.SetStatusMessage("save failed: " + err.Error())
		}
		return m, nil

	case changeTickMsg:
		if msg.gen != m.changeGen || m.openPath == "" {
			return m, nil
		}
		effs := m.host.Emit(plugin.DidChange{
			Path: m.openPath,
			Text: m.editor.GetBuffer().Text(),
		})
		cmd := m.applyEffects(effs)
		return m, cmd

	case plugin.PluginStartedMsg:
		// v1: no-op. Re-arm Recv so the inbound chain stays alive.
		return m, m.host.Recv()

	case plugin.PluginExitedMsg:
		if msg.Err != nil {
			m.statusErr = fmt.Sprintf("plugin %s: %v", msg.Name, msg.Err)
		}
		// Host has already pruned commands for this plugin in its internal
		// bookkeeping — take a fresh snapshot.
		m.commands = m.host.Commands()
		// Drop any status segments owned by the exited plugin.
		m.statusSegments = filterSegments(m.statusSegments, msg.Name)
		return m, m.host.Recv()

	case plugin.PluginNotifMsg:
		return m.handlePluginNotif(msg)

	case plugin.PluginReqMsg:
		return m.handlePluginReq(msg)

	case plugin.PluginResponseMsg:
		// v1: model logic doesn't use plugin responses directly. Re-arm.
		return m, m.host.Recv()

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	// Default: forward to the focused child.
	return m.forwardToFocused(msg)
}

// editorWidth returns the editor's width in cells, allowing for a missing
// left pane (in which case the editor fills the screen).
func (m *Model) editorWidth() int {
	w := m.width
	if m.leftPane != nil {
		w = m.width - treeWidth
	}
	return max(w, 1)
}

// handlePluginNotif translates an inbound JSON-RPC notification from an
// out-of-process plugin into model state changes. Always returns a Cmd that
// batches m.host.Recv() so the inbound chain stays alive.
func (m *Model) handlePluginNotif(msg plugin.PluginNotifMsg) (tea.Model, tea.Cmd) {
	var extra tea.Cmd
	switch msg.Method {
	case "commands/register", "commands/unregister":
		m.commands = m.host.Commands()
	case "statusBar/set":
		var p struct {
			Segment string `json:"segment"`
			Text    string `json:"text"`
			Color   string `json:"color"`
		}
		if err := json.Unmarshal(msg.Params, &p); err == nil {
			m.upsertStatusSegment(msg.Plugin, p.Segment, p.Text, p.Color)
		}
	case "ui/notify":
		var p struct {
			Level   string `json:"level"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(msg.Params, &p); err == nil && p.Message != "" {
			extra = m.editor.SetStatusMessage(p.Message)
		}
	case "pane/invalidate":
		// No-op: Bubble Tea repaints every tick.
	default:
		// Unknown method — ignore.
	}
	if extra != nil {
		return m, tea.Batch(extra, m.host.Recv())
	}
	return m, m.host.Recv()
}

// handlePluginReq handles an inbound JSON-RPC request from an out-of-process
// plugin. Always responds (or explicitly logs why it couldn't), and always
// batches m.host.Recv() so the inbound chain stays alive.
func (m *Model) handlePluginReq(msg plugin.PluginReqMsg) (tea.Model, tea.Cmd) {
	switch msg.Method {
	case "buffer/edit":
		var p struct {
			Path string `json:"path"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			_ = m.host.Respond(msg.Plugin, msg.ID, nil, &plugin.RPCError{
				Code:    plugin.ErrInvalidParams,
				Message: "invalid params: " + err.Error(),
			})
			return m, m.host.Recv()
		}
		if p.Path != m.openPath || m.openPath == "" {
			_ = m.host.Respond(msg.Plugin, msg.ID, nil, &plugin.RPCError{
				Code:    plugin.ErrInvalidParams,
				Message: "path not open",
			})
			return m, m.host.Recv()
		}
		m.editor = newEditor(p.Text, p.Path)
		contentH := m.height - statusBarLines
		contentH = max(contentH, 1)
		if newEd, _ := m.editor.SetSize(m.editorWidth(), contentH); newEd != nil {
			if e, ok := newEd.(vimtea.Editor); ok {
				m.editor = e
			}
		}
		m.lastText = p.Text
		m.dirty = false
		m.saveGen++
		m.changeGen++
		_ = m.host.Respond(msg.Plugin, msg.ID, nil, nil)
		return m, tea.Batch(m.editor.Init(), m.host.Recv())

	case "openFile":
		var p struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			_ = m.host.Respond(msg.Plugin, msg.ID, nil, &plugin.RPCError{
				Code:    plugin.ErrInvalidParams,
				Message: "invalid params: " + err.Error(),
			})
			return m, m.host.Recv()
		}
		_, cmd := m.openFile(p.Path)
		_ = m.host.Respond(msg.Plugin, msg.ID, nil, nil)
		if cmd != nil {
			return m, tea.Batch(cmd, m.host.Recv())
		}
		return m, m.host.Recv()

	default:
		_ = m.host.Respond(msg.Plugin, msg.ID, nil, &plugin.RPCError{
			Code:    plugin.ErrMethodNotFound,
			Message: "unknown method: " + msg.Method,
		})
		return m, m.host.Recv()
	}
}

// upsertStatusSegment updates an existing segment (matched by plugin+segment
// name) or appends a new one. Passing an empty Text removes the segment.
func (m *Model) upsertStatusSegment(pluginName, segment, text, color string) {
	for i, s := range m.statusSegments {
		if s.Plugin != pluginName || s.Name != segment {
			continue
		}
		if text == "" {
			m.statusSegments = append(m.statusSegments[:i], m.statusSegments[i+1:]...)
			return
		}
		m.statusSegments[i].Text = text
		m.statusSegments[i].Color = color
		return
	}
	if text == "" {
		return
	}
	m.statusSegments = append(m.statusSegments, statusSegment{
		Plugin: pluginName,
		Name:   segment,
		Text:   text,
		Color:  color,
	})
}

// filterSegments returns s with every segment owned by pluginName removed.
func filterSegments(s []statusSegment, pluginName string) []statusSegment {
	out := s[:0]
	for _, seg := range s {
		if seg.Plugin == pluginName {
			continue
		}
		out = append(out, seg)
	}
	return out
}

// handleKey implements app-level key routing. Global bindings
// (Tab/Shift+Tab/Ctrl+S/Ctrl+Q/Ctrl+P) are intercepted here before any
// forwarding; everything else is routed to the focused child.
//
// When the command palette is open it consumes every key event before the
// globals run, so Ctrl+S / Ctrl+Q are safe from being triggered by a user
// typing into the palette's query field.
func (m *Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.palette.open {
		return m.handlePaletteKey(msg)
	}
	switch msg.String() {
	case keyCtrlP:
		m.palette.Open(m.commands)
		return m, nil
	case keyTab, keyShiftTab:
		prev := m.focus
		if m.focus == focusTree {
			m.focus = focusEditor
		} else {
			m.focus = focusTree
		}
		if m.leftPane != nil && prev != m.focus {
			m.leftPane.OnFocus(m.focus == focusTree)
		}
		return m, nil

	case keyCtrlS:
		// App-wide manual save, works regardless of focus and mode.
		if err := m.flushNow(); err != nil {
			m.statusErr = err.Error()
			return m, m.editor.SetStatusMessage("save failed: " + err.Error())
		}
		return m, m.editor.SetStatusMessage("saved")

	case keyCtrlQ:
		return m.guardedQuit()
	}

	if m.focus == focusTree {
		// Tree-specific keys. Navigation (j/k, g/G, pgup/pgdn, etc.) plus
		// Enter / h / l / arrows are handled by the left pane plugin. Only
		// Ctrl+C needs app-level interception.
		if msg.String() == keyCtrlC {
			return m.guardedQuit()
		}
	}

	return m.forwardToFocused(msg)
}

// handlePaletteKey routes a key event to the palette overlay and, on Enter,
// executes the selected command through the plugin host. Always returns
// immediately — while the palette is open, the editor and left pane do not
// see keyboard input.
func (m *Model) handlePaletteKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	closed, activated := m.palette.HandleKey(msg.String(), msg.Runes)
	if activated != nil {
		result := m.host.ExecuteCommand(activated.ID, nil)
		var cmd tea.Cmd
		if result.Sync != nil {
			if result.Sync.Err != nil {
				m.statusErr = result.Sync.Err.Error()
			}
			cmd = m.applyEffects(result.Sync.Effects)
		}
		if result.Async != nil {
			if cmd == nil {
				cmd = result.Async
			} else {
				cmd = tea.Batch(cmd, result.Async)
			}
		}
		m.palette.Close()
		return m, cmd
	}
	if closed {
		m.palette.Close()
	}
	return m, nil
}

// guardedQuit flushes any dirty buffer before quitting. If the flush fails we
// refuse to quit and surface the error through the status bar so the user can
// take action (e.g. fix disk space, permissions) instead of silently losing
// edits. After a successful flush we let the host shut down its plugins (with
// a bounded timeout) before returning tea.Quit.
func (m *Model) guardedQuit() (tea.Model, tea.Cmd) {
	if m.dirty && m.openPath != "" {
		if err := m.flushNow(); err != nil {
			m.statusErr = err.Error()
			return m, m.editor.SetStatusMessage("quit aborted: save failed: " + err.Error())
		}
	}
	// Host.Shutdown logs its own errors; we don't surface them here because
	// we're on the quit path.
	_ = m.host.Shutdown(3 * time.Second)
	return m, tea.Quit
}

// forwardToFocused dispatches msg to whichever child is currently focused.
// After forwarding to the editor, we diff the buffer against lastText to
// decide whether to schedule a debounced autosave + change notification.
func (m *Model) forwardToFocused(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.focus == focusTree {
		if m.leftPane == nil {
			return m, nil
		}
		key, ok := msg.(tea.KeyMsg)
		if !ok {
			// No legacy tree.Update — non-key messages are dropped when the
			// tree has focus. Panes receive non-key state exclusively via
			// OnResize / OnFocus.
			return m, nil
		}
		effs := m.leftPane.HandleKey(keyEventFromTea(key))
		return m, m.applyEffects(effs)
	}

	newEd, cmd := m.editor.Update(msg)
	if e, ok := newEd.(vimtea.Editor); ok {
		m.editor = e
	}

	// Autosave + change-tick check — only meaningful when we have a file
	// open.
	if m.openPath != "" {
		cur := m.editor.GetBuffer().Text()
		if cur != m.lastText {
			m.dirty = true
			m.saveGen++
			m.changeGen++
			tick := scheduleSave(m.saveGen)
			changeTick := scheduleChange(m.changeGen)
			if cmd == nil {
				return m, tea.Batch(tick, changeTick)
			}
			return m, tea.Batch(cmd, tick, changeTick)
		}
	}
	return m, cmd
}

// keyEventFromTea converts a bubbletea KeyMsg into a transport-neutral
// plugin.KeyEvent that panes can consume without importing bubbletea.
func keyEventFromTea(msg tea.KeyMsg) plugin.KeyEvent {
	return plugin.KeyEvent{
		Key:   msg.String(),
		Runes: msg.Runes,
		Alt:   msg.Alt,
	}
}

// applyEffects folds a plugin.Effect slice into the model and returns the
// tea.Cmd that carries any side-effects (status-bar messages, pending file
// opens). Focus changes and pane-invalidate are pure state updates and
// produce no Cmd.
func (m *Model) applyEffects(effs []plugin.Effect) tea.Cmd {
	if len(effs) == 0 {
		return nil
	}
	var cmds []tea.Cmd
	for _, e := range effs {
		switch ef := e.(type) {
		case plugin.OpenFile:
			path := ef.Path
			cmds = append(cmds, func() tea.Msg {
				return openFileRequestMsg{path: path}
			})
		case plugin.Notify:
			if ef.Message != "" {
				cmds = append(cmds, m.editor.SetStatusMessage(ef.Message))
			}
		case plugin.Focus:
			prev := m.focus
			switch ef.Pane {
			case "left":
				m.focus = focusTree
			case "editor", "right":
				m.focus = focusEditor
			}
			if m.leftPane != nil && prev != m.focus {
				m.leftPane.OnFocus(m.focus == focusTree)
			}
		case plugin.Invalidate:
			// No-op: Bubble Tea repaints every tick.
		}
	}
	if len(cmds) == 0 {
		return nil
	}
	if len(cmds) == 1 {
		return cmds[0]
	}
	return tea.Batch(cmds...)
}

// openFile handles opening path: read the file, guard against binary/oversize
// content, reconstruct the editor (fresh instance re-registers Ctrl bindings
// via newEditor → addCtrlBindings), seed lastText so the newly-loaded content
// does not immediately register as dirty, move focus to the editor, and emit
// DidClose (for the previous file) + DidOpen to the plugin host.
func (m *Model) openFile(path string) (tea.Model, tea.Cmd) {
	info, err := os.Stat(path)
	if err != nil {
		return m, m.editor.SetStatusMessage("open failed: " + err.Error())
	}
	if info.IsDir() {
		return m, nil
	}
	if info.Size() > maxFileSize {
		return m, m.editor.SetStatusMessage(fmt.Sprintf("refusing: file > %d bytes", maxFileSize))
	}

	b, err := os.ReadFile(path)
	if err != nil {
		return m, m.editor.SetStatusMessage("open failed: " + err.Error())
	}

	// Binary detection: any NUL byte in the (already size-capped) payload is
	// a reliable signal. Scanning the whole buffer is cheap at ≤1 MiB.
	if bytes.IndexByte(b, 0x00) >= 0 {
		return m, m.editor.SetStatusMessage("refusing: binary file")
	}

	// Before we swap editors, flush any unsaved edits in the currently-open
	// file. If that flush fails, abort the open rather than silently dropping
	// the user's work.
	if m.dirty && m.openPath != "" {
		if err := m.flushNow(); err != nil {
			m.statusErr = err.Error()
			return m, m.editor.SetStatusMessage("open aborted: flush of prior file failed: " + err.Error())
		}
	}

	prevPath := m.openPath

	content := string(b)
	m.editor = newEditor(content, path)

	// Push the current size into the new editor instance.
	contentH := m.height - statusBarLines
	contentH = max(contentH, 1)
	if newEd, _ := m.editor.SetSize(m.editorWidth(), contentH); newEd != nil {
		if e, ok := newEd.(vimtea.Editor); ok {
			m.editor = e
		}
	}

	m.openPath = path
	m.lastText = content
	m.dirty = false
	m.saveGen++   // invalidate any pending save tick from before the open
	m.changeGen++ // invalidate any pending change tick from before the open
	prevFocus := m.focus
	m.focus = focusEditor
	m.statusErr = ""

	// Plugin host notifications. Previous file gets a DidClose; new file gets
	// DidOpen with language hint.
	if prevPath != "" {
		_ = m.host.Emit(plugin.DidClose{Path: prevPath})
	}
	openEffs := m.host.Emit(plugin.DidOpen{
		Path: path,
		Lang: langFromPath(path),
		Text: content,
	})

	if m.leftPane != nil && prevFocus != m.focus {
		m.leftPane.OnFocus(m.focus == focusTree)
	}

	cmds := []tea.Cmd{m.editor.Init()}
	if effCmd := m.applyEffects(openEffs); effCmd != nil {
		cmds = append(cmds, effCmd)
	}
	return m, tea.Batch(cmds...)
}

// langFromPath returns a lowercased file-extension language hint, without the
// leading dot. Empty string when the file has no extension.
func langFromPath(p string) string {
	ext := filepath.Ext(p)
	if ext == "" {
		return ""
	}
	return strings.ToLower(strings.TrimPrefix(ext, "."))
}

// flushNow writes the current buffer to disk atomically and updates the save
// bookkeeping. It also bumps saveGen so any in-flight debounced tick is
// invalidated. Safe to call even when there is nothing to save. On success,
// emits DidSave to the plugin host.
func (m *Model) flushNow() error {
	if m.openPath == "" || !m.dirty {
		return nil
	}
	cur := m.editor.GetBuffer().Text()
	if err := atomicWriteFile(m.openPath, []byte(cur)); err != nil {
		return err
	}
	m.lastText = cur
	m.lastSavedAt = nowFunc()
	m.dirty = false
	m.saveGen++
	savedPath := m.openPath
	_ = m.host.Emit(plugin.DidSave{Path: savedPath})
	return nil
}

func (m *Model) View() string {
	if m.width == 0 || m.height == 0 {
		// Pre-init render — the real view waits for the first WindowSizeMsg.
		return ""
	}

	// When the palette is open, replace the whole view with the overlay.
	// This is simpler than true compositing, and acceptable because the
	// palette blocks input anyway.
	if m.palette.open {
		return m.palette.View(m.width, m.height, m.commands)
	}

	contentH := m.height - statusBarLines
	contentH = max(contentH, 1)
	editorW := m.editorWidth()

	editorStyle := lipgloss.NewStyle().
		Width(editorW).
		Height(contentH)

	var content string
	if m.leftPane != nil {
		// Notify pane of current dims if they changed since last Render.
		if treeWidth != m.lastPaneW || contentH != m.lastPaneH {
			m.leftPane.OnResize(treeWidth, contentH)
			m.lastPaneW = treeWidth
			m.lastPaneH = contentH
		}
		treeStyle := lipgloss.NewStyle().
			Width(treeWidth).
			Height(contentH).
			BorderStyle(lipgloss.NormalBorder()).
			BorderRight(true)
		if m.focus == focusTree {
			treeStyle = treeStyle.BorderForeground(lipgloss.Color("205"))
		}
		treePane := treeStyle.Render(m.leftPane.Render(treeWidth, contentH))
		editorPane := editorStyle.Render(m.editor.View())
		content = lipgloss.JoinHorizontal(lipgloss.Top, treePane, editorPane)
	} else {
		content = editorStyle.Render(m.editor.View())
	}

	status := m.renderStatusBar()
	return lipgloss.JoinVertical(lipgloss.Left, content, status)
}

// renderStatusBar composes mode indicator (left), open-path + plugin
// segments (middle), save indicator (right) inside a single line of total
// width m.width. Plugin segments are dropped right-to-left if space is
// insufficient after truncating the path.
func (m *Model) renderStatusBar() string {
	modeText := "[" + modeName(m.editor.GetMode()) + "]"

	path := m.openPath
	if path == "" {
		path = "(no file)"
	} else if abs, err := filepath.Abs(m.root); err == nil {
		if rel, err := filepath.Rel(abs, path); err == nil && !strings.HasPrefix(rel, "..") {
			path = rel
		}
	}

	var saveIndicator string
	switch {
	case m.statusErr != "":
		saveIndicator = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Render("! " + m.statusErr)
	case m.dirty:
		saveIndicator = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Render("● unsaved")
	case !m.lastSavedAt.IsZero() && time.Since(m.lastSavedAt) < savedFadeAfter:
		saveIndicator = lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Render("✓ saved")
	default:
		saveIndicator = ""
	}

	left := lipgloss.NewStyle().Bold(true).Render(modeText)
	right := saveIndicator

	// Render plugin status segments, joined by a two-space separator. We
	// render each in its own style so colors apply independently.
	var renderedSegments []string
	for _, seg := range m.statusSegments {
		if seg.Text == "" {
			continue
		}
		st := lipgloss.NewStyle()
		if seg.Color != "" {
			st = st.Foreground(lipgloss.Color(seg.Color))
		}
		renderedSegments = append(renderedSegments, st.Render(seg.Text))
	}

	leftW := lipgloss.Width(left)
	rightW := lipgloss.Width(right)
	// Account for 2 separator spaces (left–middle, middle–right).
	middleBudget := m.width - leftW - rightW - 2
	middleBudget = max(middleBudget, 0)

	// Build the middle: path + plugin segments. Drop segments from the right
	// until the combined width fits the budget; then, if still over, truncate
	// the path from the front.
	pathW := lipgloss.Width(path)
	segSep := "  "
	totalSegW := 0
	for i, s := range renderedSegments {
		if i > 0 {
			totalSegW += lipgloss.Width(segSep)
		}
		totalSegW += lipgloss.Width(s)
	}

	for len(renderedSegments) > 0 && pathW+lipgloss.Width(segSep)+totalSegW > middleBudget {
		// Drop the rightmost segment and recompute totalSegW.
		dropped := renderedSegments[len(renderedSegments)-1]
		renderedSegments = renderedSegments[:len(renderedSegments)-1]
		totalSegW -= lipgloss.Width(dropped)
		if len(renderedSegments) > 0 {
			totalSegW -= lipgloss.Width(segSep)
		}
	}

	// Truncate path with leading ellipsis if it alone still overflows.
	segW := totalSegW
	if len(renderedSegments) > 0 {
		segW += lipgloss.Width(segSep)
	}
	if pathW+segW > middleBudget {
		avail := middleBudget - segW
		if avail > 1 {
			runes := []rune(path)
			if len(runes) > avail-1 {
				path = "…" + string(runes[len(runes)-(avail-1):])
			}
		} else {
			path = ""
		}
	}

	middle := path
	if len(renderedSegments) > 0 {
		middle = path + segSep + strings.Join(renderedSegments, segSep)
	}
	middleStyle := lipgloss.NewStyle().Width(middleBudget).Align(lipgloss.Center)
	middleRendered := middleStyle.Render(middle)

	row := lipgloss.JoinHorizontal(lipgloss.Top, left, " ", middleRendered, " ", right)
	return lipgloss.NewStyle().Width(m.width).Render(row)
}

func modeName(m vimtea.EditorMode) string {
	switch m {
	case vimtea.ModeNormal:
		return "NORMAL"
	case vimtea.ModeInsert:
		return "INSERT"
	case vimtea.ModeVisual:
		return "VISUAL"
	case vimtea.ModeCommand:
		return "COMMAND"
	default:
		return "?"
	}
}
