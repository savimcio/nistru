package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// TestMain re-executes this test binary as a stand-in plugin child process
// whenever PLUGIN_MODE is set in the environment. Each mode drives a
// different fake behavior exercised by an out-of-proc test below. The helper
// lives in testhelpers_test.go.
func TestMain(m *testing.M) {
	if mode := os.Getenv("PLUGIN_MODE"); mode != "" {
		runAsPlugin(mode)
		return
	}
	os.Exit(m.Run())
}

// -----------------------------------------------------------------------------
// In-proc host exercises.

func TestHost_EmitInProc(t *testing.T) {
	f := &fakeInProcPlugin{name: "fake", acts: []string{"onLanguage:go"}}
	h := newHostWithPlugins(f)
	h.Emit(DidOpen{Path: "/x/y.go", Lang: "go", Text: "package y"})

	if len(f.events) != 1 {
		t.Fatalf("events = %d, want 1", len(f.events))
	}
	dop, ok := f.events[0].(DidOpen)
	if !ok {
		t.Fatalf("event type = %T, want DidOpen", f.events[0])
	}
	if dop.Path != "/x/y.go" {
		t.Fatalf("path = %q, want /x/y.go", dop.Path)
	}
}

func TestHost_EmitReturnsEffects(t *testing.T) {
	want := Notify{Level: "info", Message: "hi"}
	f := &fakeInProcPlugin{
		name: "fake",
		acts: []string{"onLanguage:go"},
		effs: []Effect{want},
	}
	h := newHostWithPlugins(f)
	got := h.Emit(DidOpen{Path: "/a.go", Lang: "go"})
	if len(got) != 1 {
		t.Fatalf("effects = %d, want 1", len(got))
	}
	n, ok := got[0].(Notify)
	if !ok {
		t.Fatalf("effect[0] = %T, want Notify", got[0])
	}
	if n.Message != "hi" {
		t.Fatalf("message = %q, want hi", n.Message)
	}
}

func TestHost_EmitSkipsNonMatching(t *testing.T) {
	f := &fakeInProcPlugin{name: "fake", acts: []string{"onLanguage:py"}}
	h := newHostWithPlugins(f)
	h.Emit(DidOpen{Path: "/x.go", Lang: "go"})
	if len(f.events) != 0 {
		t.Fatalf("events = %d, want 0 (mismatched language)", len(f.events))
	}
}

func TestHost_DispatchKey(t *testing.T) {
	effect := OpenFile{Path: "/open/me"}
	p := &fakePane{
		fakeInProcPlugin: fakeInProcPlugin{
			name:     "pane",
			acts:     []string{"onStart"},
			effs:     []Effect{effect},
			paneSlot: "left",
		},
	}
	h := newHostWithPlugins(p)
	got := h.DispatchKey("pane", KeyEvent{Key: "enter"})
	if len(got) != 1 {
		t.Fatalf("effects = %d, want 1", len(got))
	}
	if of, ok := got[0].(OpenFile); !ok || of.Path != "/open/me" {
		t.Fatalf("effect = %+v, want OpenFile{/open/me}", got[0])
	}
	if len(p.keys) != 1 || p.keys[0].Key != "enter" {
		t.Fatalf("keys = %+v", p.keys)
	}
}

func TestHost_DispatchKey_UnknownPlugin(t *testing.T) {
	h := newHostWithPlugins()
	got := h.DispatchKey("ghost", KeyEvent{Key: "x"})
	if got != nil {
		t.Fatalf("effects = %+v, want nil", got)
	}
}

func TestHost_PanicRecovery(t *testing.T) {
	f := &fakeInProcPlugin{
		name:  "panicky",
		acts:  []string{"onLanguage:go"},
		panic: true,
	}
	h := newHostWithPlugins(f)
	effects := h.Emit(DidOpen{Path: "/p.go", Lang: "go"})
	if effects != nil {
		t.Fatalf("Emit returned %v, want nil after panic", effects)
	}

	// The panic flag is still set, but a second Emit must NOT re-enter
	// OnEvent — the plugin has been marked unhealthy.
	f.panic = false
	_ = h.Emit(DidOpen{Path: "/q.go", Lang: "go"})
	if len(f.events) != 0 {
		t.Fatalf("crashed plugin still received events: %+v", f.events)
	}
}

