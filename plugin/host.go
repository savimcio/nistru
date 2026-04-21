package plugin

// Host is the unified in-process + out-of-process plugin host used by the
// nistru editor. The caller is expected to be Bubble Tea's Update goroutine.
// All in-process plugin callbacks run synchronously on that goroutine; all
// out-of-process plugin traffic is funnelled through an aggregated inbound
// channel that the model drains via Recv().

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"sync"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// CommandRef names a registered command's owning plugin and display title.
type CommandRef struct {
	Plugin string
	Title  string
}

// CommandSyncResult carries the synchronous outcome of an in-process command
// invocation.
type CommandSyncResult struct {
	Result  json.RawMessage
	Effects []Effect
	Err     error
}

// CommandResult is the outcome of Host.ExecuteCommand. Exactly one of Sync
// (in-process plugin) or Async (out-of-process plugin) is non-nil.
type CommandResult struct {
	Sync  *CommandSyncResult
	Async tea.Cmd
}

// PluginMsg is the sealed sum of messages delivered by Host.Recv.
type PluginMsg interface {
	pluginMsg()
}

// PluginStartedMsg reports that an out-of-process plugin has completed its
// Initialize handshake and is ready to accept events.
type PluginStartedMsg struct{ Name string }

// PluginExitedMsg reports that an out-of-process plugin has terminated. Err
// is non-nil if the process exited abnormally; it may embed the tail of the
// plugin's stderr for diagnostics.
type PluginExitedMsg struct {
	Name string
	Err  error
}

// PluginNotifMsg surfaces a JSON-RPC notification sent by an out-of-process
// plugin to the host.
type PluginNotifMsg struct {
	Plugin string
	Method string
	Params json.RawMessage
}

// PluginReqMsg surfaces a JSON-RPC request sent by an out-of-process plugin
// to the host. The model must reply via Host.Respond using the same ID.
type PluginReqMsg struct {
	Plugin string
	ID     any
	Method string
	Params json.RawMessage
}

// PluginResponseMsg surfaces a JSON-RPC response to a request the host sent
// to an out-of-process plugin.
type PluginResponseMsg struct {
	Plugin string
	ID     any
	Result json.RawMessage
	Err    *RPCError
}

func (PluginStartedMsg) pluginMsg()  {}
func (PluginExitedMsg) pluginMsg()   {}
func (PluginNotifMsg) pluginMsg()    {}
func (PluginReqMsg) pluginMsg()      {}
func (PluginResponseMsg) pluginMsg() {}

// inboundCap is the buffer size of the aggregated inbound channel shared by
// all running out-of-process plugins.
const inboundCap = 256

// hostCapabilities advertises the extension points the host exposes to
// plugins during the Initialize handshake.
var hostCapabilities = []string{
	string(CapCommands),
	string(CapFormatter),
	string(CapStatusBar),
	string(CapPane),
}

// Host owns the registry, dispatches events, and manages out-of-process
// plugin lifecycles.
type Host struct {
	registry *Registry

	mu        sync.RWMutex
	rootPath  string
	started   bool
	running   map[string]*extPlugin // name -> running external plugin
	activated map[string]bool       // name -> has seen a matching activation
	unhealthy map[string]bool       // in-proc plugins that panicked
	inbound   chan PluginMsg

	// commands is *map[string]CommandRef, replaced via atomic.Value on each
	// register/unregister so Commands() is lock-free for readers.
	commands atomic.Value

	// nextReqID generates outbound JSON-RPC request IDs.
	nextReqID atomic.Int64
}

// NewHost returns a Host that dispatches to plugins from the given registry.
func NewHost(registry *Registry) *Host {
	h := &Host{
		registry:  registry,
		running:   make(map[string]*extPlugin),
		activated: make(map[string]bool),
		unhealthy: make(map[string]bool),
		inbound:   make(chan PluginMsg, inboundCap),
	}
	empty := make(map[string]CommandRef)
	h.commands.Store(&empty)
	return h
}

// Start records the workspace root and readies the host. Out-of-process
// plugins are not spawned here; they come online lazily on their first
// matching activation event.
//
// In-process plugins that satisfy HostAware receive a SetHost call before
// this method returns, giving them a handle they can use from background
// goroutines (via PostNotif) once Initialize is delivered.
func (h *Host) Start(rootPath string) error {
	h.mu.Lock()
	if h.started {
		h.mu.Unlock()
		return errors.New("plugin: host already started")
	}
	h.rootPath = rootPath
	h.started = true
	h.mu.Unlock()

	// Wire HostAware plugins outside h.mu: SetHost is user code and may do
	// arbitrary work; holding the host lock would invite deadlocks if an
	// implementation reaches back into the host during its own setup.
	for _, p := range h.registry.InProc() {
		if hp, ok := p.(HostAware); ok {
			hp.SetHost(h)
		}
	}
	return nil
}

