package plugin

// Out-of-process plugin lifecycle: spawn, stdio framing, reader and writer
// goroutines, coalescing latest-wins DidChange-per-path, and shutdown.
//
// Each extPlugin owns three goroutines for its lifetime:
//   - reader:  decodes frames from plugin stdout and forwards them either
//              to pending-response waiters or to the host inbound channel.
//   - writer:  serializes outgoing frames, applying DidChange coalescing.
//   - stderr:  tails the plugin's stderr into a fixed-size ring buffer
//              used to enrich PluginExitedMsg on abnormal exit.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// writesCap is the bound on the per-plugin writer queue for "other" frames
// (notifications and requests besides DidChange, plus responses).
const writesCap = 64

// stderrCap is the size of the rolling stderr buffer per plugin.
const stderrCap = 16 * 1024

// initializeTimeout bounds how long spawn waits for the Initialize response.
const initializeTimeout = 2 * time.Second

// outFrame is a single unit of work pushed to a plugin's writer goroutine.
// isNotif distinguishes notifications from requests. For responses, id is
// set and method is empty; result/params live in params and rpcErr is the
// error slot for responses.
type outFrame struct {
	isNotif bool
	isResp  bool
	method  string
	id      any
	params  any
	rpcErr  *RPCError
}

// extPlugin is the host-side handle for one running out-of-process plugin.
type extPlugin struct {
	name    string
	cmd     *exec.Cmd
	codec   *Codec
	writes  chan outFrame
	pending sync.Map // id -> chan *Response

	// pendingChange holds the latest coalesced DidChange frame per path.
	// The writer drains this before returning to block on writes.
	changeMu      sync.Mutex
	pendingChange map[string]outFrame
	changeSignal  chan struct{}

	stderrBuf *ringBuf
	done      chan struct{}
	cancel    context.CancelFunc

	closeOnce sync.Once
	closed    atomic.Bool
}

// ringBuf is a fixed-capacity byte ring buffer. It is safe for concurrent
// use by a single writer and a single reader via its own mutex.
type ringBuf struct {
	mu   sync.Mutex
	buf  []byte
	size int
}

func newRingBuf(cap int) *ringBuf { return &ringBuf{buf: make([]byte, 0, cap), size: cap} }

// Write appends p to the buffer, dropping the oldest bytes to stay within
// capacity. Always returns len(p), nil.
func (r *ringBuf) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(p) >= r.size {
		r.buf = append(r.buf[:0], p[len(p)-r.size:]...)
		return len(p), nil
	}
	if len(r.buf)+len(p) <= r.size {
		r.buf = append(r.buf, p...)
		return len(p), nil
	}
	overflow := len(r.buf) + len(p) - r.size
	r.buf = append(r.buf[:0], r.buf[overflow:]...)
	r.buf = append(r.buf, p...)
	return len(p), nil
}

// Bytes returns a copy of the buffer's current contents.
func (r *ringBuf) Bytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]byte, len(r.buf))
	copy(out, r.buf)
	return out
}

// spawn launches the plugin process described by m, wires up pipes and
// goroutines, performs the Initialize handshake, and registers the result
// in Host.running on success.
func (h *Host) spawn(m *Manifest) error {
	h.mu.RLock()
	if _, ok := h.running[m.Name]; ok {
		h.mu.RUnlock()
		return nil
	}
	rootPath := h.rootPath
	h.mu.RUnlock()

	if len(m.Cmd) == 0 {
		return fmt.Errorf("plugin: %s: empty cmd", m.Name)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, m.Cmd[0], m.Cmd[1:]...)
	// Resolve the plugin's manifest dir as Dir so relative paths inside the
	// plugin resolve correctly. We treat the manifest's first argv as the
	// plugin binary; its directory is derived from the rootPath + name
	// convention but we fall back to CWD if nothing else is usable.
	if dir := manifestDir(rootPath, m); dir != "" {
		cmd.Dir = dir
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("plugin: %s: stdin: %w", m.Name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("plugin: %s: stdout: %w", m.Name, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return fmt.Errorf("plugin: %s: stderr: %w", m.Name, err)
	}

	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("plugin: %s: start: %w", m.Name, err)
	}

	ext := &extPlugin{
		name:          m.Name,
		cmd:           cmd,
		codec:         NewCodec(stdioPipe{r: stdout, w: stdin}),
		writes:        make(chan outFrame, writesCap),
		pendingChange: make(map[string]outFrame),
		changeSignal:  make(chan struct{}, 1),
		stderrBuf:     newRingBuf(stderrCap),
		done:          make(chan struct{}),
		cancel:        cancel,
	}

	// stderr tail.
	go func() {
		_, _ = io.Copy(ext.stderrBuf, stderr)
	}()

	// Register before the handshake so reader/waiter paths can find it.
	h.mu.Lock()
	h.running[m.Name] = ext
	h.mu.Unlock()

	// Reader and writer goroutines.
	go h.readerLoop(ext)
	go h.writerLoop(ext)

	// Initialize handshake.
	initID := h.nextID()
	respCh := make(chan *Response, 1)
	ext.pending.Store(normalizeID(initID), respCh)
	ext.enqueue(outFrame{
		isNotif: false,
		method:  "initialize",
		id:      initID,
		params: Initialize{
			RootPath:     rootPath,
			Capabilities: hostCapabilities,
		},
	})

	select {
	case resp := <-respCh:
		if resp == nil {
			h.cleanupExt(m.Name)
			return fmt.Errorf("plugin: %s: initialize: process exited", m.Name)
		}
		if resp.Error != nil {
			h.cleanupExt(m.Name)
			return fmt.Errorf("plugin: %s: initialize: %s", m.Name, resp.Error.Error())
		}
	case <-time.After(initializeTimeout):
		ext.pending.Delete(normalizeID(initID))
		h.cleanupExt(m.Name)
		return fmt.Errorf("plugin: %s: initialize timed out", m.Name)
	}

	// Announce start.
	select {
	case h.inbound <- PluginStartedMsg{Name: m.Name}:
	default:
		// Inbound channel full; drop the started notification rather than
		// blocking spawn.
	}

	// Watcher: wait for the process to exit, push PluginExitedMsg.
	go h.waitLoop(ext)

	return nil
}

// manifestDir returns the on-disk directory for manifest m if it can be
// determined, or "" otherwise. The host's discovery logic stores plugins
// under <root>/.nistru/plugins/<name> or <userConfig>/nistru/plugins/<name>;
// we search those two roots by name.
func manifestDir(rootPath string, m *Manifest) string {
	candidates := []string{filepath.Join(rootPath, ".nistru", "plugins", m.Name)}
	if user, err := os.UserConfigDir(); err == nil {
		candidates = append(candidates, filepath.Join(user, "nistru", "plugins", m.Name))
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			return c
		}
	}
	return ""
}