func TestHost_PaneByName(t *testing.T) {
	p := &fakePane{
		fakeInProcPlugin: fakeInProcPlugin{
			name:     "left-pane",
			acts:     []string{"onStart"},
			paneSlot: "left",
		},
	}
	h := newHostWithPlugins(p)
	if got := h.PaneByName("left-pane"); got == nil {
		t.Fatalf("PaneByName returned nil")
	}
	if got := h.PaneByName("does-not-exist"); got != nil {
		t.Fatalf("PaneByName returned %v, want nil", got)
	}
	if got := h.Pane("left"); got == nil {
		t.Fatalf("Pane(left) returned nil")
	}
}

// -----------------------------------------------------------------------------
// HostAware / PostNotif exercises.

// hostAwarePlugin is a minimal in-proc plugin that records the host passed to
// SetHost. Declared locally to avoid leaking HostAware semantics into the
// default test fixtures, which must stay ignorant of the optional interface.
type hostAwarePlugin struct {
	fakeInProcPlugin
	mu  sync.Mutex
	got *Host
}

func (p *hostAwarePlugin) SetHost(h *Host) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.got = h
}

func (p *hostAwarePlugin) host() *Host {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.got
}

func TestHostSetsHostAwareOnStart(t *testing.T) {
	p := &hostAwarePlugin{
		fakeInProcPlugin: fakeInProcPlugin{name: "aware", acts: []string{"onStart"}},
	}
	h := newHostWithPlugins(p)
	if got := p.host(); got != h {
		t.Fatalf("SetHost got %p, want host %p", got, h)
	}
}

func TestPostNotifDeliversToInbound(t *testing.T) {
	p := &fakeInProcPlugin{name: "myplugin", acts: []string{"onStart"}}
	h := newHostWithPlugins(p)

	type payload struct {
		Level   string `json:"level"`
		Message string `json:"message"`
	}
	want := payload{Level: "info", Message: "hello"}

	done := make(chan error, 1)
	go func() {
		done <- h.PostNotif("myplugin", "ui/notify", want)
	}()

	cmd := h.Recv()
	msg := cmd().(PluginMsg)

	if err := <-done; err != nil {
		t.Fatalf("PostNotif: %v", err)
	}
	n, ok := msg.(PluginNotifMsg)
	if !ok {
		t.Fatalf("msg = %T, want PluginNotifMsg", msg)
	}
	if n.Plugin != "myplugin" || n.Method != "ui/notify" {
		t.Fatalf("notif = %+v, want {myplugin ui/notify ...}", n)
	}
	var got payload
	if err := json.Unmarshal(n.Params, &got); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}
	if got != want {
		t.Fatalf("params = %+v, want %+v", got, want)
	}
}

func TestPostNotifCommandRegistration(t *testing.T) {
	p := &fakeInProcPlugin{name: "cmds", acts: []string{"onStart"}}
	h := newHostWithPlugins(p)

	params := struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}{ID: "cmds.do", Title: "Do a thing"}

	if err := h.PostNotif("cmds", "commands/register", params); err != nil {
		t.Fatalf("PostNotif: %v", err)
	}

	// Synchronous bookkeeping: the command must be visible immediately,
	// without anyone having drained the inbound channel.
	cmds := h.Commands()
	ref, ok := cmds["cmds.do"]
	if !ok {
		t.Fatalf("Commands()[cmds.do] missing; have %+v", cmds)
	}
	if ref.Plugin != "cmds" || ref.Title != "Do a thing" {
		t.Fatalf("CommandRef = %+v, want {cmds \"Do a thing\"}", ref)
	}

	// Drain the synthetic notif so the channel doesn't block future tests.
	<-h.inbound
}

