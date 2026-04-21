// Package plugintest provides an in-memory harness that lets plugin
// authors exercise their plugsdk.Plugin implementations without spawning a
// real host process.
//
// A Harness wires the plugin up to an io.Pipe pair and drives the real SDK
// Run loop on the other end, so tests exercise the production JSON-RPC
// framing, dispatch, and response correlation paths. The harness records
// every outbound notification and request from the plugin under a mutex;
// its inspection methods return copies so tests can assert without
// worrying about concurrent mutation.
//
// Typical usage:
//
//	h := plugintest.New(t, &myPlugin{})
//	if _, err := h.Initialize(nil); err != nil {
//	    t.Fatal(err)
//	}
//	h.DidSave("/tmp/file.go")
//	if got := h.Notifications(); len(got) == 0 {
//	    t.Fatal("expected a notification after DidSave")
//	}
//
// The harness is goroutine-safe. Close is registered via t.Cleanup when
// New is called, so tests do not need to defer anything.
package plugintest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/savimcio/nistru/plugin"
	"github.com/savimcio/nistru/sdk/plugsdk"
)

// Segment is a recorded statusBar/set entry. Mirrors the SDK's wire shape.
type Segment struct {
	// Text is the label the plugin asked the host to render.
	Text string
	// Color is the plugin-supplied colour hint.
	Color string
}

// RecordedNotification is one notification the plugin sent to the fake host.
// Params is the raw JSON payload; test code typically json.Unmarshals it.
type RecordedNotification struct {
	// Method is the JSON-RPC method name.
	Method string
	// Params is the raw JSON parameter payload.
	Params json.RawMessage
}

// RecordedRequest is one request the plugin sent to the fake host. The
// harness auto-replies to every request (default: success with a nil
// result) so the plugin's Client.roundTrip calls unblock. Override the
// reply policy with Harness.SetRequestResponder.
type RecordedRequest struct {
	// ID is the JSON-RPC request id as it came off the wire.
	ID any
	// Method is the JSON-RPC method name.
	Method string
	// Params is the raw JSON parameter payload.
	Params json.RawMessage
}

// RequestResponder chooses the reply a Harness sends back for a plugin
// request. Return (result, nil) for a success response, (nil, rpcErr) for
// an error response, or (nil, nil) for a success response with a nil
// result payload.
type RequestResponder func(req RecordedRequest) (result any, rpcErr *plugin.RPCError)

// defaultResponder replies to every plugin request with a nil-result
// success response.
func defaultResponder(RecordedRequest) (any, *plugin.RPCError) { return nil, nil }

// Harness is an in-memory fake host wired to a plugsdk.Plugin via the real
// SDK Run loop. Construct with New. The zero value is not usable.
type Harness struct {
	t *testing.T

	codec *plugin.Codec
	// pipePluginSide is the ReadWriteCloser handed to the SDK. Keeping a
	// handle lets us tear it down on Close.
	pipePluginSide io.ReadWriteCloser

	// pipeCloser closes all underlying io.Pipe endpoints; idempotent.
	pipeCloser func()

	nextID atomic.Int64

	mu            sync.Mutex
	notifs        []RecordedNotification
	requests      []RecordedRequest
	segments      map[string]Segment
	commands      []string
	commandsIndex map[string]string // id -> title
	responses     map[int64]*plugin.Response
	pending       map[int64]chan *plugin.Response
	responder     RequestResponder

	runDone chan error // buffered cap 1; never drained until Close observes it
	runErr  error      // set once runDone has been drained
	runRead atomic.Bool
	closed  atomic.Bool
}

// New wires plugin p up to an in-memory fake host and starts the SDK Run
// loop in a background goroutine. The returned Harness is ready to accept
// events. Close is registered via t.Cleanup; callers do not need to defer
// anything.
func New(t *testing.T, p plugsdk.Plugin) *Harness {
	t.Helper()
	pluginSide, hostCodec, pipeCloser := newPipePair()
	h := &Harness{
		t:              t,
		codec:          hostCodec,
		pipePluginSide: pluginSide,
		pipeCloser:     pipeCloser,
		segments:       make(map[string]Segment),
		commandsIndex:  make(map[string]string),
		responses:      make(map[int64]*plugin.Response),
		pending:        make(map[int64]chan *plugin.Response),
		responder:      defaultResponder,
		runDone:        make(chan error, 1),
	}

	go func() {
		h.runDone <- plugsdk.RunWith(p, pluginSide)
	}()

	go h.readLoop()

	t.Cleanup(func() {
		_ = h.Close()
	})
	return h
}