// PostNotif enqueues a synthetic JSON-RPC notification on the host's
// inbound channel, as if it had arrived from an out-of-process plugin.
// Safe to call from any goroutine. Non-blocking: if the inbound channel
// is full, the notification is dropped and a warning is written to
// stderr. This is primarily for in-process plugins that need to report
// status from background work; out-of-process plugins already have a
// dedicated writer goroutine and should not call this method.
//
// Host-side bookkeeping (e.g. commands/register) is applied synchronously
// before the channel send, so callers see command registration as
// effective on return even if the inbound buffer is saturated.
func (h *Host) PostNotif(plugin, method string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("plugin: PostNotif marshal %s: %w", method, err)
	}
	msg := PluginNotifMsg{Plugin: plugin, Method: method, Params: raw}
	// Drive host-side bookkeeping (e.g. commands/register) synchronously
	// so callers see command registration as effective on return.
	// handleInternal is idempotent: registerCommand/unregisterCommand are
	// CAS loops against the commands atomic.Value.
	h.handleInternal(msg)
	select {
	case h.inbound <- msg:
		return nil
	default:
		fmt.Fprintf(os.Stderr, "plugin: PostNotif(%s:%s) dropped: inbound full\n", plugin, method)
		return errors.New("plugin: inbound channel full")
	}
}

// Emit delivers event to every plugin whose activation matches. For in-proc
// plugins, OnEvent runs synchronously on the caller's goroutine and the
// collected effects are returned. For out-of-proc plugins, the event is
// sent as a JSON-RPC notification via the plugin's writer goroutine; those
// plugins' effects arrive later as PluginNotifMsg via Recv.
func (h *Host) Emit(event any) []Effect {
	method, params, actEv, hasActivation := describeEvent(event)

	var effects []Effect

	// In-process dispatch.
	for _, p := range h.registry.InProc() {
		if !h.shouldDispatch(p.Name(), p.Activation(), actEv, hasActivation, event) {
			continue
		}
		h.markActivated(p.Name())
		ef := h.callOnEvent(p, event)
		effects = append(effects, ef...)
	}

	// Out-of-process dispatch.
	for _, m := range h.registry.Manifests() {
		// Skip if an in-proc plugin with the same name exists (in-proc wins).
		if h.isInProcName(m.Name) {
			continue
		}
		if !h.shouldDispatch(m.Name, m.Activation, actEv, hasActivation, event) {
			continue
		}
		h.markActivated(m.Name)

		ext, err := h.ensureSpawned(m)
		if err != nil {
			fmt.Fprintf(os.Stderr, "plugin: spawn %s: %v\n", m.Name, err)
			continue
		}
		if method == "" {
			// Event has no wire representation (unknown type). Skip silently.
			continue
		}
		ext.sendEvent(method, params, event)
	}

	return effects
}

// isInProcName reports whether the given name is an in-proc plugin. Avoids
// the manifest lookup path in Registry.ByName.
func (h *Host) isInProcName(name string) bool {
	for _, p := range h.registry.InProc() {
		if p.Name() == name {
			return true
		}
	}
	return false
}

// shouldDispatch returns true when event should be delivered to a plugin
// with the given activation patterns. Events without a natural activation
// (e.g. DidChange) are delivered only to already-activated plugins.
func (h *Host) shouldDispatch(name string, patterns []string, actEv ActivationEvent, hasActivation bool, event any) bool {
	h.mu.RLock()
	unhealthy := h.unhealthy[name]
	activated := h.activated[name]
	h.mu.RUnlock()
	if unhealthy {
		return false
	}
	if _, isShutdown := event.(Shutdown); isShutdown {
		return activated
	}
	if !hasActivation {
		// No activation computable — only deliver to already-active plugins.
		return activated
	}
	ok, err := Match(patterns, actEv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "plugin: %s: bad activation: %v\n", name, err)
		return false
	}
	if ok {
		return true
	}
	// Pattern did not match this specific event, but if the plugin is
	// already activated we still forward the event so it can keep its state
	// consistent (e.g. a formatter activated by onLanguage:go still wants
	// DidChange for the same buffer).
	return activated
}

