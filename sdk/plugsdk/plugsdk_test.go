package plugsdk

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/savimcio/nistru/plugin"
)

// rwc glues an io.Reader and io.Writer into an io.ReadWriteCloser for the
// codec. Mirrors the helper in plugin/protocol_test.go but local to this
// package to avoid cross-package test fixture coupling.
type rwc struct {
	r io.Reader
	w io.Writer
}

func (p *rwc) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *rwc) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *rwc) Close() error {
	if rc, ok := p.r.(io.Closer); ok {
		_ = rc.Close()
	}
	if wc, ok := p.w.(io.Closer); ok {
		_ = wc.Close()
	}
	return nil
}

// pipePair returns two Codecs wired via two io.Pipes so A's writes are
// B's reads and vice versa. cleanup closes all four pipe ends.
func pipePair() (pluginSide io.ReadWriteCloser, hostCodec *plugin.Codec, cleanup func()) {
	pr, pw := io.Pipe() // plugin reads from this; host writes to this
	hr, hw := io.Pipe() // host reads from this; plugin writes to this
	pluginSide = &rwc{r: pr, w: hw}
	hostCodec = plugin.NewCodec(&rwc{r: hr, w: pw})
	cleanup = func() {
		_ = pr.Close()
		_ = pw.Close()
		_ = hr.Close()
		_ = hw.Close()
	}
	return
}

// recordingPlugin is a Plugin implementation that records every invocation so
// tests can assert exactly-once delivery of each event kind. It embeds Base
// only for ClientReceiver wiring — all Plugin methods are overridden below.
type recordingPlugin struct {
	Base

	mu            sync.Mutex
	initCalls     []plugin.Initialize
	shutdownCalls int
	didOpenCalls  []plugin.DidOpen
	didChange     []plugin.DidChange
	didSave       []plugin.DidSave
	didClose      []plugin.DidClose
	execCalls     []plugin.ExecuteCommand

	initErr error
	execErr error
	execRet any
}

func (r *recordingPlugin) OnInitialize(root string, caps []string) error {
	r.mu.Lock()
	r.initCalls = append(r.initCalls, plugin.Initialize{RootPath: root, Capabilities: caps})
	r.mu.Unlock()
	return r.initErr
}

func (r *recordingPlugin) OnShutdown() error {
	r.mu.Lock()
	r.shutdownCalls++
	r.mu.Unlock()
	return nil
}

func (r *recordingPlugin) OnDidOpen(path, lang, text string) {
	r.mu.Lock()
	r.didOpenCalls = append(r.didOpenCalls, plugin.DidOpen{Path: path, Lang: lang, Text: text})
	r.mu.Unlock()
}

func (r *recordingPlugin) OnDidChange(path, text string) {
	r.mu.Lock()
	r.didChange = append(r.didChange, plugin.DidChange{Path: path, Text: text})
	r.mu.Unlock()
}

func (r *recordingPlugin) OnDidSave(path string) {
	r.mu.Lock()
	r.didSave = append(r.didSave, plugin.DidSave{Path: path})
	r.mu.Unlock()
}

func (r *recordingPlugin) OnDidClose(path string) {
	r.mu.Lock()
	r.didClose = append(r.didClose, plugin.DidClose{Path: path})
	r.mu.Unlock()
}

func (r *recordingPlugin) OnExecuteCommand(id string, args json.RawMessage) (any, error) {
	r.mu.Lock()
	r.execCalls = append(r.execCalls, plugin.ExecuteCommand{ID: id, Args: append(json.RawMessage(nil), args...)})
	ret, err := r.execRet, r.execErr
	r.mu.Unlock()
	return ret, err
}

// startRun launches RunWith in a goroutine and returns a done channel carrying
// the terminal error. Callers are responsible for triggering termination
// (typically by sending "shutdown" or closing the pipe).
func startRun(p Plugin, pluginSide io.ReadWriteCloser) <-chan error {
	done := make(chan error, 1)
	go func() {
		done <- RunWith(p, pluginSide)
	}()
	return done
}

// readOne reads a single frame and fails the test if the read errors out or
// takes longer than 2 seconds.
func readOne(t *testing.T, codec *plugin.Codec) (method string, id any, params json.RawMessage, isResp bool, resp *plugin.Response) {
	t.Helper()
	type result struct {
		method string
		id     any
		params json.RawMessage
		isResp bool
		resp   *plugin.Response
		err    error
	}
	ch := make(chan result, 1)
	go func() {
		m, id, p, ir, rp, err := codec.Read()
		ch <- result{m, id, p, ir, rp, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("codec.Read: %v", r.err)
		}
		return r.method, r.id, r.params, r.isResp, r.resp
	case <-time.After(2 * time.Second):
		t.Fatalf("codec.Read: timed out")
	}
	return "", nil, nil, false, nil
}