// SetRequestResponder overrides how the harness replies to plugin requests
// (buffer/edit, openFile, and any future round-trip). Call before driving
// events that trigger those requests.
func (h *Harness) SetRequestResponder(r RequestResponder) {
	if r == nil {
		r = defaultResponder
	}
	h.mu.Lock()
	h.responder = r
	h.mu.Unlock()
}

// readLoop drains frames from the plugin side and records them. It exits
// when the codec reads an error or EOF.
func (h *Harness) readLoop() {
	for {
		method, id, params, isResp, resp, err := h.codec.Read()
		if err != nil {
			return
		}
		if isResp {
			if resp == nil {
				continue
			}
			numID, ok := responseID(resp.ID)
			if !ok {
				continue
			}
			h.mu.Lock()
			h.responses[numID] = resp
			ch, exists := h.pending[numID]
			if exists {
				delete(h.pending, numID)
			}
			h.mu.Unlock()
			if exists {
				select {
				case ch <- resp:
				default:
				}
			}
			continue
		}
		if id == nil {
			h.recordNotification(method, params)
			continue
		}
		h.recordRequestAndReply(method, id, params)
	}
}

// recordNotification saves a plugin-originated notification and applies
// high-level bookkeeping for well-known methods (commands/register,
// commands/unregister, statusBar/set, ui/notify).
func (h *Harness) recordNotification(method string, params json.RawMessage) {
	paramsCopy := append(json.RawMessage(nil), params...)
	h.mu.Lock()
	h.notifs = append(h.notifs, RecordedNotification{Method: method, Params: paramsCopy})
	switch method {
	case "commands/register":
		var ev struct{ ID, Title string }
		if err := json.Unmarshal(paramsCopy, &ev); err == nil && ev.ID != "" {
			if _, ok := h.commandsIndex[ev.ID]; !ok {
				h.commands = append(h.commands, ev.ID)
			}
			h.commandsIndex[ev.ID] = ev.Title
		}
	case "commands/unregister":
		var ev struct{ ID string }
		if err := json.Unmarshal(paramsCopy, &ev); err == nil && ev.ID != "" {
			delete(h.commandsIndex, ev.ID)
			for i, c := range h.commands {
				if c == ev.ID {
					h.commands = append(h.commands[:i], h.commands[i+1:]...)
					break
				}
			}
		}
	case "statusBar/set":
		var ev struct{ Segment, Text, Color string }
		if err := json.Unmarshal(paramsCopy, &ev); err == nil {
			if ev.Text == "" {
				delete(h.segments, ev.Segment)
			} else {
				h.segments[ev.Segment] = Segment{Text: ev.Text, Color: ev.Color}
			}
		}
	}
	h.mu.Unlock()
}

// recordRequestAndReply saves a plugin-originated request and synthesises
// a reply using the current responder. The reply goes through the codec
// so the SDK observes a real response frame.
func (h *Harness) recordRequestAndReply(method string, id any, params json.RawMessage) {
	paramsCopy := append(json.RawMessage(nil), params...)
	req := RecordedRequest{ID: id, Method: method, Params: paramsCopy}
	h.mu.Lock()
	h.requests = append(h.requests, req)
	r := h.responder
	h.mu.Unlock()
	result, rpcErr := r(req)
	if err := h.codec.WriteResponse(id, result, rpcErr); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		// Pipe closed during shutdown — expected, swallow. Other errors
		// surface via t.Errorf so tests aren't silent about protocol
		// failures.
		h.t.Errorf("plugintest: WriteResponse(%v, %s): %v", id, method, err)
	}
}