func TestPostNotifDropsOnFullInbound(t *testing.T) {
	p := &fakeInProcPlugin{name: "flood", acts: []string{"onStart"}}
	h := newHostWithPlugins(p)

	// Fill the inbound channel to capacity via PostNotif itself. Each call
	// runs handleInternal (a no-op for ui/notify) and then a non-blocking
	// send; we expect every call up to inboundCap to succeed.
	for i := range inboundCap {
		if err := h.PostNotif("flood", "ui/notify", map[string]string{
			"message": "fill",
		}); err != nil {
			t.Fatalf("PostNotif(#%d) unexpectedly failed: %v", i, err)
		}
	}

	// Guard against a silently hung call by timing out the next PostNotif.
	// The non-blocking send must return promptly with the "inbound full"
	// sentinel error.
	done := make(chan error, 1)
	go func() {
		done <- h.PostNotif("flood", "ui/notify", map[string]string{
			"message": "overflow",
		})
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("PostNotif on full inbound returned nil, want error")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("PostNotif blocked on full inbound; want non-blocking drop")
	}

	// Drain so the host can be shut down cleanly (the test doesn't care
	// about the messages themselves).
	for range inboundCap {
		<-h.inbound
	}
}

// -----------------------------------------------------------------------------
// Per-plugin config (ConfigReceiver / Initialize.Config) exercises.

// configReceiverPlugin is a minimal in-proc plugin that records every
// OnConfig call. Declared locally so the default fakeInProcPlugin stays
// ignorant of the optional interface.
type configReceiverPlugin struct {
	fakeInProcPlugin
	mu        sync.Mutex
	received  []json.RawMessage
	returnErr error
}

func (p *configReceiverPlugin) OnConfig(raw json.RawMessage) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Copy — callers are free to retain the slice.
	cp := make(json.RawMessage, len(raw))
	copy(cp, raw)
	p.received = append(p.received, cp)
	return p.returnErr
}

func (p *configReceiverPlugin) calls() []json.RawMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]json.RawMessage, len(p.received))
	copy(out, p.received)
	return out
}

// panickyConfigReceiver panics on OnConfig; used to verify the host's panic
// boundary around the optional hook.
type panickyConfigReceiver struct {
	fakeInProcPlugin
}

func (p *panickyConfigReceiver) OnConfig(raw json.RawMessage) error {
	panic("boom-in-onconfig")
}

func TestHost_InProc_ConfigReceiver_ReceivesOwnSubtree(t *testing.T) {
	want := json.RawMessage(`{"a":1}`)
	p := &configReceiverPlugin{
		fakeInProcPlugin: fakeInProcPlugin{name: "cfg", acts: []string{"onStart"}},
	}
	other := &fakeInProcPlugin{name: "other", acts: []string{"onStart"}}
	h := newHostWithPlugins(p, other)
	h.SetPluginConfig(func(name string) json.RawMessage {
		if name == "cfg" {
			return want
		}
		return nil
	})

	h.Emit(Initialize{RootPath: "/root"})

	calls := p.calls()
	if len(calls) != 1 {
		t.Fatalf("OnConfig calls = %d, want 1", len(calls))
	}
	if string(calls[0]) != string(want) {
		t.Fatalf("OnConfig raw = %s, want %s", calls[0], want)
	}
}

func TestHost_InProc_InitializeEvent_CarriesConfig(t *testing.T) {
	want := json.RawMessage(`{"a":1}`)
	p := &fakeInProcPlugin{name: "plain", acts: []string{"onStart"}}
	h := newHostWithPlugins(p)
	h.SetPluginConfig(func(name string) json.RawMessage {
		if name == "plain" {
			return want
		}
		return nil
	})

	h.Emit(Initialize{RootPath: "/root"})

	if len(p.events) != 1 {
		t.Fatalf("events = %d, want 1", len(p.events))
	}
	init, ok := p.events[0].(Initialize)
	if !ok {
		t.Fatalf("event type = %T, want Initialize", p.events[0])
	}
	if string(init.Config) != string(want) {
		t.Fatalf("Initialize.Config = %s, want %s", init.Config, want)
	}
	if init.RootPath != "/root" {
		t.Fatalf("Initialize.RootPath = %q, want /root", init.RootPath)
	}
}

