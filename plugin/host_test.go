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
