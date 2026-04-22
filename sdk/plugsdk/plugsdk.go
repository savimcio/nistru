// Package plugsdk is the Go SDK for authoring out-of-process nistru plugins.
//
// Authors implement Plugin (or embed Base for no-op defaults) and call Run,
// which takes over stdin/stdout and drives the JSON-RPC 2.0 dialect defined
// in github.com/savimcio/nistru/plugin. Framing and dispatch are handled by
// the SDK; plugin methods are invoked synchronously on a single reader
// goroutine, so handlers must not assume concurrent calls. Long-running work
// should spawn its own goroutine and talk back to the host via *Client.
package plugsdk

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"

	"github.com/savimcio/nistru/plugin"
)

// Plugin is the contract an out-of-process plugin author implements. Every
// method is optional: embed Base to satisfy this interface with no-op
// defaults and override only what you need.
type Plugin interface {
	OnInitialize(root string, capabilities []string) error
	OnShutdown() error
	OnDidOpen(path, lang, text string)
	OnDidChange(path, text string)
	OnDidSave(path string)
	OnDidClose(path string)
	OnExecuteCommand(id string, args json.RawMessage) (result any, err error)
}

// ClientReceiver lets a plugin receive its *Client before OnInitialize runs.
// Base implements this, so embedders get the client for free.
type ClientReceiver interface {
	SetClient(*Client)
}

// ConfigReceiver is an optional extension: plugins implementing it receive
// their config sub-tree as a JSON RawMessage. OnConfig fires on every
// Initialize dispatch (once before OnInitialize on spawn, and again on every
// host reload), regardless of whether a config sub-tree is present. A nil
// raw argument means `[plugins.<name>]` is absent from the effective config;
// plugins should treat this as a reset/defaults signal. This mirrors the
// in-process plugin.ConfigReceiver contract — the host invokes both with the
// same semantics. Plugins that only need config at startup can also read it
// from the Initialize frame (the Config field), whichever is more
// convenient.
type ConfigReceiver interface {
	OnConfig(raw json.RawMessage) error
}

// Base is a zero-value Plugin with no-op defaults. Embed it to avoid
// implementing methods you do not care about. Base also satisfies
// ClientReceiver: call Base.Client() from your methods to reach the host.
type Base struct {
	client *Client
}

// SetClient records the client the SDK constructed; invoked by Run before
// OnInitialize.
func (b *Base) SetClient(c *Client) { b.client = c }

// Client returns the *Client the SDK passed in via SetClient, or nil if Run
// has not yet called SetClient.
func (b *Base) Client() *Client { return b.client }

// OnInitialize is the no-op default.
func (b *Base) OnInitialize(root string, capabilities []string) error { return nil }

// OnConfig is the no-op default; plugins that care about config override it.
func (Base) OnConfig(raw json.RawMessage) error { return nil }

// OnShutdown is the no-op default.
func (b *Base) OnShutdown() error { return nil }

// OnDidOpen is the no-op default.
func (b *Base) OnDidOpen(path, lang, text string) {}

// OnDidChange is the no-op default.
func (b *Base) OnDidChange(path, text string) {}

// OnDidSave is the no-op default.
func (b *Base) OnDidSave(path string) {}

// OnDidClose is the no-op default.
func (b *Base) OnDidClose(path string) {}

// OnExecuteCommand is the no-op default; returns nil result and no error.
func (b *Base) OnExecuteCommand(id string, args json.RawMessage) (any, error) {
	return nil, nil
}

// Client is the plugin's handle back to the host. All methods are safe for
// concurrent use: outbound writes are serialized by the underlying codec.
type Client struct {
	codec  *plugin.Codec
	nextID atomic.Uint64

	mu      sync.Mutex
	pending map[uint64]chan *plugin.Response
}

// newClient constructs a Client around a codec and initializes the pending
// response map.
func newClient(codec *plugin.Codec) *Client {
	return &Client{
		codec:   codec,
		pending: make(map[uint64]chan *plugin.Response),
	}
}

// RegisterCommand asks the host to register a command owned by this plugin.
// Sent as a notification; no response is expected.
func (c *Client) RegisterCommand(id, title string) error {
	return c.codec.WriteNotification("commands/register", struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}{ID: id, Title: title})
}

// UnregisterCommand asks the host to drop a previously registered command.
func (c *Client) UnregisterCommand(id string) error {
	return c.codec.WriteNotification("commands/unregister", struct {
		ID string `json:"id"`
	}{ID: id})
}

// SetStatusBar upserts a status-bar segment keyed by (plugin, segment). An
// empty text removes the segment.
func (c *Client) SetStatusBar(segment, text, color string) error {
	return c.codec.WriteNotification("statusBar/set", struct {
		Segment string `json:"segment"`
		Text    string `json:"text"`
		Color   string `json:"color"`
	}{Segment: segment, Text: text, Color: color})
}

