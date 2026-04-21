package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// rwc glues an io.Reader and io.Writer into an io.ReadWriteCloser for the
// codec's constructor. Closing tears down both endpoints.
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

// pipePair returns two Codecs where A's writes are readable by B and vice
// versa, each backed by its own io.Pipe.
func pipePair() (a, b *Codec, cleanup func()) {
	ar, aw := io.Pipe() // a reads from this; b writes to this
	br, bw := io.Pipe() // b reads from this; a writes to this
	a = NewCodec(&rwc{r: ar, w: bw})
	b = NewCodec(&rwc{r: br, w: aw})
	cleanup = func() {
		_ = ar.Close()
		_ = aw.Close()
		_ = br.Close()
		_ = bw.Close()
	}
	return
}

func TestCodec_RequestRoundTrip(t *testing.T) {
	a, b, cleanup := pipePair()
	defer cleanup()

	type params struct {
		X int    `json:"x"`
		Y string `json:"y"`
	}
	go func() {
		if err := a.WriteRequest("doThing", 42, params{X: 7, Y: "hi"}); err != nil {
			t.Errorf("WriteRequest: %v", err)
		}
	}()

	method, id, raw, isResp, resp, err := b.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if isResp {
		t.Fatalf("expected request, got response: %+v", resp)
	}
	if method != "doThing" {
		t.Fatalf("method = %q, want doThing", method)
	}
	// JSON numbers decode into float64 by default.
	if f, ok := id.(float64); !ok || int(f) != 42 {
		t.Fatalf("id = %v (%T), want 42", id, id)
	}
	var got params
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}
	if got.X != 7 || got.Y != "hi" {
		t.Fatalf("params = %+v, want {7 hi}", got)
	}
}

func TestCodec_NotificationRoundTrip(t *testing.T) {
	a, b, cleanup := pipePair()
	defer cleanup()

	go func() {
		if err := a.WriteNotification("hello", map[string]string{"k": "v"}); err != nil {
			t.Errorf("WriteNotification: %v", err)
		}
	}()

	method, id, _, isResp, _, err := b.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if isResp {
		t.Fatalf("expected notification, got response")
	}
	if method != "hello" {
		t.Fatalf("method = %q, want hello", method)
	}
	if id != nil {
		t.Fatalf("notification id = %v, want nil", id)
	}
}

func TestCodec_ResponseRoundTrip(t *testing.T) {
	a, b, cleanup := pipePair()
	defer cleanup()

	go func() {
		if err := a.WriteResponse(1, map[string]int{"n": 9}, nil); err != nil {
			t.Errorf("WriteResponse: %v", err)
		}
	}()

	_, _, _, isResp, resp, err := b.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !isResp || resp == nil {
		t.Fatalf("expected response, got request")
	}
	var got map[string]int
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if got["n"] != 9 {
		t.Fatalf("result n = %d, want 9", got["n"])
	}
}