// Notifications returns a copy of every notification the plugin has sent
// in the order they arrived.
func (h *Harness) Notifications() []RecordedNotification {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]RecordedNotification, len(h.notifs))
	copy(out, h.notifs)
	return out
}

// Requests returns a copy of every request the plugin has sent in the
// order they arrived.
func (h *Harness) Requests() []RecordedRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]RecordedRequest, len(h.requests))
	copy(out, h.requests)
	return out
}

// StatusSegments returns a copy of the current statusBar/set segment map
// keyed by segment name. Cleared entries (SetStatusBar called with empty
// text) are not included.
func (h *Harness) StatusSegments() map[string]Segment {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]Segment, len(h.segments))
	maps.Copy(out, h.segments)
	return out
}

// Commands returns the ordered list of command ids the plugin has
// registered and not yet unregistered.
func (h *Harness) Commands() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.commands))
	copy(out, h.commands)
	return out
}

// LastResponseFor returns the response the plugin emitted for the given
// host request id, if any, together with a presence flag.
func (h *Harness) LastResponseFor(id int64) (*plugin.Response, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.responses[id]
	if !ok {
		return nil, false
	}
	// Return a shallow copy; Response contains pointers/json.RawMessage
	// slices but the harness does not mutate them after insertion.
	cp := *r
	return &cp, true
}

// Initialize feeds an initialize request and waits for the plugin's
// response. Returns the response's Result payload and any RPC error.
// params may be a plugin.Initialize struct, a map, or nil (defaults to
// {}).
func (h *Harness) Initialize(params any) (json.RawMessage, error) {
	if params == nil {
		params = plugin.Initialize{}
	}
	resp, err := h.request("initialize", params)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, fmt.Errorf("plugintest: initialize: host disconnected")
	}
	if resp.Error != nil {
		return nil, resp.Error
	}
	return resp.Result, nil
}

// DidOpen feeds a didOpen notification.
func (h *Harness) DidOpen(path, lang, content string) {
	h.notify("didOpen", plugin.DidOpen{Path: path, Lang: lang, Text: content})
}

// DidChange feeds a didChange notification.
func (h *Harness) DidChange(path, content string) {
	h.notify("didChange", plugin.DidChange{Path: path, Text: content})
}

// DidSave feeds a didSave notification.
func (h *Harness) DidSave(path string) {
	h.notify("didSave", plugin.DidSave{Path: path})
}

// DidClose feeds a didClose notification.
func (h *Harness) DidClose(path string) {
	h.notify("didClose", plugin.DidClose{Path: path})
}

// ExecuteCommand feeds an executeCommand request and waits for the
// response. args is encoded to JSON and passed through as params.Args.
func (h *Harness) ExecuteCommand(id string, args any) (json.RawMessage, error) {
	var raw json.RawMessage
	if args != nil {
		b, err := json.Marshal(args)
		if err != nil {
			return nil, fmt.Errorf("plugintest: marshal args: %w", err)
		}
		raw = b
	}
	resp, err := h.request("executeCommand", plugin.ExecuteCommand{ID: id, Args: raw})
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, fmt.Errorf("plugintest: executeCommand: host disconnected")
	}
	if resp.Error != nil {
		return nil, resp.Error
	}
	return resp.Result, nil
}

// Shutdown feeds a shutdown request, waits for the SDK to return, then
// closes the pipes. Subsequent event calls are no-ops. The returned error
// is whatever RunWith produced; nil on clean shutdown.
func (h *Harness) Shutdown() error {
	if h.closed.Load() {
		return nil
	}
	if _, err := h.request("shutdown", struct{}{}); err != nil {
		// Even if the handshake errored we still want to tear down.
		_ = err
	}
	runErr := h.waitRun(2 * time.Second)
	// Mark closed and release pipes so later event calls are no-ops
	// rather than blocking on writes to a now-idle reader.
	if h.closed.CompareAndSwap(false, true) {
		h.pipeCloser()
	}
	return runErr
}