func TestHost_InProc_NoLookup_NoConfig(t *testing.T) {
	recv := &configReceiverPlugin{
		fakeInProcPlugin: fakeInProcPlugin{name: "cfg", acts: []string{"onStart"}},
	}
	h := newHostWithPlugins(recv)
	// No SetPluginConfig call at all.

	h.Emit(Initialize{RootPath: "/root"})

	// Unified contract: OnConfig fires on every Initialize dispatch, even
	// when no lookup is installed. Plugins that implement ConfigReceiver
	// see OnConfig(nil) as a signal to keep/reset defaults.
	calls := recv.calls()
	if len(calls) != 1 {
		t.Fatalf("OnConfig calls = %d, want 1 (nil raw when no lookup)", len(calls))
	}
	if len(calls[0]) != 0 {
		t.Fatalf("OnConfig raw = %s, want nil/empty without lookup", calls[0])
	}
	if len(recv.events) != 1 {
		t.Fatalf("events = %d, want 1", len(recv.events))
	}
	init, ok := recv.events[0].(Initialize)
	if !ok {
		t.Fatalf("event type = %T, want Initialize", recv.events[0])
	}
	if init.Config != nil {
		t.Fatalf("Initialize.Config = %s, want nil without lookup", init.Config)
	}
}

func TestHost_InProc_OnConfigPanics_MarksUnhealthy(t *testing.T) {
	p := &panickyConfigReceiver{
		fakeInProcPlugin: fakeInProcPlugin{name: "panicky", acts: []string{"onStart"}},
	}
	h := newHostWithPlugins(p)
	h.SetPluginConfig(func(name string) json.RawMessage {
		return json.RawMessage(`{"x":true}`)
	})

	h.Emit(Initialize{RootPath: "/root"})

	// Initialize must NOT have been delivered (panic skipped OnEvent).
	if len(p.events) != 0 {
		t.Fatalf("plugin got events after OnConfig panic: %+v", p.events)
	}
	h.mu.RLock()
	unhealthy := h.unhealthy[p.Name()]
	h.mu.RUnlock()
	if !unhealthy {
		t.Fatalf("plugin not marked unhealthy after OnConfig panic")
	}

	// Subsequent emits must not deliver either.
	h.Emit(DidOpen{Path: "/a.go", Lang: "go"})
	if len(p.events) != 0 {
		t.Fatalf("unhealthy plugin received follow-up events: %+v", p.events)
	}
}

// TestHost_OutOfProc_SpawnHandshake_CarriesConfig asserts the spawn-path
// Initialize frame carries the plugin's configured sub-tree, symmetric with
// the Emit-path injection covered above. Spawning a real subprocess just to
// read the handshake would require a separate test binary and is brittle, so
// instead we exercise the pure buildInitializeFrame helper that spawn now
// delegates to — that helper is the single point that pulls config from the
// host and embeds it in the outbound Initialize.
func TestHost_OutOfProc_SpawnHandshake_CarriesConfig(t *testing.T) {
	want := json.RawMessage(`{"channel":"dev"}`)
	reg := NewRegistry()
	m := &Manifest{
		Name:       "ext",
		Version:    "0.0.1",
		Cmd:        []string{"ignored"},
		Activation: []string{"onLanguage:go"},
	}
	reg.manifests = append(reg.manifests, m)
	h := NewHost(reg)
	if err := h.Start(""); err != nil {
		t.Fatalf("Start: %v", err)
	}
	h.SetPluginConfig(func(name string) json.RawMessage {
		if name == "ext" {
			return want
		}
		return nil
	})

	frame := h.buildInitializeFrame(m, "/root", int64(1))
	if frame.method != "initialize" {
		t.Fatalf("frame.method = %q, want initialize", frame.method)
	}
	if frame.isNotif {
		t.Fatalf("frame.isNotif = true, want false (request)")
	}
	init, ok := frame.params.(Initialize)
	if !ok {
		t.Fatalf("frame.params type = %T, want Initialize", frame.params)
	}
	if string(init.Config) != string(want) {
		t.Fatalf("Initialize.Config = %s, want %s", init.Config, want)
	}
	if init.RootPath != "/root" {
		t.Fatalf("Initialize.RootPath = %q, want /root", init.RootPath)
	}
	if len(init.Capabilities) == 0 {
		t.Fatalf("Initialize.Capabilities empty, want host capabilities")
	}
}