func (h *Host) markActivated(name string) {
	h.mu.Lock()
	h.activated[name] = true
	h.mu.Unlock()
}

// callOnEvent invokes p.OnEvent with panic recovery. A panicking plugin is
// marked unhealthy and receives no further events this session.
func (h *Host) callOnEvent(p Plugin, event any) (effects []Effect) {
	defer func() {
		// Panic boundary: prevent a misbehaving in-proc plugin from taking
		// down the editor's Update goroutine.
		if r := recover(); r != nil {
			h.markUnhealthy(p.Name(), fmt.Errorf("panic in OnEvent: %v", r))
			effects = nil
		}
	}()
	return p.OnEvent(event)
}

func (h *Host) markUnhealthy(name string, err error) {
	fmt.Fprintf(os.Stderr, "plugin: %s crashed: %v\n", name, err)
	h.mu.Lock()
	h.unhealthy[name] = true
	h.mu.Unlock()
}

// DispatchKey routes a key event to the named in-process plugin's Pane.
// Returns the effects produced by HandleKey, or an empty slice if the
// plugin panicked (in which case the plugin is marked unhealthy).
func (h *Host) DispatchKey(pluginName string, k KeyEvent) []Effect {
	h.mu.RLock()
	unhealthy := h.unhealthy[pluginName]
	h.mu.RUnlock()
	if unhealthy {
		return nil
	}
	for _, p := range h.registry.InProc() {
		if p.Name() != pluginName {
			continue
		}
		pane, ok := p.(Pane)
		if !ok {
			return nil
		}
		return h.callHandleKey(p.Name(), pane, k)
	}
	return nil
}

func (h *Host) callHandleKey(name string, pane Pane, k KeyEvent) (effects []Effect) {
	defer func() {
		// Panic boundary: keep pane key handlers from crashing the host.
		if r := recover(); r != nil {
			h.markUnhealthy(name, fmt.Errorf("panic in HandleKey: %v", r))
			effects = nil
		}
	}()
	return pane.HandleKey(k)
}

// Pane returns the in-proc pane assigned to the given slot, or nil if no
// plugin owns it. Identical slot returned by multiple plugins picks the
// first registered.
func (h *Host) Pane(slot string) Pane {
	for _, p := range h.registry.InProc() {
		pane, ok := p.(Pane)
		if !ok {
			continue
		}
		if h.paneSlot(p.Name(), pane) == slot {
			return pane
		}
	}
	return nil
}

// PaneByName returns the in-proc pane for a named plugin, or nil.
func (h *Host) PaneByName(name string) Pane {
	for _, p := range h.registry.InProc() {
		if p.Name() != name {
			continue
		}
		pane, ok := p.(Pane)
		if !ok {
			return nil
		}
		return pane
	}
	return nil
}

// paneSlot reads Pane.Slot with panic recovery.
func (h *Host) paneSlot(name string, pane Pane) (slot string) {
	defer func() {
		// Panic boundary: Slot() is not expected to panic, but callers must
		// not be taken down if it does.
		if r := recover(); r != nil {
			h.markUnhealthy(name, fmt.Errorf("panic in Slot: %v", r))
			slot = ""
		}
	}()
	return pane.Slot()
}

// Commands returns a copy-on-write snapshot of the registered command map.
// Callers may hold the returned map indefinitely; the host replaces it
// atomically on register/unregister.
func (h *Host) Commands() map[string]CommandRef {
	if m := h.commands.Load(); m != nil {
		return *(m.(*map[string]CommandRef))
	}
	return map[string]CommandRef{}
}

func (h *Host) registerCommand(plugin, id, title string) {
	for {
		cur := h.commands.Load().(*map[string]CommandRef)
		next := make(map[string]CommandRef, len(*cur)+1)
		maps.Copy(next, *cur)
		next[id] = CommandRef{Plugin: plugin, Title: title}
		if h.commands.CompareAndSwap(cur, &next) {
			return
		}
	}
}

func (h *Host) unregisterCommand(id string) {
	for {
		cur := h.commands.Load().(*map[string]CommandRef)
		if _, ok := (*cur)[id]; !ok {
			return
		}
		next := make(map[string]CommandRef, len(*cur))
		for k, v := range *cur {
			if k == id {
				continue
			}
			next[k] = v
		}
		if h.commands.CompareAndSwap(cur, &next) {
			return
		}
	}
}