// Notify asks the host to display a transient status-bar message.
func (c *Client) Notify(level, message string) error {
	return c.codec.WriteNotification("ui/notify", struct {
		Level   string `json:"level"`
		Message string `json:"message"`
	}{Level: level, Message: message})
}

// BufferEdit asks the host to replace the buffer at path with text. Blocks
// until the host responds.
func (c *Client) BufferEdit(path, text string) error {
	return c.roundTrip("buffer/edit", struct {
		Path string `json:"path"`
		Text string `json:"text"`
	}{Path: path, Text: text})
}

// OpenFile asks the host to open path in the editor. Blocks until the host
// responds.
func (c *Client) OpenFile(path string) error {
	return c.roundTrip("openFile", struct {
		Path string `json:"path"`
	}{Path: path})
}

// roundTrip sends a request, waits for the matching response, and returns
// any RPCError verbatim.
func (c *Client) roundTrip(method string, params any) error {
	id := c.nextID.Add(1)
	ch := make(chan *plugin.Response, 1)

	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.codec.WriteRequest(method, id, params); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return fmt.Errorf("plugsdk: write %s: %w", method, err)
	}

	resp, ok := <-ch
	if !ok || resp == nil {
		return fmt.Errorf("plugsdk: %s: host closed connection", method)
	}
	if resp.Error != nil {
		return resp.Error
	}
	return nil
}

// deliverResponse routes a host response to the waiter keyed by resp.ID.
// Unknown IDs are dropped with a stderr log.
func (c *Client) deliverResponse(resp *plugin.Response) {
	id, ok := toUint64(resp.ID)
	if !ok {
		fmt.Fprintf(os.Stderr, "plugsdk: response with non-numeric id %v\n", resp.ID)
		return
	}
	c.mu.Lock()
	ch, exists := c.pending[id]
	if exists {
		delete(c.pending, id)
	}
	c.mu.Unlock()
	if !exists {
		fmt.Fprintf(os.Stderr, "plugsdk: response for unknown id %d\n", id)
		return
	}
	select {
	case ch <- resp:
	default:
	}
}

// drainPending unblocks any in-flight requests with a nil response when the
// host disconnects.
func (c *Client) drainPending() {
	c.mu.Lock()
	ids := make([]uint64, 0, len(c.pending))
	for id := range c.pending {
		ids = append(ids, id)
	}
	for _, id := range ids {
		ch := c.pending[id]
		delete(c.pending, id)
		select {
		case ch <- nil:
		default:
		}
	}
	c.mu.Unlock()
}

// toUint64 accepts the two JSON encodings the codec may surface for an id
// (float64 from the generic decoder, or an integer-typed value from a
// re-encoded frame) and normalizes to uint64.
func toUint64(v any) (uint64, bool) {
	switch n := v.(type) {
	case float64:
		if n < 0 {
			return 0, false
		}
		return uint64(n), true
	case int64:
		if n < 0 {
			return 0, false
		}
		return uint64(n), true
	case uint64:
		return n, true
	case json.Number:
		i, err := n.Int64()
		if err != nil || i < 0 {
			return 0, false
		}
		return uint64(i), true
	}
	return 0, false
}

// stdio adapts stdin/stdout to io.ReadWriteCloser for plugin.NewCodec.
type stdio struct{}

func (stdio) Read(p []byte) (int, error)  { return os.Stdin.Read(p) }
func (stdio) Write(p []byte) (int, error) { return os.Stdout.Write(p) }
func (stdio) Close() error                { return nil }

// Run takes over stdin/stdout and drives the JSON-RPC loop until shutdown.
// It returns nil on clean shutdown and an error on a protocol or I/O
// failure. Plugin methods are invoked synchronously on the single reader
// goroutine.
func Run(p Plugin) error {
	return RunWith(p, stdio{})
}

// RunWith is the transport-agnostic variant of Run. It drives the JSON-RPC
// loop against rwc instead of stdin/stdout. This is the entry used by the
// plugintest harness to exercise the SDK's real dispatch path against an
// in-memory pipe. Callers outside of tests should prefer Run.
func RunWith(p Plugin, rwc io.ReadWriteCloser) error {
	codec := plugin.NewCodec(rwc)
	client := newClient(codec)
	if recv, ok := p.(ClientReceiver); ok {
		recv.SetClient(client)
	}
	defer client.drainPending()

	for {
		method, id, params, isResponse, resp, err := codec.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if isResponse {
			if resp != nil {
				client.deliverResponse(resp)
			}
			continue
		}
		if method == "shutdown" {
			shutdownErr := p.OnShutdown()
			if id != nil {
				var rpcErr *plugin.RPCError
				if shutdownErr != nil {
					rpcErr = &plugin.RPCError{
						Code:    plugin.ErrInternal,
						Message: shutdownErr.Error(),
					}
				}
				if werr := codec.WriteResponse(id, nil, rpcErr); werr != nil {
					return werr
				}
			}
			return nil
		}
		if werr := dispatch(p, codec, method, id, params); werr != nil {
			return werr
		}
	}
}