// Close releases every resource the harness owns: it closes the pipes,
// waits for the Run goroutine to return, and fails the test via
// t.Errorf if Run returned an unexpected error.
//
// Close is idempotent and is registered with t.Cleanup automatically. It
// is exported so tests that want deterministic teardown before a later
// assertion can call it explicitly.
func (h *Harness) Close() error {
	if !h.closed.CompareAndSwap(false, true) {
		return nil
	}
	h.pipeCloser()
	// Unblock any pending request waiters.
	h.mu.Lock()
	for id, ch := range h.pending {
		delete(h.pending, id)
		select {
		case ch <- nil:
		default:
		}
	}
	h.mu.Unlock()
	err := h.waitRun(2 * time.Second)
	// A clean EOF returns nil. ErrClosedPipe comes from closing the pipe
	// while the SDK is mid-read and is expected.
	if err != nil && !errors.Is(err, io.ErrClosedPipe) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return err
	}
	return nil
}

// waitRun blocks up to timeout for the RunWith goroutine to return. Safe
// to call multiple times; the terminal error is cached on first observation.
func (h *Harness) waitRun(timeout time.Duration) error {
	if h.runRead.Load() {
		return h.runErr
	}
	select {
	case err := <-h.runDone:
		h.runErr = err
		h.runRead.Store(true)
		return err
	case <-time.After(timeout):
		return fmt.Errorf("plugintest: RunWith did not return within %v", timeout)
	}
}

// notify writes a JSON-RPC notification to the plugin.
func (h *Harness) notify(method string, params any) {
	if h.closed.Load() {
		return
	}
	if err := h.codec.WriteNotification(method, params); err != nil {
		h.t.Errorf("plugintest: WriteNotification(%s): %v", method, err)
	}
}

// request writes a JSON-RPC request and blocks for the response, honoring
// a 2-second deadline to prevent stuck tests.
func (h *Harness) request(method string, params any) (*plugin.Response, error) {
	if h.closed.Load() {
		return nil, fmt.Errorf("plugintest: harness closed")
	}
	id := h.nextID.Add(1)
	ch := make(chan *plugin.Response, 1)
	h.mu.Lock()
	h.pending[id] = ch
	h.mu.Unlock()

	if err := h.codec.WriteRequest(method, id, params); err != nil {
		h.mu.Lock()
		delete(h.pending, id)
		h.mu.Unlock()
		return nil, fmt.Errorf("plugintest: WriteRequest(%s): %w", method, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	select {
	case resp := <-ch:
		return resp, nil
	case <-ctx.Done():
		h.mu.Lock()
		delete(h.pending, id)
		h.mu.Unlock()
		return nil, fmt.Errorf("plugintest: %s: timed out waiting for response", method)
	}
}

// newPipePair returns a plugin-side io.ReadWriteCloser and a host-side
// Codec that talk to each other via two io.Pipes.
func newPipePair() (pluginSide io.ReadWriteCloser, hostCodec *plugin.Codec, cleanup func()) {
	pr, pw := io.Pipe() // plugin reads from this; host writes to this
	hr, hw := io.Pipe() // host reads from this; plugin writes to this
	pluginSide = &pipe{r: pr, w: hw}
	hostCodec = plugin.NewCodec(&pipe{r: hr, w: pw})
	var once sync.Once
	cleanup = func() {
		once.Do(func() {
			_ = pr.Close()
			_ = pw.Close()
			_ = hr.Close()
			_ = hw.Close()
		})
	}
	return
}

// pipe glues an io.Reader and io.Writer into an io.ReadWriteCloser for the
// plugin.Codec constructor.
type pipe struct {
	r io.Reader
	w io.Writer
}

func (p *pipe) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *pipe) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *pipe) Close() error {
	if rc, ok := p.r.(io.Closer); ok {
		_ = rc.Close()
	}
	if wc, ok := p.w.(io.Closer); ok {
		_ = wc.Close()
	}
	return nil
}

// responseID normalises the id field on a Response to an int64 for
// harness-side bookkeeping. JSON numbers decode as float64 by default; the
// harness only mints integer ids, so any other id shape (string, nil,
// negative) is rejected.
func responseID(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		if n < 0 {
			return 0, false
		}
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, false
		}
		return i, true
	}
	return 0, false
}
