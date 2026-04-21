package plugintest_test

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/savimcio/nistru/plugin"
	"github.com/savimcio/nistru/sdk/plugsdk"
	"github.com/savimcio/nistru/sdk/plugsdk/plugintest"
)

// fakePlugin is a toy Plugin that exercises every part of the harness:
// it registers a command on initialize, sets a status-bar segment, emits a
// notification on save, and answers an executeCommand by making a
// Client.OpenFile round-trip.
type fakePlugin struct {
	plugsdk.Base

	mu          sync.Mutex
	saved       []string
	changed     []string
	opened      []string
	commandArgs []json.RawMessage
	openFileErr chan error
}

func newFakePlugin() *fakePlugin {
	return &fakePlugin{openFileErr: make(chan error, 1)}
}

func (f *fakePlugin) OnInitialize(root string, caps []string) error {
	if err := f.Client().RegisterCommand("do", "Do Thing"); err != nil {
		return err
	}
	return f.Client().SetStatusBar("status", "idle", "gray")
}

func (f *fakePlugin) OnDidOpen(path, lang, text string) {
	f.mu.Lock()
	f.opened = append(f.opened, path)
	f.mu.Unlock()
}

func (f *fakePlugin) OnDidChange(path, text string) {
	f.mu.Lock()
	f.changed = append(f.changed, path)
	f.mu.Unlock()
}

func (f *fakePlugin) OnDidSave(path string) {
	f.mu.Lock()
	f.saved = append(f.saved, path)
	f.mu.Unlock()
	_ = f.Client().Notify("info", "saved "+path)
}

func (f *fakePlugin) OnExecuteCommand(id string, args json.RawMessage) (any, error) {
	f.mu.Lock()
	f.commandArgs = append(f.commandArgs, append(json.RawMessage(nil), args...))
	f.mu.Unlock()
	if id == "do" {
		// Spawn a goroutine because the SDK runs handlers synchronously
		// on its reader goroutine — blocking on a round-trip from here
		// would deadlock.
		go func() {
			f.openFileErr <- f.Client().OpenFile("/opened")
		}()
		return map[string]string{"status": "ok"}, nil
	}
	return nil, nil
}

func TestHarness_InitializeRegistersCommandAndSegment(t *testing.T) {
	fp := newFakePlugin()
	h := plugintest.New(t, fp)
	if _, err := h.Initialize(nil); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if got := h.Commands(); len(got) != 1 || got[0] != "do" {
		t.Fatalf("Commands() = %v, want [do]", got)
	}
	segs := h.StatusSegments()
	if len(segs) != 1 {
		t.Fatalf("StatusSegments() = %v, want one entry", segs)
	}
	if segs["status"].Text != "idle" || segs["status"].Color != "gray" {
		t.Fatalf("StatusSegments()[status] = %+v", segs["status"])
	}
}

func TestHarness_EventsDispatchAndNotificationsRecord(t *testing.T) {
	fp := newFakePlugin()
	h := plugintest.New(t, fp)
	if _, err := h.Initialize(nil); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	h.DidOpen("/a.go", "go", "package a")
	h.DidChange("/a.go", "package a // edit")
	h.DidSave("/a.go")
	h.DidClose("/a.go")

	// Wait until the save notification is visible by polling Notifications.
	if !waitForNotif(h, "ui/notify", 2) {
		t.Fatalf("ui/notify never arrived; notifs=%+v", h.Notifications())
	}

	fp.mu.Lock()
	opened := append([]string(nil), fp.opened...)
	changed := append([]string(nil), fp.changed...)
	saved := append([]string(nil), fp.saved...)
	fp.mu.Unlock()
	if len(opened) != 1 || opened[0] != "/a.go" {
		t.Fatalf("opened = %v", opened)
	}
	if len(changed) != 1 || changed[0] != "/a.go" {
		t.Fatalf("changed = %v", changed)
	}
	if len(saved) != 1 || saved[0] != "/a.go" {
		t.Fatalf("saved = %v", saved)
	}
}