// dispatch handles one non-shutdown incoming frame. Returns only on a write
// failure; plugin-method errors for executeCommand are reported as RPC error
// responses, not returned.
func dispatch(p Plugin, codec *plugin.Codec, method string, id any, params json.RawMessage) error {
	switch method {
	case "initialize":
		var ev plugin.Initialize
		if err := json.Unmarshal(params, &ev); err != nil {
			return writeInvalidParams(codec, id, err)
		}
		// OnConfig fires before OnInitialize so receivers see config first.
		// OnConfig errors are logged but non-fatal, matching the in-proc
		// host behaviour in plugin.Host.callOnConfig. OnConfig fires on every
		// Initialize dispatch — even when ev.Config is nil — so plugins see a
		// uniform contract across initial spawn and reload. A nil raw signals
		// "no [plugins.<name>] section is present; reset to defaults". See
		// ConfigReceiver above.
		if cr, ok := p.(ConfigReceiver); ok {
			if cfgErr := cr.OnConfig(ev.Config); cfgErr != nil {
				fmt.Fprintf(os.Stderr, "plugsdk: OnConfig: %v\n", cfgErr)
			}
		}
		initErr := p.OnInitialize(ev.RootPath, ev.Capabilities)
		if id == nil {
			if initErr != nil {
				fmt.Fprintf(os.Stderr, "plugsdk: initialize: %v\n", initErr)
			}
			return nil
		}
		var rpcErr *plugin.RPCError
		if initErr != nil {
			rpcErr = &plugin.RPCError{Code: plugin.ErrInternal, Message: initErr.Error()}
		}
		return codec.WriteResponse(id, nil, rpcErr)

	case "didOpen":
		var ev plugin.DidOpen
		if err := json.Unmarshal(params, &ev); err != nil {
			fmt.Fprintf(os.Stderr, "plugsdk: didOpen: %v\n", err)
			return nil
		}
		p.OnDidOpen(ev.Path, ev.Lang, ev.Text)
		return nil

	case "didChange":
		var ev plugin.DidChange
		if err := json.Unmarshal(params, &ev); err != nil {
			fmt.Fprintf(os.Stderr, "plugsdk: didChange: %v\n", err)
			return nil
		}
		p.OnDidChange(ev.Path, ev.Text)
		return nil

	case "didSave":
		var ev plugin.DidSave
		if err := json.Unmarshal(params, &ev); err != nil {
			fmt.Fprintf(os.Stderr, "plugsdk: didSave: %v\n", err)
			return nil
		}
		p.OnDidSave(ev.Path)
		return nil

	case "didClose":
		var ev plugin.DidClose
		if err := json.Unmarshal(params, &ev); err != nil {
			fmt.Fprintf(os.Stderr, "plugsdk: didClose: %v\n", err)
			return nil
		}
		p.OnDidClose(ev.Path)
		return nil

	case "executeCommand":
		var ev plugin.ExecuteCommand
		if err := json.Unmarshal(params, &ev); err != nil {
			if id != nil {
				return writeInvalidParams(codec, id, err)
			}
			fmt.Fprintf(os.Stderr, "plugsdk: executeCommand: %v\n", err)
			return nil
		}
		result, cmdErr := p.OnExecuteCommand(ev.ID, ev.Args)
		if id == nil {
			if cmdErr != nil {
				fmt.Fprintf(os.Stderr, "plugsdk: executeCommand %s: %v\n", ev.ID, cmdErr)
			}
			return nil
		}
		if cmdErr != nil {
			return codec.WriteResponse(id, nil, &plugin.RPCError{
				Code:    plugin.ErrInternal,
				Message: cmdErr.Error(),
			})
		}
		return codec.WriteResponse(id, result, nil)

	default:
		if id == nil {
			fmt.Fprintf(os.Stderr, "plugsdk: unknown notification %q\n", method)
			return nil
		}
		return codec.WriteResponse(id, nil, &plugin.RPCError{
			Code:    plugin.ErrMethodNotFound,
			Message: "unknown method: " + method,
		})
	}
}

// writeInvalidParams emits a standard InvalidParams error response.
func writeInvalidParams(codec *plugin.Codec, id any, err error) error {
	return codec.WriteResponse(id, nil, &plugin.RPCError{
		Code:    plugin.ErrInvalidParams,
		Message: "invalid params: " + err.Error(),
	})
}