// ExecuteCommand invokes the command identified by id. For an in-proc
// owning plugin, runs synchronously. For an out-of-proc owning plugin,
// returns a tea.Cmd that resolves to a PluginResponseMsg when the RPC
// completes.
func (h *Host) ExecuteCommand(id string, args json.RawMessage) CommandResult {
	ref, ok := h.Commands()[id]
	if !ok {
		return CommandResult{Sync: &CommandSyncResult{
			Err: fmt.Errorf("plugin: unknown command %q", id),
		}}
	}
	// In-proc path.
	for _, p := range h.registry.InProc() {
		if p.Name() != ref.Plugin {
			continue
		}
		h.mu.RLock()
		unhealthy := h.unhealthy[p.Name()]
		h.mu.RUnlock()
		if unhealthy {
			return CommandResult{Sync: &CommandSyncResult{
				Err: fmt.Errorf("plugin: %s is unhealthy", p.Name()),
			}}
		}
		h.markActivated(p.Name())
		effects := h.callOnEvent(p, ExecuteCommand{ID: id, Args: args})
		var resultRaw json.RawMessage
		// In-proc plugins may optionally return an ExecuteCommandResult as
		// the first effect; most signal via effects alone.
		return CommandResult{Sync: &CommandSyncResult{
			Result:  resultRaw,
			Effects: effects,
		}}
	}
	// Out-of-proc path.
	for _, m := range h.registry.Manifests() {
		if m.Name != ref.Plugin {
			continue
		}
		reqID := h.nextID()
		ext, err := h.ensureSpawned(m)
		if err != nil {
			return CommandResult{Sync: &CommandSyncResult{
				Err: fmt.Errorf("plugin: spawn %s: %w", m.Name, err),
			}}
		}
		h.markActivated(m.Name)
		ch := make(chan *Response, 1)
		ext.pending.Store(normalizeID(reqID), ch)
		// Enqueue the request on the writer.
		ext.enqueue(outFrame{
			isNotif: false,
			method:  "executeCommand",
			id:      reqID,
			params:  ExecuteCommand{ID: id, Args: args},
		})
		plugin := m.Name
		return CommandResult{Async: func() tea.Msg {
			select {
			case resp, ok := <-ch:
				if !ok || resp == nil {
					return PluginResponseMsg{
						Plugin: plugin,
						ID:     reqID,
						Err: &RPCError{
							Code:    ErrInternal,
							Message: "plugin exited before responding",
						},
					}
				}
				return PluginResponseMsg{
					Plugin: plugin,
					ID:     resp.ID,
					Result: resp.Result,
					Err:    resp.Error,
				}
			case <-time.After(30 * time.Second):
				ext.pending.Delete(normalizeID(reqID))
				return PluginResponseMsg{
					Plugin: plugin,
					ID:     reqID,
					Err: &RPCError{
						Code:    ErrInternal,
						Message: "plugin command timed out",
					},
				}
			}
		}}
	}
	return CommandResult{Sync: &CommandSyncResult{
		Err: fmt.Errorf("plugin: owner %q of command %q is not registered", ref.Plugin, id),
	}}
}

// Recv returns a tea.Cmd that blocks on the aggregated inbound channel and
// returns exactly one PluginMsg. The caller must re-subscribe by issuing
// Recv() again after handling the message.
func (h *Host) Recv() tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-h.inbound
		if !ok {
			return nil
		}
		// Update internal host state before surfacing to the caller.
		h.handleInternal(msg)
		return msg
	}
}

// handleInternal performs host-side bookkeeping for selected inbound
// messages before they are surfaced to the model.
func (h *Host) handleInternal(msg PluginMsg) {
	switch m := msg.(type) {
	case PluginNotifMsg:
		switch m.Method {
		case "commands/register":
			var p struct {
				ID    string `json:"id"`
				Title string `json:"title"`
			}
			if err := json.Unmarshal(m.Params, &p); err == nil && p.ID != "" {
				h.registerCommand(m.Plugin, p.ID, p.Title)
			}
		case "commands/unregister":
			var p struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(m.Params, &p); err == nil && p.ID != "" {
				h.unregisterCommand(p.ID)
			}
		}
	case PluginExitedMsg:
		h.mu.Lock()
		if ext, ok := h.running[m.Name]; ok {
			delete(h.running, m.Name)
			// Drop any commands that were owned by this plugin.
			cur := h.commands.Load().(*map[string]CommandRef)
			next := make(map[string]CommandRef, len(*cur))
			for k, v := range *cur {
				if v.Plugin == m.Name {
					continue
				}
				next[k] = v
			}
			h.commands.Store(&next)
			_ = ext
		}
		// An abnormal exit (non-nil Err) marks the plugin unhealthy so
		// subsequent Emit calls do not respawn it within the same session.
		if m.Err != nil {
			h.unhealthy[m.Name] = true
		}
		h.mu.Unlock()
	}
}