// stdioPipe adapts an io.ReadCloser + io.WriteCloser pair to an
// io.ReadWriteCloser so NewCodec can wrap both ends of the plugin's stdio.
type stdioPipe struct {
	r io.ReadCloser
	w io.WriteCloser
}

func (s stdioPipe) Read(p []byte) (int, error)  { return s.r.Read(p) }
func (s stdioPipe) Write(p []byte) (int, error) { return s.w.Write(p) }
func (s stdioPipe) Close() error {
	rerr := s.r.Close()
	werr := s.w.Close()
	if werr != nil {
		return werr
	}
	return rerr
}

// enqueue pushes a generic frame onto the writer, falling back to a dropped
// warning if the queue is full.
func (e *extPlugin) enqueue(f outFrame) {
	if e.closed.Load() {
		return
	}
	select {
	case e.writes <- f:
	default:
		fmt.Fprintf(os.Stderr, "plugin: %s: writer queue full, dropping %s\n", e.name, describeFrame(f))
	}
}

// enqueueResponse is a convenience wrapper for response frames.
func (e *extPlugin) enqueueResponse(id any, result any, rpcErr *RPCError) {
	e.enqueue(outFrame{isResp: true, id: id, params: result, rpcErr: rpcErr})
}

// sendEvent routes an outgoing event frame, applying DidChange coalescing.
// For DidChange, the latest frame per path replaces any prior pending one
// and signals the writer via a one-slot changeSignal channel. For every
// other event type, the frame goes on the bounded writes channel.
func (e *extPlugin) sendEvent(method string, params any, raw any) {
	if e.closed.Load() {
		return
	}
	if method == "didChange" {
		dc, ok := raw.(DidChange)
		if ok {
			e.changeMu.Lock()
			e.pendingChange[dc.Path] = outFrame{
				isNotif: true,
				method:  method,
				params:  params,
			}
			e.changeMu.Unlock()
			select {
			case e.changeSignal <- struct{}{}:
			default:
			}
			return
		}
	}
	e.enqueue(outFrame{isNotif: true, method: method, params: params})
}

// closeWrites closes the writer channel and cancels any pending changes.
// Idempotent.
func (e *extPlugin) closeWrites() {
	e.closeOnce.Do(func() {
		e.closed.Store(true)
		close(e.writes)
		// Signal the writer to drain final coalesced changes.
		select {
		case e.changeSignal <- struct{}{}:
		default:
		}
	})
}

