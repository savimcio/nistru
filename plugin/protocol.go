package plugin

// JSON-RPC 2.0 codec with newline-delimited framing. Each message is exactly
// one JSON object followed by a single '\n'. The codec is safe for concurrent
// writes; reads must be serialized by a single consumer.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// Standard JSON-RPC 2.0 error codes.
const (
	ErrParse          = -32700
	ErrInvalidRequest = -32600
	ErrMethodNotFound = -32601
	ErrInvalidParams  = -32602
	ErrInternal       = -32603
)

// Request is a JSON-RPC 2.0 request or notification. ID is omitted for
// notifications and may be a number or string for requests.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	ID      any             `json:"id,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is a JSON-RPC 2.0 response. Exactly one of Result or Error is set.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// Notification is a JSON-RPC 2.0 notification (a request with no ID).
type Notification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// RPCError is a JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Error implements the error interface.
func (e *RPCError) Error() string {
	return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message)
}

// Codec is a bidirectional JSON-RPC 2.0 codec framed by newlines.
type Codec struct {
	r  *bufio.Reader
	w  io.Writer
	mu sync.Mutex
}

// NewCodec wraps a ReadWriteCloser as a JSON-RPC 2.0 codec.
func NewCodec(rw io.ReadWriteCloser) *Codec {
	return &Codec{
		r: bufio.NewReaderSize(rw, 1<<16),
		w: rw,
	}
}

// envelope is used to sniff whether an incoming frame is a request/notification
// or a response without committing to a concrete type first.
//
// Method is a pointer so the codec can distinguish field *presence* from an
// empty string. A frame like `{"jsonrpc":"2.0","method":"","id":1}` has
// Method="" (a malformed request, not a response), whereas a response omits
// the key entirely and decodes to Method=nil. Using a plain string here would
// conflate the two.
type envelope struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  *string         `json:"method,omitempty"`
	ID      any             `json:"id,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// Read decodes exactly one frame from the underlying reader. If the frame is
// a response, isResponse is true and resp is non-nil; otherwise method/id/
// params describe the incoming request or notification (id is nil for
// notifications). Responses are identified by the absence of the "method"
// key (per JSON-RPC 2.0), not by an empty-string value — a frame with
// method:"" is a malformed request, not a response.
func (c *Codec) Read() (method string, id any, params json.RawMessage, isResponse bool, resp *Response, err error) {
	line, readErr := c.r.ReadBytes('\n')
	if readErr != nil && len(line) == 0 {
		err = readErr
		return
	}
	var env envelope
	if jerr := json.Unmarshal(line, &env); jerr != nil {
		err = fmt.Errorf("jsonrpc: parse frame: %w", jerr)
		return
	}
	// JSON-RPC 2.0: responses have no "method"; requests and notifications
	// always do. Sniffing on Result/Error would misclassify a response with a
	// null/absent result (e.g. Initialize returning no payload) as a
	// notification. env.Method is a *string so nil means "absent" rather than
	// "empty string"; a frame with method:"" is a malformed request, still
	// routed down the request path.
	if env.Method == nil {
		isResponse = true
		resp = &Response{
			JSONRPC: env.JSONRPC,
			ID:      env.ID,
			Result:  env.Result,
			Error:   env.Error,
		}
		return
	}
	method = *env.Method
	id = env.ID
	params = env.Params
	return
}

// WriteRequest writes a JSON-RPC 2.0 request with the given method, id, and
// params. Params are marshaled with encoding/json.
func (c *Codec) WriteRequest(method string, id any, params any) error {
	raw, err := marshalParams(params)
	if err != nil {
		return err
	}
	return c.writeFrame(Request{
		JSONRPC: "2.0",
		Method:  method,
		ID:      id,
		Params:  raw,
	})
}

// WriteNotification writes a JSON-RPC 2.0 notification (no id).
func (c *Codec) WriteNotification(method string, params any) error {
	raw, err := marshalParams(params)
	if err != nil {
		return err
	}
	return c.writeFrame(Notification{
		JSONRPC: "2.0",
		Method:  method,
		Params:  raw,
	})
}

// WriteResponse writes a JSON-RPC 2.0 response. Pass a non-nil rpcErr to
// emit an error response; otherwise result is marshaled into the Result
// field.
func (c *Codec) WriteResponse(id any, result any, rpcErr *RPCError) error {
	resp := Response{
		JSONRPC: "2.0",
		ID:      id,
	}
	if rpcErr != nil {
		resp.Error = rpcErr
	} else {
		raw, err := marshalParams(result)
		if err != nil {
			return err
		}
		resp.Result = raw
	}
	return c.writeFrame(resp)
}

func marshalParams(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	if raw, ok := v.(json.RawMessage); ok {
		return raw, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("jsonrpc: marshal params: %w", err)
	}
	return b, nil
}

func (c *Codec) writeFrame(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("jsonrpc: marshal frame: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.w.Write(b); err != nil {
		return err
	}
	if _, err := c.w.Write([]byte{'\n'}); err != nil {
		return err
	}
	return nil
}