// TestHost_ReEmitInitialize_DeliversToActivatedOnly asserts that
// ReEmitInitialize re-fires OnConfig + OnEvent(Initialize) for in-proc
// plugins that have already been activated and skips those that have not.
// The dormant plugin shares the same activation, so the scope test is
// "did Initialize dispatch previously reach this plugin", not "does the
// activation match".
func TestHost_ReEmitInitialize_DeliversToActivatedOnly(t *testing.T) {
	// Two ConfigReceiver stubs. Only the first activates (onStart). The
	// second activates on a language we never emit, so Initialize never
	// lands on it — it stays dormant until ReEmit runs.
	active := &configReceiverPlugin{
		fakeInProcPlugin: fakeInProcPlugin{name: "active", acts: []string{"onStart"}},
	}
	dormant := &configReceiverPlugin{
		fakeInProcPlugin: fakeInProcPlugin{name: "dormant", acts: []string{"onLanguage:py"}},
	}
	h := newHostWithPlugins(active, dormant)

	// First config tree.
	first := json.RawMessage(`{"k":1}`)
	h.SetPluginConfig(func(name string) json.RawMessage {
		if name == "active" {
			return first
		}
		return nil
	})
	h.Emit(Initialize{RootPath: "/r"})

	// Sanity: only active saw OnConfig once.
	if n := len(active.calls()); n != 1 {
		t.Fatalf("pre: active OnConfig calls = %d, want 1", n)
	}
	if n := len(dormant.calls()); n != 0 {
		t.Fatalf("pre: dormant OnConfig calls = %d, want 0", n)
	}
	if n := len(active.events); n != 1 {
		t.Fatalf("pre: active events = %d, want 1", n)
	}
	if n := len(dormant.events); n != 0 {
		t.Fatalf("pre: dormant events = %d, want 0", n)
	}

	// Swap the config and re-emit.
	second := json.RawMessage(`{"k":2}`)
	h.SetPluginConfig(func(name string) json.RawMessage {
		if name == "active" {
			return second
		}
		return nil
	})
	h.ReEmitInitialize()

	// Active: one more OnConfig with the new bytes, one more Initialize.
	activeCalls := active.calls()
	if len(activeCalls) != 2 {
		t.Fatalf("post: active OnConfig calls = %d, want 2", len(activeCalls))
	}
	if string(activeCalls[1]) != string(second) {
		t.Fatalf("post: active second OnConfig = %s, want %s", activeCalls[1], second)
	}
	if len(active.events) != 2 {
		t.Fatalf("post: active events = %d, want 2", len(active.events))
	}
	init, ok := active.events[1].(Initialize)
	if !ok {
		t.Fatalf("post: active second event = %T, want Initialize", active.events[1])
	}
	if string(init.Config) != string(second) {
		t.Fatalf("post: Initialize.Config = %s, want %s", init.Config, second)
	}

	// Dormant: must stay untouched.
	if n := len(dormant.calls()); n != 0 {
		t.Fatalf("post: dormant OnConfig calls = %d, want 0", n)
	}
	if n := len(dormant.events); n != 0 {
		t.Fatalf("post: dormant events = %d, want 0", n)
	}
}