func TestHarness_ExecuteCommandReturnsResultAndRecordsRoundTrip(t *testing.T) {
	fp := newFakePlugin()
	h := plugintest.New(t, fp)
	if _, err := h.Initialize(nil); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	res, err := h.ExecuteCommand("do", map[string]string{"k": "v"})
	if err != nil {
		t.Fatalf("ExecuteCommand: %v", err)
	}
	var got struct{ Status string }
	if err := json.Unmarshal(res, &got); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if got.Status != "ok" {
		t.Fatalf("result.status = %q, want ok", got.Status)
	}

	// The plugin also fires a round-trip openFile. Wait for it via the
	// plugin's fired channel.
	select {
	case err := <-fp.openFileErr:
		if err != nil {
			t.Fatalf("OpenFile returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OpenFile never returned")
	}

	reqs := h.Requests()
	if len(reqs) != 1 {
		t.Fatalf("Requests() = %+v, want 1 entry", reqs)
	}
	if reqs[0].Method != "openFile" {
		t.Fatalf("request method = %q, want openFile", reqs[0].Method)
	}

	// Args passed through into OnExecuteCommand should also be visible.
	fp.mu.Lock()
	argsSeen := append([]json.RawMessage(nil), fp.commandArgs...)
	fp.mu.Unlock()
	if len(argsSeen) != 1 {
		t.Fatalf("commandArgs = %+v", argsSeen)
	}
	var argsGot struct{ K string }
	if err := json.Unmarshal(argsSeen[0], &argsGot); err != nil {
		t.Fatalf("unmarshal args: %v", err)
	}
	if argsGot.K != "v" {
		t.Fatalf("args.k = %q, want v", argsGot.K)
	}
}

func TestHarness_CustomResponderReturnsErrorToPlugin(t *testing.T) {
	fp := newFakePlugin()
	h := plugintest.New(t, fp)
	h.SetRequestResponder(func(req plugintest.RecordedRequest) (any, *plugin.RPCError) {
		return nil, &plugin.RPCError{Code: plugin.ErrInternal, Message: "denied"}
	})
	if _, err := h.Initialize(nil); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if _, err := h.ExecuteCommand("do", nil); err != nil {
		t.Fatalf("ExecuteCommand: %v", err)
	}
	select {
	case err := <-fp.openFileErr:
		if err == nil {
			t.Fatal("OpenFile returned nil, expected RPC error")
		}
		var rpcErr *plugin.RPCError
		if !errors.As(err, &rpcErr) || rpcErr.Code != plugin.ErrInternal {
			t.Fatalf("OpenFile err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OpenFile never returned")
	}
}

func TestHarness_LastResponseForInitialize(t *testing.T) {
	fp := newFakePlugin()
	h := plugintest.New(t, fp)
	if _, err := h.Initialize(nil); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	// The harness mints request ids starting at 1; Initialize is the
	// first request.
	resp, ok := h.LastResponseFor(1)
	if !ok || resp == nil {
		t.Fatalf("LastResponseFor(1) not found")
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if _, ok := h.LastResponseFor(99); ok {
		t.Fatalf("LastResponseFor(99) should not be found")
	}
}

func TestHarness_SetRequestResponderNilResetsToDefault(t *testing.T) {
	fp := newFakePlugin()
	h := plugintest.New(t, fp)
	// Switch responders then reset.
	h.SetRequestResponder(func(plugintest.RecordedRequest) (any, *plugin.RPCError) { return nil, nil })
	h.SetRequestResponder(nil)
	if _, err := h.Initialize(nil); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
}

func TestHarness_ShutdownReturnsClean(t *testing.T) {
	fp := newFakePlugin()
	h := plugintest.New(t, fp)
	if _, err := h.Initialize(nil); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := h.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// Subsequent events silently no-op because the harness is closed.
	h.DidOpen("/x", "y", "z")
	if _, err := h.ExecuteCommand("do", nil); err == nil {
		t.Fatalf("ExecuteCommand after Shutdown: want error, got nil")
	}
}

func TestHarness_UnregisterCommandDropsIt(t *testing.T) {
	fp := &unregisterExample{}
	h := plugintest.New(t, fp)
	if _, err := h.Initialize(nil); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if got := h.Commands(); len(got) != 1 || got[0] != "tmp" {
		t.Fatalf("Commands() after init = %v", got)
	}
	if _, err := h.ExecuteCommand("drop", nil); err != nil {
		t.Fatalf("ExecuteCommand: %v", err)
	}
	// The unregister notification is fire-and-forget; poll for empty.
	if !waitForEmptyCommands(h, 2) {
		t.Fatalf("commands never cleared: %v", h.Commands())
	}
}

type unregisterExample struct{ plugsdk.Base }

func (u *unregisterExample) OnInitialize(root string, caps []string) error {
	return u.Client().RegisterCommand("tmp", "Temp")
}

func (u *unregisterExample) OnExecuteCommand(id string, args json.RawMessage) (any, error) {
	if id == "drop" {
		return nil, u.Client().UnregisterCommand("tmp")
	}
	return nil, nil
}

func TestHarness_StatusSegmentClearDropsKey(t *testing.T) {
	fp := &togglePlugin{}
	h := plugintest.New(t, fp)
	if _, err := h.Initialize(nil); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if len(h.StatusSegments()) != 1 {
		t.Fatalf("segments after init = %+v", h.StatusSegments())
	}
	if _, err := h.ExecuteCommand("clear", nil); err != nil {
		t.Fatalf("ExecuteCommand: %v", err)
	}
	if !waitForEmptySegments(h, 2) {
		t.Fatalf("segments never cleared: %v", h.StatusSegments())
	}
}

type togglePlugin struct{ plugsdk.Base }

func (p *togglePlugin) OnInitialize(root string, caps []string) error {
	return p.Client().SetStatusBar("seg", "on", "green")
}

func (p *togglePlugin) OnExecuteCommand(id string, args json.RawMessage) (any, error) {
	if id == "clear" {
		return nil, p.Client().SetStatusBar("seg", "", "")
	}
	return nil, nil
}

// waitForNotif polls the harness up to `seconds` seconds for a
// notification with the given method.
func waitForNotif(h *plugintest.Harness, method string, seconds int) bool {
	return poll(seconds, func() bool {
		for _, n := range h.Notifications() {
			if n.Method == method {
				return true
			}
		}
		return false
	})
}

func waitForEmptyCommands(h *plugintest.Harness, seconds int) bool {
	return poll(seconds, func() bool { return len(h.Commands()) == 0 })
}

func waitForEmptySegments(h *plugintest.Harness, seconds int) bool {
	return poll(seconds, func() bool { return len(h.StatusSegments()) == 0 })
}

// poll runs cond repeatedly until it returns true or the deadline expires.
func poll(seconds int, cond func() bool) bool {
	deadline := time.Now().Add(time.Duration(seconds) * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}