func TestCodec_NullResultResponseRoundTrip(t *testing.T) {
	// Regression: a response with a nil result and no error (e.g. Initialize
	// returning no payload) must be classified as a response, not a
	// notification. Sniffing on Result/Error is insufficient because both
	// elide with omitempty on the wire; JSON-RPC 2.0 responses are
	// identified by the absence of "method".
	a, b, cleanup := pipePair()
	defer cleanup()

	go func() {
		if err := a.WriteResponse(42, nil, nil); err != nil {
			t.Errorf("WriteResponse: %v", err)
		}
	}()

	method, id, _, isResp, resp, err := b.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !isResp || resp == nil {
		t.Fatalf("expected response, got request/notification (method=%q id=%v)", method, id)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	if resp.Result != nil {
		t.Fatalf("expected nil result, got %s", string(resp.Result))
	}
}

func TestCodec_EmptyMethodIsRequestPath(t *testing.T) {
	// Regression: a frame with an explicit empty-string method is a malformed
	// request, NOT a response. The codec must classify it as a request (so
	// the host can reject it via the normal invalid-method path) rather than
	// routing it to the response channel. This is the contract the envelope's
	// *string Method field protects — presence vs empty-string.
	r, w := io.Pipe()
	defer r.Close()

	reader := NewCodec(&rwc{r: r, w: io.Discard})

	go func() {
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","method":"","id":1}` + "\n"))
		_ = w.Close()
	}()

	method, _, _, isResp, resp, err := reader.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if isResp {
		t.Fatalf("frame with method:\"\" must be classified as a request, got response %+v", resp)
	}
	if method != "" {
		t.Fatalf("method = %q, want empty string", method)
	}
}

func TestCodec_ErrorResponseRoundTrip(t *testing.T) {
	a, b, cleanup := pipePair()
	defer cleanup()

	rpcErr := &RPCError{Code: ErrMethodNotFound, Message: "nope"}
	go func() {
		if err := a.WriteResponse("id-7", nil, rpcErr); err != nil {
			t.Errorf("WriteResponse: %v", err)
		}
	}()

	_, _, _, isResp, resp, err := b.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !isResp || resp == nil {
		t.Fatalf("expected response")
	}
	if resp.Error == nil || resp.Error.Code != ErrMethodNotFound || resp.Error.Message != "nope" {
		t.Fatalf("error = %+v, want MethodNotFound/nope", resp.Error)
	}
}

func TestCodec_RawMessageParamsPassThrough(t *testing.T) {
	a, b, cleanup := pipePair()
	defer cleanup()

	raw := json.RawMessage(`{"arbitrary":[1,2,3]}`)
	go func() {
		if err := a.WriteNotification("passthrough", raw); err != nil {
			t.Errorf("WriteNotification: %v", err)
		}
	}()

	_, _, params, isResp, _, err := b.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if isResp {
		t.Fatalf("expected notification")
	}
	if string(params) != string(raw) {
		t.Fatalf("params = %s, want %s", params, raw)
	}
}

// byteByByteWriter decorates an io.Writer so each call emits only one byte at
// a time. It forces the codec's bufio.Reader to perform multiple reads before
// seeing the framing newline.
type byteByByteWriter struct{ w io.Writer }

func (b *byteByByteWriter) Write(p []byte) (int, error) {
	total := 0
	for _, c := range p {
		n, err := b.w.Write([]byte{c})
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func TestCodec_PartialReads(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close()
	defer w.Close()

	reader := NewCodec(&rwc{r: r, w: io.Discard})
	writer := NewCodec(&rwc{r: strings.NewReader(""), w: &byteByByteWriter{w: w}})

	go func() {
		if err := writer.WriteRequest("slow", 1, map[string]int{"n": 2}); err != nil {
			t.Errorf("WriteRequest: %v", err)
		}
	}()

	method, _, _, _, _, err := reader.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if method != "slow" {
		t.Fatalf("method = %q, want slow", method)
	}
}

func TestCodec_MalformedJSONFrame(t *testing.T) {
	r, w := io.Pipe()
	defer r.Close()

	reader := NewCodec(&rwc{r: r, w: io.Discard})

	go func() {
		_, _ = w.Write([]byte("not-json{\n"))
		_ = w.Close()
	}()

	_, _, _, _, _, err := reader.Read()
	if err == nil {
		t.Fatalf("Read: expected error on malformed JSON")
	}
	// A clean EOF is not the error we want — we want a parse error.
	if errors.Is(err, io.EOF) {
		t.Fatalf("Read: got EOF, want parse error")
	}
}

func TestCodec_ConcurrentWrites(t *testing.T) {
	a, b, cleanup := pipePair()
	defer cleanup()

	const N = 100

	// Writer goroutines fire in parallel.
	var wg sync.WaitGroup
	wg.Add(N)
	for i := range N {
		go func() {
			defer wg.Done()
			method := fmt.Sprintf("m%d", i)
			if err := a.WriteNotification(method, map[string]int{"i": i}); err != nil {
				t.Errorf("WriteNotification: %v", err)
			}
		}()
	}

	// Reader loop in foreground with a timeout.
	seen := make(map[string]bool, N)
	done := make(chan struct{})
	var readErr error
	go func() {
		defer close(done)
		for range N {
			method, _, _, isResp, _, err := b.Read()
			if err != nil {
				readErr = err
				return
			}
			if isResp {
				readErr = fmt.Errorf("got response, expected notification")
				return
			}
			seen[method] = true
		}
	}()

	wg.Wait()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("reader did not finish in time; seen=%d", len(seen))
	}
	if readErr != nil {
		t.Fatalf("reader error: %v", readErr)
	}
	if len(seen) != N {
		t.Fatalf("got %d unique methods, want %d", len(seen), N)
	}
	for i := range N {
		m := fmt.Sprintf("m%d", i)
		if !seen[m] {
			t.Fatalf("missing method %q", m)
		}
	}
}

func TestRPCError_ErrorString(t *testing.T) {
	e := &RPCError{Code: -32601, Message: "method not found"}
	if got := e.Error(); !strings.Contains(got, "method not found") {
		t.Fatalf("Error() = %q, missing message", got)
	}
}