// TestHost_ReEmitInitialize_CallsOnConfigWithNilWhenLookupReturnsNil
// exercises the host-layer fix for the "treepane reload leaves stale
// skip_dirs when config is removed" bug. On reload (ReEmitInitialize), the
// host MUST call OnConfig unconditionally — even when the installed lookup
// returns nil for the plugin's name — so ConfigReceiver implementations can
// reset config-derived state. The initial Emit path keeps the "OnConfig
// fires only when raw != nil" guard; only the reload dispatch changes.
func TestHost_ReEmitInitialize_CallsOnConfigWithNilWhenLookupReturnsNil(t *testing.T) {
	stub := &configReceiverPlugin{
		fakeInProcPlugin: fakeInProcPlugin{name: "stub", acts: []string{"onStart"}},
	}
	h := newHostWithPlugins(stub)

	// First activation with a real config sub-tree so the plugin is
	// recorded as activated and OnConfig lands with the expected bytes.
	first := json.RawMessage(`{"x":1}`)
	h.SetPluginConfig(func(name string) json.RawMessage {
		if name == "stub" {
			return first
		}
		return nil
	})
	h.Emit(Initialize{RootPath: "/r"})

	calls := stub.calls()
	if len(calls) != 1 {
		t.Fatalf("pre: OnConfig calls = %d, want 1", len(calls))
	}
	if string(calls[0]) != string(first) {
		t.Fatalf("pre: OnConfig[0] = %s, want %s", calls[0], first)
	}

	// Simulate the user removing [plugins.stub] from config: the lookup
	// now returns nil for every name.
	h.SetPluginConfig(func(name string) json.RawMessage {
		return nil
	})

	// Reload must deliver OnConfig(nil) despite raw being nil, so the
	// plugin can reset config-derived state back to defaults.
	h.ReEmitInitialize()

	calls = stub.calls()
	if len(calls) < 2 {
		t.Fatalf("post: OnConfig calls = %d, want >= 2", len(calls))
	}
	// Expect to observe at least one nil OnConfig after the lookup swap.
	sawNil := false
	for _, c := range calls[1:] {
		if len(c) == 0 {
			sawNil = true
			break
		}
	}
	if !sawNil {
		t.Fatalf("post: no OnConfig(nil) observed after reload; calls = %v", calls)
	}

	// The Initialize event itself must still land, carrying nil Config.
	if len(stub.events) < 2 {
		t.Fatalf("post: events = %d, want >= 2 (Initialize re-emit)", len(stub.events))
	}
	init, ok := stub.events[1].(Initialize)
	if !ok {
		t.Fatalf("post: second event = %T, want Initialize", stub.events[1])
	}
	if init.Config != nil {
		t.Fatalf("post: Initialize.Config = %s, want nil after lookup removal", init.Config)
	}
}

// TestHost_Emit_InitialBoot_CallsOnConfigEvenWhenRawNil asserts the unified
// ConfigReceiver contract on the initial Emit path: OnConfig fires on every
// Initialize dispatch, even when the lookup returns nil for the plugin's
// name. A nil raw signals "no [plugins.<name>] section is present; reset to
// defaults". This matches the reload path (ReEmitInitialize) and the
// out-of-process SDK dispatch semantics, so receivers can't be caught out
// by split-semantics between transports or call sites.
func TestHost_Emit_InitialBoot_CallsOnConfigEvenWhenRawNil(t *testing.T) {
	stub := &configReceiverPlugin{
		fakeInProcPlugin: fakeInProcPlugin{name: "stub", acts: []string{"onStart"}},
	}
	h := newHostWithPlugins(stub)
	// Lookup installed, but returns nil for every name.
	h.SetPluginConfig(func(name string) json.RawMessage {
		return nil
	})
	h.Emit(Initialize{RootPath: "/r"})

	calls := stub.calls()
	if len(calls) != 1 {
		t.Fatalf("OnConfig calls = %d, want 1 even when raw is nil", len(calls))
	}
	if len(calls[0]) != 0 {
		t.Fatalf("OnConfig raw = %s, want nil/empty", calls[0])
	}
	if len(stub.events) != 1 {
		t.Fatalf("Initialize not delivered: events = %d, want 1", len(stub.events))
	}
}