// Respond writes a JSON-RPC response back to an out-of-process plugin for a
// previously received PluginReqMsg. result is marshaled via encoding/json;
// if rpcErr is non-nil an error response is sent instead.
func (h *Host) Respond(plugin string, id any, result any, rpcErr *RPCError) error {
	h.mu.RLock()
	ext, ok := h.running[plugin]
	h.mu.RUnlock()
	if !ok {
		return fmt.Errorf("plugin: %s is not running", plugin)
	}
	ext.enqueueResponse(id, result, rpcErr)
	return nil
}

// Shutdown delivers Shutdown to every running plugin and waits up to
// timeout for external processes to exit. Survivors are killed.
func (h *Host) Shutdown(timeout time.Duration) error {
	// In-proc plugins: deliver the event, then call Shutdown().
	for _, p := range h.registry.InProc() {
		h.mu.RLock()
		activated := h.activated[p.Name()]
		unhealthy := h.unhealthy[p.Name()]
		h.mu.RUnlock()
		if !activated || unhealthy {
			continue
		}
		_ = h.callOnEvent(p, Shutdown{})
		if err := h.callShutdown(p); err != nil {
			fmt.Fprintf(os.Stderr, "plugin: %s shutdown: %v\n", p.Name(), err)
		}
	}

	// Out-of-proc plugins.
	h.mu.Lock()
	exts := make([]*extPlugin, 0, len(h.running))
	for _, ext := range h.running {
		exts = append(exts, ext)
	}
	h.mu.Unlock()

	for _, ext := range exts {
		ext.enqueue(outFrame{isNotif: true, method: "shutdown", params: Shutdown{}})
		ext.closeWrites()
	}

	deadline := time.Now().Add(timeout)
	for _, ext := range exts {
		remaining := max(time.Until(deadline), 0)
		if !ext.waitFor(remaining) {
			_ = ext.kill()
		}
	}
	return nil
}

func (h *Host) callShutdown(p Plugin) (err error) {
	defer func() {
		// Panic boundary: protect the editor's shutdown path from a
		// misbehaving plugin.
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in Shutdown: %v", r)
		}
	}()
	return p.Shutdown()
}

// nextID returns a fresh outbound request ID.
func (h *Host) nextID() int64 {
	return h.nextReqID.Add(1)
}

// ensureSpawned returns the running external plugin for manifest m, spawning
// it on first call. The caller's goroutine blocks until Initialize has
// completed (or a 2s timeout elapses).
func (h *Host) ensureSpawned(m *Manifest) (*extPlugin, error) {
	h.mu.RLock()
	ext, ok := h.running[m.Name]
	h.mu.RUnlock()
	if ok {
		return ext, nil
	}
	if err := h.spawn(m); err != nil {
		return nil, err
	}
	h.mu.RLock()
	ext = h.running[m.Name]
	h.mu.RUnlock()
	if ext == nil {
		return nil, fmt.Errorf("plugin: %s failed to come online", m.Name)
	}
	return ext, nil
}

// describeEvent returns the wire method + params for event along with the
// derived ActivationEvent (if any). hasActivation reports whether an
// activation could be computed — false for events like DidChange that only
// go to already-activated plugins.
func describeEvent(event any) (method string, params any, ev ActivationEvent, hasActivation bool) {
	switch e := event.(type) {
	case Initialize:
		return "initialize", e, ActivationEvent{Kind: ActStart}, true
	case DidOpen:
		return "didOpen", e, ActivationEvent{Kind: ActLanguage, Value: e.Lang}, true
	case DidChange:
		return "didChange", e, ActivationEvent{}, false
	case DidSave:
		return "didSave", e, ActivationEvent{Kind: ActSave, Value: e.Path}, true
	case DidClose:
		return "didClose", e, ActivationEvent{}, false
	case Shutdown:
		return "shutdown", e, ActivationEvent{}, false
	case ExecuteCommand:
		return "executeCommand", e, ActivationEvent{Kind: ActCommand, Value: e.ID}, true
	}
	return "", nil, ActivationEvent{}, false
}