// waitFor blocks up to timeout for the process to exit. Returns true if it
// exited within the deadline.
func (e *extPlugin) waitFor(timeout time.Duration) bool {
	select {
	case <-e.done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// kill forcibly terminates the plugin process and cancels its context.
func (e *extPlugin) kill() error {
	e.cancel()
	if e.cmd.Process != nil {
		return e.cmd.Process.Kill()
	}
	return nil
}

// cleanupExt tears down the registered extPlugin without surfacing an
// exit message to the inbound channel. Used on spawn-time failures.
func (h *Host) cleanupExt(name string) {
	h.mu.Lock()
	ext, ok := h.running[name]
	if ok {
		delete(h.running, name)
	}
	h.mu.Unlock()
	if !ok {
		return
	}
	ext.closeWrites()
	ext.cancel()
	_ = ext.kill()
}

// readerLoop decodes frames from the plugin's stdout and routes them either
// to pending-response waiters or to the host inbound channel. Runs until
// the codec returns io.EOF or an error.
func (h *Host) readerLoop(ext *extPlugin) {
	for {
		method, id, params, isResponse, resp, err := ext.codec.Read()
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrClosed) {
				fmt.Fprintf(os.Stderr, "plugin: %s: read: %v\n", ext.name, err)
			}
			return
		}
		if isResponse {
			if resp == nil {
				continue
			}
			if ch, ok := ext.pending.LoadAndDelete(normalizeID(resp.ID)); ok {
				respCh := ch.(chan *Response)
				select {
				case respCh <- resp:
				default:
				}
			}
			continue
		}
		// Request (has id) or notification (id == nil).
		if id == nil {
			h.deliver(PluginNotifMsg{Plugin: ext.name, Method: method, Params: params})
		} else {
			h.deliver(PluginReqMsg{Plugin: ext.name, ID: id, Method: method, Params: params})
		}
	}
}

// deliver pushes a message onto the host inbound channel, logging and
// dropping if it would block. 256 slots is expected to be sufficient; a
// drop here indicates a stuck consumer.
func (h *Host) deliver(msg PluginMsg) {
	select {
	case h.inbound <- msg:
	default:
		fmt.Fprintf(os.Stderr, "plugin: inbound full, dropping %T\n", msg)
	}
}

// writerLoop drains outgoing frames and coalesced DidChange entries to the
// plugin. Terminates when writes is closed and no pending changes remain.
func (h *Host) writerLoop(ext *extPlugin) {
	for {
		// Flush any coalesced DidChange entries first.
		ext.flushChanges()

		select {
		case f, ok := <-ext.writes:
			if !ok {
				// writes closed; drain any final coalesced changes then exit.
				ext.flushChanges()
				return
			}
			if err := writeFrame(ext.codec, f); err != nil {
				fmt.Fprintf(os.Stderr, "plugin: %s: write: %v\n", ext.name, err)
				ext.cancel()
				return
			}
		case <-ext.changeSignal:
			// Loop around to flush.
		}
	}
}

// flushChanges drains pendingChange and writes each entry. Safe to call
// from the writer goroutine only.
func (e *extPlugin) flushChanges() {
	e.changeMu.Lock()
	if len(e.pendingChange) == 0 {
		e.changeMu.Unlock()
		return
	}
	pending := e.pendingChange
	e.pendingChange = make(map[string]outFrame)
	e.changeMu.Unlock()
	for _, f := range pending {
		if err := writeFrame(e.codec, f); err != nil {
			fmt.Fprintf(os.Stderr, "plugin: %s: write didChange: %v\n", e.name, err)
			e.cancel()
			return
		}
	}
}

// writeFrame dispatches f to the appropriate codec method.
func writeFrame(c *Codec, f outFrame) error {
	switch {
	case f.isResp:
		return c.WriteResponse(f.id, f.params, f.rpcErr)
	case f.isNotif:
		return c.WriteNotification(f.method, f.params)
	default:
		return c.WriteRequest(f.method, f.id, f.params)
	}
}

// waitLoop waits for the plugin process to exit, then pushes
// PluginExitedMsg and cleans up.
func (h *Host) waitLoop(ext *extPlugin) {
	err := ext.cmd.Wait()
	close(ext.done)

	// Drain pending waiters with a nil response to unblock callers.
	ext.pending.Range(func(key, value any) bool {
		if ch, ok := value.(chan *Response); ok {
			select {
			case ch <- nil:
			default:
			}
		}
		ext.pending.Delete(key)
		return true
	})

	if err != nil {
		tail := ext.stderrBuf.Bytes()
		if len(tail) > 0 {
			err = fmt.Errorf("%w: stderr: %s", err, trimRight(tail))
		}
	}

	h.deliver(PluginExitedMsg{Name: ext.name, Err: err})
}

// trimRight strips trailing newline bytes so stderr tails render cleanly.
func trimRight(b []byte) string {
	return string(bytes.TrimRight(b, "\r\n"))
}

// normalizeID collapses numeric JSON-RPC id variants to a single canonical
// form so sync.Map lookups succeed regardless of whether the id came from a
// Go int (stored at write time) or a JSON-decoded float64 (observed at read
// time). String ids are passed through unchanged.
func normalizeID(id any) any {
	switch v := id.(type) {
	case nil:
		return nil
	case float64:
		if v == float64(int64(v)) {
			return int64(v)
		}
		return v
	case float32:
		if v == float32(int64(v)) {
			return int64(v)
		}
		return v
	case int:
		return int64(v)
	case int32:
		return int64(v)
	case int64:
		return v
	case uint:
		return int64(v)
	case uint32:
		return int64(v)
	case uint64:
		return int64(v)
	default:
		return id
	}
}

// describeFrame formats an outFrame for diagnostic logging.
func describeFrame(f outFrame) string {
	if f.isResp {
		return fmt.Sprintf("response(%v)", f.id)
	}
	if f.isNotif {
		return fmt.Sprintf("notif(%s)", f.method)
	}
	return fmt.Sprintf("request(%s)", f.method)
}