// TestHost_OutOfProc_SpawnHandshake_NoConfigIsOmitted asserts the helper
// emits a nil Config when no lookup is installed, so the wire JSON omits
// the field (omitempty) and plugins without config see no surprise value.
func TestHost_OutOfProc_SpawnHandshake_NoConfigIsOmitted(t *testing.T) {
	reg := NewRegistry()
	m := &Manifest{Name: "ext", Version: "0.0.1", Cmd: []string{"ignored"}}
	reg.manifests = append(reg.manifests, m)
	h := NewHost(reg)
	if err := h.Start(""); err != nil {
		t.Fatalf("Start: %v", err)
	}

	frame := h.buildInitializeFrame(m, "/root", int64(1))
	init, ok := frame.params.(Initialize)
	if !ok {
		t.Fatalf("frame.params type = %T, want Initialize", frame.params)
	}
	if init.Config != nil {
		t.Fatalf("Initialize.Config = %s, want nil without a lookup", init.Config)
	}
}

// -----------------------------------------------------------------------------
// Out-of-proc host exercises.

func TestHost_SpawnAndInitialize(t *testing.T) {
	h, _, _ := newHostWithManifest(t, "ok", nil)
	defer h.Shutdown(time.Second)

	h.Emit(DidOpen{Path: "/a.go", Lang: "go", Text: ""})

	got := recv(t, h, 5*time.Second)
	if s, ok := got.(PluginStartedMsg); !ok || s.Name != "fake" {
		t.Fatalf("got %T %+v, want PluginStartedMsg{fake}", got, got)
	}
}

func TestHost_InitializeTimeout(t *testing.T) {
	h, _, _ := newHostWithManifest(t, "slow_init", nil)
	defer h.Shutdown(time.Second)

	// Emit kicks spawn, which must surface a spawn error because initialize
	// doesn't complete within the 2s budget. The error is logged to stderr;
	// the running map must end up empty.
	h.Emit(DidOpen{Path: "/a.go", Lang: "go", Text: ""})

	// After the timeout fires, the running map must not contain the plugin.
	waitUntil(t, 5*time.Second, func() bool {
		h.mu.RLock()
		_, stillThere := h.running["fake"]
		h.mu.RUnlock()
		return !stillThere
	})
}

func TestHost_CrashOnEvent(t *testing.T) {
	h, _, _ := newHostWithManifest(t, "crash_on_didopen", nil)
	defer h.Shutdown(time.Second)

	h.Emit(DidOpen{Path: "/a.go", Lang: "go", Text: ""})

	// Expect first a PluginStartedMsg, then an Exited with non-nil err.
	var sawExit bool
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !sawExit {
		select {
		case msg := <-h.inbound:
			h.handleInternal(msg)
			if e, ok := msg.(PluginExitedMsg); ok {
				if e.Err == nil {
					t.Fatalf("PluginExitedMsg err is nil, want non-nil")
				}
				sawExit = true
			}
		case <-time.After(500 * time.Millisecond):
		}
	}
	if !sawExit {
		t.Fatalf("did not observe PluginExitedMsg within deadline")
	}

	// Further Emit calls must not respawn. The unhealthy bookkeeping is set
	// synchronously in handleInternal above, so a second Emit returning
	// immediately and the running map being empty are both required
	// conditions — no sleep needed.
	h.Emit(DidOpen{Path: "/b.go", Lang: "go", Text: ""})
	h.mu.RLock()
	_, running := h.running["fake"]
	unhealthy := h.unhealthy["fake"]
	h.mu.RUnlock()
	if running {
		t.Fatalf("plugin respawned after crash")
	}
	if !unhealthy {
		t.Fatalf("plugin not marked unhealthy after crash")
	}
}