// waitDone waits up to timeout for the Run goroutine to finish and returns
// its terminal error.
func waitDone(t *testing.T, done <-chan error, timeout time.Duration) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		t.Fatalf("RunWith did not return within %v", timeout)
	}
	return nil
}

// TestRun_DispatchesEventsExactlyOnce feeds every event kind the SDK
// recognises and asserts the corresponding plugin method was invoked
// exactly once with the right params.
func TestRun_DispatchesEventsExactlyOnce(t *testing.T) {
	pluginSide, host, cleanup := pipePair()
	defer cleanup()

	p := &recordingPlugin{}
	done := startRun(p, pluginSide)

	// Initialize — notification form (no id). Plugin should record it.
	if err := host.WriteNotification("initialize", plugin.Initialize{RootPath: "/tmp/root", Capabilities: []string{"commands"}}); err != nil {
		t.Fatalf("write initialize: %v", err)
	}
	if err := host.WriteNotification("didOpen", plugin.DidOpen{Path: "/a.go", Lang: "go", Text: "package a"}); err != nil {
		t.Fatalf("write didOpen: %v", err)
	}
	if err := host.WriteNotification("didChange", plugin.DidChange{Path: "/a.go", Text: "package a // changed"}); err != nil {
		t.Fatalf("write didChange: %v", err)
	}
	if err := host.WriteNotification("didSave", plugin.DidSave{Path: "/a.go"}); err != nil {
		t.Fatalf("write didSave: %v", err)
	}
	if err := host.WriteNotification("didClose", plugin.DidClose{Path: "/a.go"}); err != nil {
		t.Fatalf("write didClose: %v", err)
	}
	// executeCommand as a notification (fire-and-forget).
	if err := host.WriteNotification("executeCommand", plugin.ExecuteCommand{ID: "hello"}); err != nil {
		t.Fatalf("write executeCommand: %v", err)
	}

	// Graceful shutdown triggers Run to return.
	if err := host.WriteNotification("shutdown", struct{}{}); err != nil {
		t.Fatalf("write shutdown: %v", err)
	}

	if err := waitDone(t, done, 2*time.Second); err != nil {
		t.Fatalf("RunWith: unexpected error %v", err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if got, want := len(p.initCalls), 1; got != want {
		t.Fatalf("OnInitialize calls = %d, want %d", got, want)
	}
	if p.initCalls[0].RootPath != "/tmp/root" || len(p.initCalls[0].Capabilities) != 1 || p.initCalls[0].Capabilities[0] != "commands" {
		t.Fatalf("OnInitialize = %+v", p.initCalls[0])
	}
	if got, want := len(p.didOpenCalls), 1; got != want {
		t.Fatalf("OnDidOpen calls = %d, want %d", got, want)
	}
	if p.didOpenCalls[0] != (plugin.DidOpen{Path: "/a.go", Lang: "go", Text: "package a"}) {
		t.Fatalf("OnDidOpen = %+v", p.didOpenCalls[0])
	}
	if got, want := len(p.didChange), 1; got != want {
		t.Fatalf("OnDidChange calls = %d, want %d", got, want)
	}
	if p.didChange[0].Path != "/a.go" || p.didChange[0].Text != "package a // changed" {
		t.Fatalf("OnDidChange = %+v", p.didChange[0])
	}
	if got, want := len(p.didSave), 1; got != want {
		t.Fatalf("OnDidSave calls = %d, want %d", got, want)
	}
	if p.didSave[0].Path != "/a.go" {
		t.Fatalf("OnDidSave = %+v", p.didSave[0])
	}
	if got, want := len(p.didClose), 1; got != want {
		t.Fatalf("OnDidClose calls = %d, want %d", got, want)
	}
	if p.didClose[0].Path != "/a.go" {
		t.Fatalf("OnDidClose = %+v", p.didClose[0])
	}
	if got, want := len(p.execCalls), 1; got != want {
		t.Fatalf("OnExecuteCommand calls = %d, want %d", got, want)
	}
	if p.execCalls[0].ID != "hello" {
		t.Fatalf("OnExecuteCommand = %+v", p.execCalls[0])
	}
	if p.shutdownCalls != 1 {
		t.Fatalf("OnShutdown calls = %d, want 1", p.shutdownCalls)
	}
}

// TestRun_UnknownMethodAsRequestEmitsMethodNotFound exercises the SDK's
// error contract: a request with an unknown method must produce a
// MethodNotFound RPC error response, not a panic or drop.
func TestRun_UnknownMethodAsRequestEmitsMethodNotFound(t *testing.T) {
	pluginSide, host, cleanup := pipePair()
	defer cleanup()

	done := startRun(&recordingPlugin{}, pluginSide)

	if err := host.WriteRequest("noSuchMethod", 7, struct{}{}); err != nil {
		t.Fatalf("write request: %v", err)
	}
	_, id, _, isResp, resp := readOne(t, host)
	if !isResp {
		t.Fatalf("expected response, got request")
	}
	if resp == nil || resp.Error == nil {
		t.Fatalf("expected error response, got %+v", resp)
	}
	if resp.Error.Code != plugin.ErrMethodNotFound {
		t.Fatalf("error code = %d, want %d", resp.Error.Code, plugin.ErrMethodNotFound)
	}
	if !strings.Contains(resp.Error.Message, "noSuchMethod") {
		t.Fatalf("error message = %q, want to contain 'noSuchMethod'", resp.Error.Message)
	}
	// Normalize the id for comparison — Read returns float64 for JSON numbers.
	if f, ok := id.(float64); !ok || int(f) != 7 {
		// ResponseID echoes back too; double-check via resp.ID.
		if f2, ok2 := resp.ID.(float64); !ok2 || int(f2) != 7 {
			t.Fatalf("echoed id = %v (%T), want 7", resp.ID, resp.ID)
		}
	}

	if err := host.WriteNotification("shutdown", struct{}{}); err != nil {
		t.Fatalf("write shutdown: %v", err)
	}
	if err := waitDone(t, done, 2*time.Second); err != nil {
		t.Fatalf("RunWith: %v", err)
	}
}

// TestRun_UnknownMethodAsNotificationIsSilentlyDropped asserts the SDK
// contract that unknown notifications are logged to stderr but do not
// produce an RPC error on the wire (there is no id to reply to).
func TestRun_UnknownMethodAsNotificationIsSilentlyDropped(t *testing.T) {
	pluginSide, host, cleanup := pipePair()
	defer cleanup()

	done := startRun(&recordingPlugin{}, pluginSide)

	if err := host.WriteNotification("unknown/notif", struct{}{}); err != nil {
		t.Fatalf("write notif: %v", err)
	}
	// Follow with a DidSave to prove the loop kept running.
	if err := host.WriteNotification("didSave", plugin.DidSave{Path: "/x"}); err != nil {
		t.Fatalf("write didSave: %v", err)
	}
	if err := host.WriteNotification("shutdown", struct{}{}); err != nil {
		t.Fatalf("write shutdown: %v", err)
	}
	if err := waitDone(t, done, 2*time.Second); err != nil {
		t.Fatalf("RunWith: %v", err)
	}
}

// TestRun_InitializeAsRequestProducesResponse asserts the SDK replies to an
// initialize *request* (with id) with a success response when the plugin's
// OnInitialize returns nil. The request form is how the real host drives
// the handshake.
func TestRun_InitializeAsRequestProducesResponse(t *testing.T) {
	pluginSide, host, cleanup := pipePair()
	defer cleanup()

	done := startRun(&recordingPlugin{}, pluginSide)

	if err := host.WriteRequest("initialize", 1, plugin.Initialize{RootPath: "/r", Capabilities: []string{"commands"}}); err != nil {
		t.Fatalf("write init: %v", err)
	}
	_, _, _, isResp, resp := readOne(t, host)
	if !isResp || resp == nil {
		t.Fatalf("expected response")
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if resp.Result != nil {
		t.Fatalf("expected nil result, got %s", string(resp.Result))
	}

	if err := host.WriteNotification("shutdown", struct{}{}); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if err := waitDone(t, done, 2*time.Second); err != nil {
		t.Fatalf("RunWith: %v", err)
	}
}

// TestRun_InitializeRequestPropagatesPluginError asserts that an error from
// OnInitialize surfaces as an ErrInternal RPC error response.
func TestRun_InitializeRequestPropagatesPluginError(t *testing.T) {
	pluginSide, host, cleanup := pipePair()
	defer cleanup()

	p := &recordingPlugin{initErr: errors.New("boom")}
	done := startRun(p, pluginSide)

	if err := host.WriteRequest("initialize", 1, plugin.Initialize{RootPath: "/r"}); err != nil {
		t.Fatalf("write init: %v", err)
	}
	_, _, _, isResp, resp := readOne(t, host)
	if !isResp || resp == nil || resp.Error == nil {
		t.Fatalf("expected error response, got isResp=%v resp=%+v", isResp, resp)
	}
	if resp.Error.Code != plugin.ErrInternal {
		t.Fatalf("error code = %d, want %d", resp.Error.Code, plugin.ErrInternal)
	}
	if !strings.Contains(resp.Error.Message, "boom") {
		t.Fatalf("error message = %q", resp.Error.Message)
	}

	if err := host.WriteNotification("shutdown", struct{}{}); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if err := waitDone(t, done, 2*time.Second); err != nil {
		t.Fatalf("RunWith: %v", err)
	}
}

// TestRun_ExecuteCommandRequestReturnsResult asserts the SDK correctly
// frames the plugin's result into a response when OnExecuteCommand returns
// a non-nil value.
func TestRun_ExecuteCommandRequestReturnsResult(t *testing.T) {
	pluginSide, host, cleanup := pipePair()
	defer cleanup()

	p := &recordingPlugin{execRet: map[string]int{"n": 42}}
	done := startRun(p, pluginSide)

	if err := host.WriteRequest("executeCommand", 11, plugin.ExecuteCommand{ID: "do", Args: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("write exec: %v", err)
	}
	_, _, _, isResp, resp := readOne(t, host)
	if !isResp || resp == nil || resp.Error != nil {
		t.Fatalf("expected success response, got isResp=%v resp=%+v", isResp, resp)
	}
	var got map[string]int
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if got["n"] != 42 {
		t.Fatalf("result = %+v", got)
	}

	if err := host.WriteNotification("shutdown", struct{}{}); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if err := waitDone(t, done, 2*time.Second); err != nil {
		t.Fatalf("RunWith: %v", err)
	}
}

// TestRun_ExecuteCommandRequestPropagatesError asserts plugin command
// errors are surfaced as ErrInternal RPC responses.
func TestRun_ExecuteCommandRequestPropagatesError(t *testing.T) {
	pluginSide, host, cleanup := pipePair()
	defer cleanup()

	p := &recordingPlugin{execErr: errors.New("kaboom")}
	done := startRun(p, pluginSide)

	if err := host.WriteRequest("executeCommand", 2, plugin.ExecuteCommand{ID: "nope"}); err != nil {
		t.Fatalf("write exec: %v", err)
	}
	_, _, _, isResp, resp := readOne(t, host)
	if !isResp || resp == nil || resp.Error == nil {
		t.Fatalf("expected error response, got %+v", resp)
	}
	if resp.Error.Code != plugin.ErrInternal || !strings.Contains(resp.Error.Message, "kaboom") {
		t.Fatalf("error = %+v", resp.Error)
	}

	if err := host.WriteNotification("shutdown", struct{}{}); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if err := waitDone(t, done, 2*time.Second); err != nil {
		t.Fatalf("RunWith: %v", err)
	}
}

// TestClient_NotificationFraming asserts that Client.SetStatusBar,
// Client.Notify, and Client.RegisterCommand produce JSON-RPC notifications
// with the documented method names and param shapes.
func TestClient_NotificationFraming(t *testing.T) {
	pluginSide, host, cleanup := pipePair()
	defer cleanup()

	p := &commandRegisteringPlugin{}
	done := startRun(p, pluginSide)

	// The plugin sends three notifications from its OnInitialize. We drive
	// initialize as a request so we can synchronise on the response and
	// know the writes have been flushed.
	if err := host.WriteRequest("initialize", 1, plugin.Initialize{RootPath: "/r"}); err != nil {
		t.Fatalf("write init: %v", err)
	}

	// The SDK invokes OnInitialize synchronously before writing the
	// response, so the three notifications are guaranteed to land on the
	// wire before the init response. Our responseOrdering contract:
	// RegisterCommand, SetStatusBar, Notify, then initialize response.
	type frame struct {
		method string
		isResp bool
		params json.RawMessage
		resp   *plugin.Response
	}
	var frames []frame
	for range 4 {
		m, _, params, isResp, resp := readOne(t, host)
		frames = append(frames, frame{method: m, isResp: isResp, params: params, resp: resp})
	}

	// Find each expected notification anywhere in the first three frames.
	seen := map[string]json.RawMessage{}
	for i, f := range frames {
		if i < 3 {
			if f.isResp {
				t.Fatalf("frame %d unexpectedly a response: %+v", i, f.resp)
			}
			seen[f.method] = f.params
		}
	}
	if _, ok := seen["commands/register"]; !ok {
		t.Fatalf("missing commands/register notification; frames=%+v", frames)
	}
	if _, ok := seen["statusBar/set"]; !ok {
		t.Fatalf("missing statusBar/set notification")
	}
	if _, ok := seen["ui/notify"]; !ok {
		t.Fatalf("missing ui/notify notification")
	}
	if !frames[3].isResp {
		t.Fatalf("expected 4th frame to be init response, got %+v", frames[3])
	}

	// Spot-check param shapes against SDK contract.
	var reg struct {
		ID, Title string
	}
	if err := json.Unmarshal(seen["commands/register"], &reg); err != nil {
		t.Fatalf("unmarshal commands/register: %v", err)
	}
	if reg.ID != "hello" || reg.Title != "Say Hello" {
		t.Fatalf("commands/register = %+v", reg)
	}

	var sb struct {
		Segment, Text, Color string
	}
	if err := json.Unmarshal(seen["statusBar/set"], &sb); err != nil {
		t.Fatalf("unmarshal statusBar/set: %v", err)
	}
	if sb.Segment != "seg" || sb.Text != "42" || sb.Color != "green" {
		t.Fatalf("statusBar/set = %+v", sb)
	}

	var nf struct {
		Level, Message string
	}
	if err := json.Unmarshal(seen["ui/notify"], &nf); err != nil {
		t.Fatalf("unmarshal ui/notify: %v", err)
	}
	if nf.Level != "info" || nf.Message != "ready" {
		t.Fatalf("ui/notify = %+v", nf)
	}

	if err := host.WriteNotification("shutdown", struct{}{}); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if err := waitDone(t, done, 2*time.Second); err != nil {
		t.Fatalf("RunWith: %v", err)
	}
}

// commandRegisteringPlugin registers a command and sets a status-bar
// segment on initialize to exercise Client's outbound methods.
type commandRegisteringPlugin struct {
	Base
}

func (p *commandRegisteringPlugin) OnInitialize(root string, caps []string) error {
	if err := p.Client().RegisterCommand("hello", "Say Hello"); err != nil {
		return err
	}
	if err := p.Client().SetStatusBar("seg", "42", "green"); err != nil {
		return err
	}
	return p.Client().Notify("info", "ready")
}

// TestClient_RoundTripRequestReturnsAfterResponse asserts Client.roundTrip
// framing is correct: the plugin writes a request with an auto-incrementing
// id and unblocks the caller when a matching response arrives.
func TestClient_RoundTripRequestReturnsAfterResponse(t *testing.T) {
	pluginSide, host, cleanup := pipePair()
	defer cleanup()

	p := &roundTripPlugin{fired: make(chan error, 1)}
	done := startRun(p, pluginSide)

	// Drive the plugin to call Client.OpenFile as a fire-and-forget
	// notification so the host side only needs to handle one frame
	// (the openFile request) before the plugin's handler completes.
	if err := host.WriteNotification("executeCommand", plugin.ExecuteCommand{ID: "open"}); err != nil {
		t.Fatalf("write exec notif: %v", err)
	}

	// Expect the openFile request from the plugin and reply to unblock it.
	m, id, params, isResp, _ := readOne(t, host)
	if isResp {
		t.Fatalf("expected openFile request, got response")
	}
	if m != "openFile" {
		t.Fatalf("method = %q, want openFile", m)
	}
	var got struct{ Path string }
	if err := json.Unmarshal(params, &got); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}
	if got.Path != "/tmp/file" {
		t.Fatalf("path = %q", got.Path)
	}
	if err := host.WriteResponse(id, nil, nil); err != nil {
		t.Fatalf("WriteResponse: %v", err)
	}

	// Confirm the plugin's roundTrip call returned nil.
	select {
	case err := <-p.fired:
		if err != nil {
			t.Fatalf("OpenFile returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OpenFile never returned")
	}

	if err := host.WriteNotification("shutdown", struct{}{}); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if err := waitDone(t, done, 2*time.Second); err != nil {
		t.Fatalf("RunWith: %v", err)
	}
}

// roundTripPlugin calls Client.OpenFile in a background goroutine (per the
// SDK contract — handlers must not block, since round-trip replies arrive
// on the same reader goroutine) and records the outcome on fired.
type roundTripPlugin struct {
	Base
	fired chan error
}

func (p *roundTripPlugin) OnExecuteCommand(id string, args json.RawMessage) (any, error) {
	go func() {
		p.fired <- p.Client().OpenFile("/tmp/file")
	}()
	return nil, nil
}

// TestClient_RoundTripUnblocksOnDisconnect asserts Client.drainPending wakes
// any in-flight requests with an error when Run returns (the pipe is
// closed from the host side).
func TestClient_RoundTripUnblocksOnDisconnect(t *testing.T) {
	pluginSide, host, cleanup := pipePair()
	defer cleanup()
	_ = host // host is unused here; we just close to simulate disconnect

	p := &roundTripPlugin{fired: make(chan error, 1)}
	done := startRun(p, pluginSide)

	// Drive roundTrip by sending executeCommand.
	if err := host.WriteNotification("executeCommand", plugin.ExecuteCommand{ID: "open"}); err != nil {
		t.Fatalf("write exec: %v", err)
	}
	// Consume the openFile request so it does not block the writer, then
	// close the pipes — this simulates the host going away before
	// responding.
	_, _, _, _, _ = readOne(t, host)
	cleanup()

	select {
	case err := <-p.fired:
		if err == nil {
			t.Fatal("OpenFile returned nil; expected a disconnect error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OpenFile never returned after disconnect")
	}

	// RunWith should return (non-nil error on pipe close is acceptable).
	_ = waitDone(t, done, 2*time.Second)
}

// TestRun_ShutdownAsRequestEmitsResponse asserts the "shutdown" *request*
// form produces a success response before Run returns.
func TestRun_ShutdownAsRequestEmitsResponse(t *testing.T) {
	pluginSide, host, cleanup := pipePair()
	defer cleanup()

	done := startRun(&recordingPlugin{}, pluginSide)

	if err := host.WriteRequest("shutdown", 99, struct{}{}); err != nil {
		t.Fatalf("write shutdown: %v", err)
	}
	_, _, _, isResp, resp := readOne(t, host)
	if !isResp || resp == nil {
		t.Fatalf("expected response")
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if err := waitDone(t, done, 2*time.Second); err != nil {
		t.Fatalf("RunWith: %v", err)
	}
}

// TestRun_EOFReturnsNil asserts clean EOF (not an arbitrary pipe error) is
// mapped to a nil return by the SDK.
func TestRun_EOFReturnsNil(t *testing.T) {
	// Use an io.Reader that returns EOF immediately so the SDK sees a
	// clean EOF. io.Pipe.Close returns ErrClosedPipe instead of EOF, so we
	// need a different harness.
	rw := &rwc{r: strings.NewReader(""), w: io.Discard}
	done := make(chan error, 1)
	go func() { done <- RunWith(&recordingPlugin{}, rw) }()
	if err := waitDone(t, done, 2*time.Second); err != nil {
		t.Fatalf("RunWith on empty reader (EOF): %v, want nil", err)
	}
}

// TestRun_MalformedParamsRequestProducesInvalidParams asserts a request
// with a syntactically valid but structurally wrong params payload produces
// an InvalidParams RPC response.
func TestRun_MalformedParamsRequestProducesInvalidParams(t *testing.T) {
	pluginSide, host, cleanup := pipePair()
	defer cleanup()

	done := startRun(&recordingPlugin{}, pluginSide)

	// initialize expects an object; send a string instead.
	if err := host.WriteRequest("initialize", 1, json.RawMessage(`"not an object"`)); err != nil {
		t.Fatalf("write request: %v", err)
	}
	_, _, _, isResp, resp := readOne(t, host)
	if !isResp || resp == nil || resp.Error == nil {
		t.Fatalf("expected error response, got %+v", resp)
	}
	if resp.Error.Code != plugin.ErrInvalidParams {
		t.Fatalf("error code = %d, want %d", resp.Error.Code, plugin.ErrInvalidParams)
	}

	if err := host.WriteNotification("shutdown", struct{}{}); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if err := waitDone(t, done, 2*time.Second); err != nil {
		t.Fatalf("RunWith: %v", err)
	}
}

// TestBaseDefaults asserts Base is a zero-value Plugin with sensible defaults.
func TestBaseDefaults(t *testing.T) {
	var b Base
	if err := b.OnInitialize("/r", nil); err != nil {
		t.Fatalf("OnInitialize: %v", err)
	}
	if err := b.OnShutdown(); err != nil {
		t.Fatalf("OnShutdown: %v", err)
	}
	b.OnDidOpen("", "", "")
	b.OnDidChange("", "")
	b.OnDidSave("")
	b.OnDidClose("")
	if res, err := b.OnExecuteCommand("", nil); err != nil || res != nil {
		t.Fatalf("OnExecuteCommand default: %v %v", res, err)
	}
	if b.Client() != nil {
		t.Fatalf("Client() before SetClient must be nil")
	}
	client := newClient(plugin.NewCodec(&rwc{r: strings.NewReader(""), w: io.Discard}))
	b.SetClient(client)
	if b.Client() != client {
		t.Fatalf("Client() after SetClient returned wrong value")
	}
}

// TestBasePluginNoopDefaultsCoverAllHandlers feeds every event kind into a
// plugin that only embeds Base to exercise the no-op defaults; any panic
// would fail the test.
func TestBasePluginNoopDefaultsCoverAllHandlers(t *testing.T) {
	type baseOnly struct{ Base }
	pluginSide, host, cleanup := pipePair()
	defer cleanup()

	done := startRun(&baseOnly{}, pluginSide)

	// All supported notifications — each should dispatch into the Base
	// defaults and return silently.
	writes := []struct {
		method string
		params any
	}{
		{"didOpen", plugin.DidOpen{Path: "/x", Lang: "y", Text: "z"}},
		{"didChange", plugin.DidChange{Path: "/x", Text: "z"}},
		{"didSave", plugin.DidSave{Path: "/x"}},
		{"didClose", plugin.DidClose{Path: "/x"}},
	}
	for _, w := range writes {
		if err := host.WriteNotification(w.method, w.params); err != nil {
			t.Fatalf("write %s: %v", w.method, err)
		}
	}
	// executeCommand as a request — the Base default returns (nil, nil).
	if err := host.WriteRequest("executeCommand", 1, plugin.ExecuteCommand{ID: "unk"}); err != nil {
		t.Fatalf("write exec: %v", err)
	}
	_, _, _, isResp, resp := readOne(t, host)
	if !isResp || resp == nil || resp.Error != nil {
		t.Fatalf("expected success response, got %+v", resp)
	}

	if err := host.WriteNotification("shutdown", struct{}{}); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if err := waitDone(t, done, 2*time.Second); err != nil {
		t.Fatalf("RunWith: %v", err)
	}
}

// TestClient_UnregisterCommandFraming asserts the notification wire format
// for UnregisterCommand.
func TestClient_UnregisterCommandFraming(t *testing.T) {
	pluginSide, host, cleanup := pipePair()
	defer cleanup()

	p := &unregisterPlugin{}
	done := startRun(p, pluginSide)

	if err := host.WriteNotification("initialize", plugin.Initialize{RootPath: "/r"}); err != nil {
		t.Fatalf("write init: %v", err)
	}
	m, _, params, isResp, _ := readOne(t, host)
	if isResp {
		t.Fatalf("expected commands/unregister notification")
	}
	if m != "commands/unregister" {
		t.Fatalf("method = %q, want commands/unregister", m)
	}
	var got struct{ ID string }
	if err := json.Unmarshal(params, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ID != "dead" {
		t.Fatalf("id = %q, want dead", got.ID)
	}

	if err := host.WriteNotification("shutdown", struct{}{}); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if err := waitDone(t, done, 2*time.Second); err != nil {
		t.Fatalf("RunWith: %v", err)
	}
}

type unregisterPlugin struct{ Base }

func (p *unregisterPlugin) OnInitialize(root string, caps []string) error {
	return p.Client().UnregisterCommand("dead")
}

// TestClient_BufferEditFraming asserts the request wire format and
// response-correlation path for BufferEdit.
func TestClient_BufferEditFraming(t *testing.T) {
	pluginSide, host, cleanup := pipePair()
	defer cleanup()

	p := &bufferEditPlugin{fired: make(chan error, 1)}
	done := startRun(p, pluginSide)

	if err := host.WriteNotification("executeCommand", plugin.ExecuteCommand{ID: "edit"}); err != nil {
		t.Fatalf("write exec: %v", err)
	}
	m, id, params, isResp, _ := readOne(t, host)
	if isResp {
		t.Fatalf("expected request")
	}
	if m != "buffer/edit" {
		t.Fatalf("method = %q, want buffer/edit", m)
	}
	var got struct{ Path, Text string }
	if err := json.Unmarshal(params, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Path != "/a" || got.Text != "new" {
		t.Fatalf("params = %+v", got)
	}
	// Reply with an error; SDK should surface it via BufferEdit's return.
	if err := host.WriteResponse(id, nil, &plugin.RPCError{Code: plugin.ErrInternal, Message: "disk full"}); err != nil {
		t.Fatalf("WriteResponse: %v", err)
	}
	select {
	case err := <-p.fired:
		if err == nil {
			t.Fatalf("BufferEdit returned nil, want error")
		}
		var rpcErr *plugin.RPCError
		if !errors.As(err, &rpcErr) || rpcErr.Code != plugin.ErrInternal {
			t.Fatalf("BufferEdit err = %v (want ErrInternal)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("BufferEdit never returned")
	}

	if err := host.WriteNotification("shutdown", struct{}{}); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if err := waitDone(t, done, 2*time.Second); err != nil {
		t.Fatalf("RunWith: %v", err)
	}
}

type bufferEditPlugin struct {
	Base
	fired chan error
}

func (p *bufferEditPlugin) OnExecuteCommand(id string, args json.RawMessage) (any, error) {
	go func() { p.fired <- p.Client().BufferEdit("/a", "new") }()
	return nil, nil
}

// configRecordingPlugin implements ConfigReceiver and records every OnConfig
// invocation (raw bytes + ordering vs. OnInitialize) so tests can assert the
// SDK routes config through the optional interface correctly.
type configRecordingPlugin struct {
	Base
	mu            sync.Mutex
	configs       []json.RawMessage
	initsAfterCfg int // incremented on OnInitialize only when configs non-empty at call time
	inits         int
}

func (p *configRecordingPlugin) OnConfig(raw json.RawMessage) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := make(json.RawMessage, len(raw))
	copy(cp, raw)
	p.configs = append(p.configs, cp)
	return nil
}

func (p *configRecordingPlugin) OnInitialize(root string, caps []string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inits++
	if len(p.configs) > 0 {
		p.initsAfterCfg++
	}
	return nil
}

// TestSDK_Initialize_RoutesConfigToReceiver asserts an initialize frame
// carrying a Config sub-tree invokes OnConfig with the exact bytes before
// OnInitialize fires.
func TestSDK_Initialize_RoutesConfigToReceiver(t *testing.T) {
	pluginSide, host, cleanup := pipePair()
	defer cleanup()

	p := &configRecordingPlugin{}
	done := startRun(p, pluginSide)

	want := json.RawMessage(`{"a":1}`)
	if err := host.WriteRequest("initialize", 1, plugin.Initialize{
		RootPath:     "/r",
		Capabilities: []string{"commands"},
		Config:       want,
	}); err != nil {
		t.Fatalf("write init: %v", err)
	}
	_, _, _, isResp, resp := readOne(t, host)
	if !isResp || resp == nil || resp.Error != nil {
		t.Fatalf("expected success response, got isResp=%v resp=%+v", isResp, resp)
	}

	p.mu.Lock()
	if got := len(p.configs); got != 1 {
		p.mu.Unlock()
		t.Fatalf("OnConfig calls = %d, want 1", got)
	}
	if string(p.configs[0]) != string(want) {
		p.mu.Unlock()
		t.Fatalf("OnConfig raw = %s, want %s", p.configs[0], want)
	}
	if p.inits != 1 {
		p.mu.Unlock()
		t.Fatalf("OnInitialize calls = %d, want 1", p.inits)
	}
	if p.initsAfterCfg != 1 {
		p.mu.Unlock()
		t.Fatalf("OnInitialize fired before OnConfig (initsAfterCfg=%d)", p.initsAfterCfg)
	}
	p.mu.Unlock()

	if err := host.WriteNotification("shutdown", struct{}{}); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if err := waitDone(t, done, 2*time.Second); err != nil {
		t.Fatalf("RunWith: %v", err)
	}
}

// TestSDK_Initialize_NoConfig_CallsOnConfigWithNil asserts the unified
// ConfigReceiver contract: an initialize frame without a Config field still
// invokes OnConfig with a nil raw argument, signalling "no config section
// is present; reset to defaults". This matches the in-process host contract
// in plugin.ConfigReceiver — both transports have uniform semantics so
// plugin authors don't need to special-case their transport.
func TestSDK_Initialize_NoConfig_CallsOnConfigWithNil(t *testing.T) {
	pluginSide, host, cleanup := pipePair()
	defer cleanup()

	p := &configRecordingPlugin{}
	done := startRun(p, pluginSide)

	if err := host.WriteRequest("initialize", 1, plugin.Initialize{
		RootPath:     "/r",
		Capabilities: []string{"commands"},
	}); err != nil {
		t.Fatalf("write init: %v", err)
	}
	_, _, _, isResp, resp := readOne(t, host)
	if !isResp || resp == nil || resp.Error != nil {
		t.Fatalf("expected success response, got isResp=%v resp=%+v", isResp, resp)
	}

	p.mu.Lock()
	if got := len(p.configs); got != 1 {
		p.mu.Unlock()
		t.Fatalf("OnConfig calls = %d, want 1 (with nil raw when no Config field)", got)
	}
	if got := len(p.configs[0]); got != 0 {
		raw := p.configs[0]
		p.mu.Unlock()
		t.Fatalf("OnConfig raw = %s, want nil/empty when initialize frame has no Config", raw)
	}
	if p.inits != 1 {
		p.mu.Unlock()
		t.Fatalf("OnInitialize calls = %d, want 1", p.inits)
	}
	// OnConfig must still fire before OnInitialize.
	if p.initsAfterCfg != 1 {
		p.mu.Unlock()
		t.Fatalf("OnInitialize fired before OnConfig (initsAfterCfg=%d)", p.initsAfterCfg)
	}
	p.mu.Unlock()

	if err := host.WriteNotification("shutdown", struct{}{}); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if err := waitDone(t, done, 2*time.Second); err != nil {
		t.Fatalf("RunWith: %v", err)
	}
}

// TestSDK_Base_OnConfig_IsNoOp asserts the default Base.OnConfig is a
// harmless no-op for embedders that don't care about config.
func TestSDK_Base_OnConfig_IsNoOp(t *testing.T) {
	var b Base
	if err := b.OnConfig(nil); err != nil {
		t.Fatalf("Base.OnConfig(nil) = %v, want nil", err)
	}
	if err := b.OnConfig([]byte("{}")); err != nil {
		t.Fatalf("Base.OnConfig({}) = %v, want nil", err)
	}
}

// TestToUint64Accepts covers each branch of the id-normalizer the SDK uses
// when routing responses.
func TestToUint64Accepts(t *testing.T) {
	cases := []struct {
		in   any
		want uint64
		ok   bool
	}{
		{float64(42), 42, true},
		{float64(-1), 0, false},
		{int64(7), 7, true},
		{int64(-1), 0, false},
		{uint64(9), 9, true},
		{json.Number("11"), 11, true},
		{json.Number("-4"), 0, false},
		{json.Number("notnum"), 0, false},
		{"string", 0, false},
		{nil, 0, false},
	}
	for _, tc := range cases {
		got, ok := toUint64(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("toUint64(%v) = (%d,%v), want (%d,%v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}
