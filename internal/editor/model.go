package editor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/savimcio/nistru/internal/config"
	"github.com/savimcio/nistru/internal/plugins/autoupdate"
	"github.com/savimcio/nistru/internal/plugins/settingscmd"
	"github.com/savimcio/nistru/internal/plugins/treepane"
	"github.com/savimcio/nistru/plugin"
)

// keyCtrlC is the tree-pane's universal quit intercept. It stays hardcoded
// regardless of the user's keymap because Ctrl+C is the terminal's universal
// interrupt contract — rebinding it would break muscle memory. Every other
// app-level binding flows through m.cfg.Keymap.
const keyCtrlC = "ctrl+c"

// Layout constants. Only statusBarLines is truly fixed; the other knobs
// (treeWidth, maxFileSize, savedFadeAfter) now live on the Model via
// m.cfg (UI.TreeWidth, Editor.MaxFileSize, UI.SavedFadeAfter).
const (
	statusBarLines = 1
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
// left-pane plugin), the Editor (goeditor-backed via goeditorAdapter), focus
// routing, window size, and the autosave/change-debounce bookkeeping.
type Model struct {
	root string
	cfg  *config.Config

	host     *plugin.Host
	registry *plugin.Registry
	leftPane plugin.Pane

	// commands is a cached snapshot of the host's command registry. Refreshed
	// whenever a plugin registers/unregisters a command.
	commands map[string]plugin.CommandRef

	// statusSegments are plugin-contributed status bar fragments, keyed by
	// plugin name for in-place updates.
	statusSegments []statusSegment

	editor Editor
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
// empty; opening a file through the tree replaces it. A nil cfg is equivalent
// to config.Defaults() — tests that don't care about configuration pass nil
// and get the built-in defaults without having to thread a loader through.
//
// The settingscmd plugin needs a live handle to the Model's config so its
// showResolved / reload commands reflect the post-reload state. We solve the
// circular dependency with a pointer-to-pointer: mref is nil until
// newModelWithRegistry finishes building m, then gets its address. settingscmd
// only calls getCfg lazily (on palette command invocation), never during
// registration, so "nil mref means cfg is not ready yet" never fires in
// practice — but the fallback keeps the closure total.
func NewModel(root string, cfg *config.Config) (*Model, error) {
	registry := plugin.NewRegistry()

	tp, err := treepane.New(root)
	if err != nil {
		return nil, fmt.Errorf("build tree plugin: %w", err)
	}
	registry.RegisterInProc(tp)

	registry.RegisterInProc(autoupdate.New())

	var mref *Model
	getCfg := func() *config.Config {
		if mref == nil {
			return cfg
		}
		return mref.cfg
	}
	registry.RegisterInProc(settingscmd.New(root, getCfg))

	m, err := newModelWithRegistry(root, registry, cfg)
	if err != nil {
		return nil, err
	}
	mref = m
	return m, nil
}

// newModelWithRegistry is the shared constructor used by NewModel and by
// tests that need to inject a pre-populated registry (e.g. a replacement
// autoupdate plugin with test seams). It starts the host, fires the
// onStart-matching Initialize event (so in-proc plugins register their
// palette commands, start background workers, etc.), then snapshots commands
// and returns a Model ready for Init().
//
// Emit(Initialize) runs OnEvent synchronously for in-proc plugins on the
// calling goroutine; side effects (commands/register, statusBar/set) flow
// through PostNotif whose host-side bookkeeping runs before the inbound
// channel send, so the subsequent host.Commands() snapshot already includes
// anything a plugin registered during Initialize.
func newModelWithRegistry(root string, registry *plugin.Registry, cfg *config.Config) (*Model, error) {
	if cfg == nil {
		cfg = config.Defaults()
	}
	host := plugin.NewHost(registry)
	// Install the per-plugin config lookup BEFORE Start/Emit so Initialize
	// carries each plugin's sub-tree in the same dispatch.
	host.SetPluginConfig(cfg.PluginConfig)
	if err := host.Start(root); err != nil {
		return nil, fmt.Errorf("start plugin host: %w", err)
	}

	host.Emit(plugin.Initialize{RootPath: root, Capabilities: nil})

	m := &Model{
		root:     root,
		cfg:      cfg,
		host:     host,
		registry: registry,
		leftPane: host.Pane("left"),
		commands: host.Commands(),
		editor:   newEditor("", "", &editorOpts{Keymap: cfg.Keymap, RelativeNumbers: cfg.UI.RelativeNumbers}),
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
			tw := m.treeWidth()
			if tw != m.lastPaneW || contentH != m.lastPaneH {
				m.leftPane.OnResize(tw, contentH)
				m.lastPaneW = tw
				m.lastPaneH = contentH
			}
		}
		cmd := m.editor.SetSize(editorW, contentH)
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
			Text: m.editor.Content(),
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

	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}

	// Default: forward to the focused child.
	return m.forwardToFocused(msg)
}

// editorWidth returns the editor's width in cells, allowing for a missing
// left pane (in which case the editor fills the screen). The tree pane's
// width comes from the user's config via m.cfg.UI.TreeWidth; falling back to
// the default keeps partially-initialised test Models (m.cfg == nil) safe.
func (m *Model) editorWidth() int {
	w := m.width
	if m.leftPane != nil {
		w = m.width - m.treeWidth()
	}
	return max(w, 1)
}

// treeWidth returns the configured left-pane width, defaulting to the
// built-in constant when m.cfg is nil (i.e. in hand-constructed test
// Models).
func (m *Model) treeWidth() int {
	if m.cfg == nil {
		return config.Defaults().UI.TreeWidth
	}
	return m.cfg.UI.TreeWidth
}

// keymap returns the active Keymap, falling back to the built-in defaults
// when m.cfg is nil. Hand-built test Models routinely skip wiring m.cfg, so
// the defensive return keeps every keypath safe without burdening each call
// site with a nil check.
func (m *Model) keymap() config.Keymap {
	if m.cfg == nil {
		return config.DefaultKeymap()
	}
	return m.cfg.Keymap
}

// savedFadeAfter returns the UI.SavedFadeAfter duration, falling back to
// the built-in default when m.cfg is nil.
func (m *Model) savedFadeAfter() time.Duration {
	if m.cfg == nil {
		return config.Defaults().UI.SavedFadeAfter
	}
	return m.cfg.UI.SavedFadeAfter
}

// maxFileSize returns the open-file size ceiling, falling back to the
// built-in default when m.cfg is nil.
func (m *Model) maxFileSize() uint64 {
	if m.cfg == nil {
		return uint64(config.Defaults().Editor.MaxFileSize)
	}
	return uint64(m.cfg.Editor.MaxFileSize)
}

// editorOptsFromCfg returns the editorOpts derived from m.cfg, suitable for
// passing to newEditor. Returns nil when m.cfg is nil so newEditor falls
// back to its own defaults.
func (m *Model) editorOptsFromCfg() *editorOpts {
	if m.cfg == nil {
		return nil
	}
	return &editorOpts{Keymap: m.cfg.Keymap, RelativeNumbers: m.cfg.UI.RelativeNumbers}
}

// saveDebounce returns the autosave debounce window, falling back to the
// built-in default when m.cfg is nil.
func (m *Model) saveDebounce() time.Duration {
	if m.cfg == nil {
		return config.Defaults().Autosave.SaveDebounce
	}
	return m.cfg.Autosave.SaveDebounce
}

// changeDebounce returns the DidChange debounce window, falling back to the
// built-in default when m.cfg is nil.
func (m *Model) changeDebounce() time.Duration {
	if m.cfg == nil {
		return config.Defaults().Autosave.ChangeDebounce
	}
	return m.cfg.Autosave.ChangeDebounce
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
		// Swap the buffer in place on the existing editor — same lifecycle
		// reasoning as openFile (F2.1): constructing a new adapter leaks the
		// previous one's in-flight cmds past the replacement, and those
		// cmds cross-contaminate the new editor via the non-key forwarding
		// path in Update.
		m.editor.SetContent(p.Text)
		_ = m.editor.SetMode(ModeNormal)
		contentH := m.height - statusBarLines
		contentH = max(contentH, 1)
		_ = m.editor.SetSize(m.editorWidth(), contentH)
		// Seed lastText from the editor's own Content() — goeditor drops any
		// trailing newline at parse time (F.3 removed the re-append heuristic),
		// so p.Text "formatted\n" round-trips to "formatted". Using p.Text
		// verbatim would leave m.lastText one byte off from m.editor.Content(),
		// and the next non-edit keystroke's dirty-diff in forwardToEditor
		// would fire a false positive (autosave, DidChange, dirty flag).
		// Matching openFile's seeding fixes that.
		m.lastText = m.editor.Content()
		m.dirty = false
		m.saveGen++
		m.changeGen++
		_ = m.host.Respond(msg.Plugin, msg.ID, nil, nil)
		return m, m.host.Recv()

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
func (m *Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.palette.open {
		return m.handlePaletteKey(msg)
	}
	key := msg.String()
	km := m.keymap()
	switch key {
	case km.Lookup(config.ActionPalette):
		m.palette.Open(m.commands)
		return m, nil
	case km.Lookup(config.ActionFocusNext), km.Lookup(config.ActionFocusPrev):
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

	case km.Lookup(config.ActionSave):
		// App-wide manual save, works regardless of focus and mode.
		if err := m.flushNow(); err != nil {
			m.statusErr = err.Error()
			return m, m.editor.SetStatusMessage("save failed: " + err.Error())
		}
		return m, m.editor.SetStatusMessage("saved")

	case km.Lookup(config.ActionQuit):
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
func (m *Model) handlePaletteKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	closed, activated := m.palette.HandleKey(msg.String(), []rune(msg.Text))
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
//
// Key messages are focus-gated (tree vs editor). Non-key messages, however,
// are ALWAYS forwarded to the editor regardless of focus: goeditor uses
// non-key messages (tea.Cmd ticks) to drive transient UI such as the timer
// that clears DispatchMessage text. If we dropped those while the tree had
// focus, status messages emitted via SetStatusMessage would stick on screen
// forever because their clear event never arrives. Panes do not consume
// non-key messages — they get state via OnResize / OnFocus — so the editor
// is the correct sink for everything non-key.
func (m *Model) forwardToFocused(msg tea.Msg) (tea.Model, tea.Cmd) {
	if _, isKey := msg.(tea.KeyPressMsg); !isKey {
		var cmd tea.Cmd
		m.editor, cmd = m.editor.Update(msg)
		return m, cmd
	}

	if m.focus == focusTree {
		if m.leftPane == nil {
			return m, nil
		}
		// Safe to type-assert: we just checked msg is a KeyPressMsg above.
		key := msg.(tea.KeyPressMsg)
		effs := m.leftPane.HandleKey(keyEventFromTea(key))
		return m, m.applyEffects(effs)
	}

	var cmd tea.Cmd
	m.editor, cmd = m.editor.Update(msg)

	// Autosave + change-tick check — only meaningful when we have a file
	// open.
	if m.openPath != "" {
		cur := m.editor.Content()
		if cur != m.lastText {
			m.dirty = true
			m.saveGen++
			m.changeGen++
			tick := scheduleSave(m.saveGen, m.saveDebounce())
			changeTick := scheduleChange(m.changeGen, m.changeDebounce())
			if cmd == nil {
				return m, tea.Batch(tick, changeTick)
			}
			return m, tea.Batch(cmd, tick, changeTick)
		}
	}
	return m, cmd
}

// keyEventFromTea converts a bubbletea v2 KeyPressMsg into a transport-neutral
// plugin.KeyEvent that panes can consume without importing bubbletea. In v2
// the old msg.Runes / msg.Alt fields are replaced by msg.Text (string) and
// msg.Mod (KeyMod) respectively.
func keyEventFromTea(msg tea.KeyPressMsg) plugin.KeyEvent {
	return plugin.KeyEvent{
		Key:   msg.String(),
		Runes: []rune(msg.Text),
		Alt:   msg.Mod.Contains(tea.ModAlt),
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
		case plugin.ReloadConfigRequest:
			// The reload lives in a method so the surrounding switch stays a
			// routing table — the flow is too big to inline.
			cmds = append(cmds, m.reloadConfig())
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

// reloadConfig reparses the layered TOML config from disk, swaps the
// Model's cfg in place so every closure holding m.cfg sees the update,
// pushes the new plugin lookup into the host, and re-emits Initialize to
// already-activated plugins so they can pick up fresh per-plugin config.
// Warnings are printed to stderr in the same style as cmd/nistru/main.go
// — we duplicate the loop here rather than extract a helper, because the
// one-line format is trivial and both call sites stay independent.
//
// The Editor bakes in its opts at construction time (relative numbers and,
// in future, the Ctrl-binding keymap), so for those specific knobs we detect
// a change and rebuild the editor instance in place. Content and file path
// are preserved; cursor position and vim mode are reset — acceptable v1
// tradeoff, called out in the return contract.
func (m *Model) reloadConfig() tea.Cmd {
	newCfg, warnings, err := config.Load(m.root)
	if err != nil {
		return m.editor.SetStatusMessage("reload failed: " + err.Error())
	}
	for _, w := range warnings {
		fmt.Fprintf(os.Stderr, "nistru: config: %s: %s\n", w.Source, w.Message)
	}
	// Snapshot the editor-relevant knobs BEFORE the in-place mutation so the
	// post-swap comparison has something to diff against. Copying the Keymap
	// (a map) is necessary because *m.cfg = *newCfg shares the underlying
	// map header — without a snapshot, oldKeymap would already reflect the
	// new bindings by the time we compared.
	var oldKnobs editorKnobs
	if m.cfg != nil {
		oldKnobs = snapshotEditorKnobs(m.cfg)
	}

	// In-place mutation preserves m.cfg's address so any closure captured
	// during construction (e.g. settingscmd's getCfg) automatically sees
	// the fresh values without needing a refresh hook.
	if m.cfg != nil && newCfg != nil {
		*m.cfg = *newCfg
	} else {
		m.cfg = newCfg
	}

	var initCmd tea.Cmd
	if m.cfg != nil && editorKnobsChanged(oldKnobs, snapshotEditorKnobs(m.cfg)) {
		buf := m.editor.Content()
		m.editor = newEditor(buf, m.openPath, m.editorOptsFromCfg())
		contentH := m.height - statusBarLines
		contentH = max(contentH, 1)
		_ = m.editor.SetSize(m.editorWidth(), contentH)
		initCmd = m.editor.Init()
	}

	m.host.SetPluginConfig(m.cfg.PluginConfig)
	m.host.ReEmitInitialize()

	msg := m.editor.SetStatusMessage("settings reloaded")
	if initCmd != nil {
		return tea.Batch(initCmd, msg)
	}
	return msg
}

// editorKnobs is the subset of config.Config that the Editor bakes in at
// construction time — changing any of these means we must rebuild the
// editor instance for the change to take effect.
type editorKnobs struct {
	relativeNumbers bool
	undoKey         string
	redoKey         string
	cutKey          string
	copyKey         string
	pasteKey        string
}

// snapshotEditorKnobs copies the editor-relevant fields out of cfg into a
// plain struct so they survive an in-place overwrite of *m.cfg. cfg must be
// non-nil.
func snapshotEditorKnobs(cfg *config.Config) editorKnobs {
	km := cfg.Keymap
	if km == nil {
		km = config.DefaultKeymap()
	}
	return editorKnobs{
		relativeNumbers: cfg.UI.RelativeNumbers,
		undoKey:         km.Lookup(config.ActionUndo),
		redoKey:         km.Lookup(config.ActionRedo),
		cutKey:          km.Lookup(config.ActionCutLine),
		copyKey:         km.Lookup(config.ActionCopyLine),
		pasteKey:        km.Lookup(config.ActionPaste),
	}
}

// editorKnobsChanged reports whether any editor-relevant knob differs between
// two snapshots. A plain struct equality check would suffice today, but the
// named helper documents intent at the call site.
func editorKnobsChanged(a, b editorKnobs) bool {
	return a != b
}

// openFile handles opening path: read the file, guard against binary/oversize
// content, swap the editor's buffer in place via SetContent, seed lastText so
// the newly-loaded content does not immediately register as dirty, move focus
// to the editor, and emit DidClose (for the previous file) + DidOpen to the
// plugin host.
//
// The editor instance itself is reused across file opens. goeditor's Init
// spawns a blocking channel listener, and its internal vim interpreter emits
// timer cmds (cursor blink, DispatchMessage TTL). Constructing a fresh editor
// on every open — as this function used to — leaked those in-flight cmds from
// the previous adapter; when they resolved, the resulting tea.Msg reached the
// Model and (per the non-key forwarding in Update) landed on the NEW editor,
// producing stale state bleed across files. SetContent inside goeditor drops
// the old buffer (history, cursor, viewport) while keeping the channel alive,
// which is exactly the lifecycle we want.
//
// We also force ModeNormal post-swap because the previous implementation got
// that for free from "new editor starts in Normal" — without the explicit
// reset, opening a file while the prior buffer was mid-Insert would leave the
// new file in Insert mode, which would be a UX regression.
func (m *Model) openFile(path string) (tea.Model, tea.Cmd) {
	info, err := os.Stat(path)
	if err != nil {
		return m, m.editor.SetStatusMessage("open failed: " + err.Error())
	}
	if info.IsDir() {
		return m, nil
	}
	maxSz := m.maxFileSize()
	if uint64(info.Size()) > maxSz {
		return m, m.editor.SetStatusMessage(fmt.Sprintf("refusing: file > %d bytes", maxSz))
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

	// Before we replace the buffer, flush any unsaved edits in the currently-
	// open file. If that flush fails, abort the open rather than silently
	// dropping the user's work.
	if m.dirty && m.openPath != "" {
		if err := m.flushNow(); err != nil {
			m.statusErr = err.Error()
			return m, m.editor.SetStatusMessage("open aborted: flush of prior file failed: " + err.Error())
		}
	}

	prevPath := m.openPath

	content := string(b)
	// Swap the buffer in place. goeditor's SetContent → SetBuffer resets the
	// per-file state (history, cursor, viewport) without touching the channel
	// Init listens on, so the lifecycle stays single-instance for the Model's
	// lifetime. See the function doc for why we don't reconstruct.
	m.editor.SetContent(content)
	// Force Normal mode so opening a file mid-Insert doesn't land the user in
	// Insert on the new buffer — previously this came free from constructing
	// a fresh editor.
	_ = m.editor.SetMode(ModeNormal)

	// Re-push the current size so goeditor's viewport picks up the new buffer
	// at the correct dimensions (SetBuffer calls ScrollViewport but does not
	// know about the outer pane's width/height).
	contentH := m.height - statusBarLines
	contentH = max(contentH, 1)
	_ = m.editor.SetSize(m.editorWidth(), contentH)

	m.openPath = path
	// Seed lastText from the editor's actual buffer rather than the raw
	// file bytes — goeditor does not preserve trailing newlines, so the
	// raw content and the editor's Content() can differ by one byte on
	// open. Using the editor's view keeps the dirty-diff honest: a freshly
	// opened file must not register as modified until the user edits.
	m.lastText = m.editor.Content()
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
	// Publish the editor's actual buffer rather than the raw file bytes —
	// goeditor does not preserve trailing newlines, so a plugin that caches
	// DidOpen.Text (e.g. the bundled gofmt) would otherwise operate on stale
	// content until the first DidChange arrived. m.lastText was seeded above
	// from m.editor.Content(), which is the canonical view the user is
	// editing.
	openEffs := m.host.Emit(plugin.DidOpen{
		Path: path,
		Lang: langFromPath(path),
		Text: m.lastText,
	})

	if m.leftPane != nil && prevFocus != m.focus {
		m.leftPane.OnFocus(m.focus == focusTree)
	}

	// No editor.Init() here: the editor is reused, and Init was already called
	// at Model construction. Re-calling would spawn a second listener goroutine
	// on the same channel — a subtle leak we now avoid.
	var cmds []tea.Cmd
	if effCmd := m.applyEffects(openEffs); effCmd != nil {
		cmds = append(cmds, effCmd)
	}
	if len(cmds) == 0 {
		return m, nil
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
	cur := m.editor.Content()
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

// View composes the final terminal frame. In bubbletea v2 View returns a
// tea.View struct whose Content holds the rendered body and whose declarative
// fields (AltScreen, MouseMode, etc.) replace the old Program-level options.
// We always request the alt-screen and cell-motion mouse events so the
// runtime matches the v1 behaviour that used to live in run.go.
func (m *Model) View() tea.View {
	return tea.View{
		Content:   m.renderFrame(),
		AltScreen: true,
		MouseMode: tea.MouseModeCellMotion,
	}
}

// renderFrame produces the string body of the view. Split out from View so
// tests and internal callers can inspect the raw string without having to
// reach through the tea.View wrapper. goeditor handles width natively, so
// there is no defensive clampPaneBox step here any more — the previous
// stopgap is removed as part of the migration.
func (m *Model) renderFrame() string {
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
		tw := m.treeWidth()
		// Notify pane of current dims if they changed since last Render.
		if tw != m.lastPaneW || contentH != m.lastPaneH {
			m.leftPane.OnResize(tw, contentH)
			m.lastPaneW = tw
			m.lastPaneH = contentH
		}
		treeStyle := lipgloss.NewStyle().
			Width(tw).
			Height(contentH).
			BorderStyle(lipgloss.NormalBorder()).
			BorderRight(true)
		if m.focus == focusTree {
			treeStyle = treeStyle.BorderForeground(lipgloss.Color("205"))
		}
		rawTree := m.leftPane.Render(tw, contentH)
		rawEditor := m.editor.View()
		treePane := treeStyle.Render(rawTree)
		editorPane := editorStyle.Render(rawEditor)
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
	modeText := "[" + modeName(m.editor.Mode()) + "]"

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
	case !m.lastSavedAt.IsZero() && time.Since(m.lastSavedAt) < m.savedFadeAfter():
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

func modeName(m Mode) string {
	switch m {
	case ModeNormal:
		return "NORMAL"
	case ModeInsert:
		return "INSERT"
	case ModeVisual:
		return "VISUAL"
	case ModeCommand:
		return "COMMAND"
	default:
		return "?"
	}
}