func TestHost_NotifyRoundTrip(t *testing.T) {
	h, _, _ := newHostWithManifest(t, "notify_hello", nil)
	defer h.Shutdown(time.Second)

	h.Emit(DidOpen{Path: "/a.go", Lang: "go", Text: ""})

	// Expect a PluginNotifMsg{ui/notify} within a handful of messages.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case msg := <-h.inbound:
			h.handleInternal(msg)
			if n, ok := msg.(PluginNotifMsg); ok && n.Method == "ui/notify" {
				var p struct {
					Level   string `json:"level"`
					Message string `json:"message"`
				}
				if err := json.Unmarshal(n.Params, &p); err != nil {
					t.Fatalf("unmarshal notify params: %v", err)
				}
				if p.Message != "hello-from-plugin" {
					t.Fatalf("message = %q, want hello-from-plugin", p.Message)
				}
				return
			}
		case <-time.After(500 * time.Millisecond):
		}
	}
	t.Fatalf("did not observe ui/notify within deadline")
}

func TestHost_DidChangeCoalescing(t *testing.T) {
	record := filepath.Join(t.TempDir(), "record")
	h, _, _ := newHostWithManifest(t, "flood_didchange", map[string]string{
		"PLUGIN_RECORD": record,
	})
	defer h.Shutdown(time.Second)

	// Bring the plugin online first.
	h.Emit(DidOpen{Path: "/a.go", Lang: "go", Text: ""})
	waitForStarted(t, h, "fake", 5*time.Second)

	// Fire 1000 DidChange for the same path back-to-back.
	const N = 1000
	for i := range N {
		h.Emit(DidChange{Path: "/a.go", Text: "change-" + strconv.Itoa(i)})
	}

	// Give the writer goroutine time to drain coalesced updates.
	waitUntil(t, 5*time.Second, func() bool {
		data, err := os.ReadFile(record)
		if err != nil {
			return false
		}
		// The last write must have text "change-999".
		_, text := parseRecord(string(data))
		return text == "change-"+strconv.Itoa(N-1)
	})

	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	count, text := parseRecord(string(data))
	if text != "change-"+strconv.Itoa(N-1) {
		t.Fatalf("final text = %q, want change-%d", text, N-1)
	}
	if count >= int64(N) {
		t.Fatalf("count = %d, want strictly fewer than %d (coalescing failed)", count, N)
	}
	if count < 1 {
		t.Fatalf("count = %d, want >= 1", count)
	}
}

func TestHost_ShutdownGraceful(t *testing.T) {
	h, _, _ := newHostWithManifest(t, "ok", nil)

	h.Emit(DidOpen{Path: "/a.go", Lang: "go", Text: ""})
	waitForStarted(t, h, "fake", 5*time.Second)

	start := time.Now()
	if err := h.Shutdown(3 * time.Second); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Shutdown took %s, want <3s", elapsed)
	}
}

func TestHost_ShutdownForceKill(t *testing.T) {
	h, _, _ := newHostWithManifest(t, "hang", nil)

	h.Emit(DidOpen{Path: "/a.go", Lang: "go", Text: ""})
	waitForStarted(t, h, "fake", 5*time.Second)

	start := time.Now()
	_ = h.Shutdown(200 * time.Millisecond)
	elapsed := time.Since(start)
	if elapsed > time.Second {
		t.Fatalf("Shutdown took %s, want ~250ms", elapsed)
	}
}

// -----------------------------------------------------------------------------
// Low-level writer/codec safety: make sure concurrent Emit calls don't trip
// races in the extPlugin writer path. Uses a minimal in-memory peer so we
// don't spawn a subprocess.
func TestExtPlugin_WriterConcurrencySafety(t *testing.T) {
	// Self-contained smoke: many concurrent WriteNotification calls against
	// one codec must produce valid frames for a single reader. Mirrors the
	// production host's writer serialization guarantee.
	a, b, cleanup := pipePair()
	defer cleanup()

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = a.WriteNotification("didChange", DidChange{Path: "/p", Text: strconv.Itoa(i)})
		}(i)
	}

	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for range 50 {
			_, _, _, _, _, err := b.Read()
			if err != nil {
				t.Errorf("Read: %v", err)
				return
			}
		}
	}()

	wg.Wait()
	select {
	case <-readerDone:
	case <-time.After(3 * time.Second):
		t.Fatalf("reader did not drain 50 frames")
	}
}
