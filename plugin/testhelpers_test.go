package plugin

// Shared test fixtures and helpers for the plugin package. Moved out of
// host_test.go so new *_test.go files can reuse them.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// -----------------------------------------------------------------------------
// In-proc fakes.

// fakeInProcPlugin implements plugin.Plugin and records every event it saw.
type fakeInProcPlugin struct {
	name   string
	acts   []string
	events []any
	effs   []Effect // returned from OnEvent
	panic  bool
	// paneSlot is non-empty when the plugin should also satisfy Pane.
	paneSlot string
	keys     []KeyEvent
}

func (f *fakeInProcPlugin) Name() string         { return f.name }
func (f *fakeInProcPlugin) Activation() []string { return f.acts }
func (f *fakeInProcPlugin) OnEvent(ev any) []Effect {
	if f.panic {
		panic("boom")
	}
	f.events = append(f.events, ev)
	return f.effs
}
func (f *fakeInProcPlugin) Shutdown() error { return nil }

// fakePane extends fakeInProcPlugin with Pane.
type fakePane struct {
	fakeInProcPlugin
	rendered string
}

func (p *fakePane) Render(w, h int) string { return p.rendered }
func (p *fakePane) OnResize(w, h int)      {}
func (p *fakePane) OnFocus(focused bool)   {}
func (p *fakePane) Slot() string           { return p.paneSlot }
func (p *fakePane) HandleKey(k KeyEvent) []Effect {
	p.keys = append(p.keys, k)
	return p.effs
}

// newHostWithPlugins builds a Host with the given in-proc plugins registered
// and Start()ed. Panics on Start failure so tests stay terse.
func newHostWithPlugins(plugins ...Plugin) *Host {
	reg := NewRegistry()
	for _, p := range plugins {
		reg.RegisterInProc(p)
	}
	h := NewHost(reg)
	if err := h.Start(""); err != nil {
		panic(err)
	}
	return h
}

// -----------------------------------------------------------------------------
// Self-spawn harness for out-of-proc tests.

// runAsPlugin implements a minimal JSON-RPC 2.0 peer on stdin/stdout. It
// honours PLUGIN_MODE to fake scenarios the host wants to observe. Invoked
// from TestMain when PLUGIN_MODE is set in the child process env.
func runAsPlugin(mode string) {
	codec := NewCodec(&rwc{r: os.Stdin, w: os.Stdout})

	// Counter for didChange frames, so the host can check coalescing.
	var changes int64

	// Record-file sink (for flood_didchange). Written lazily on each didChange.
	recordPath := os.Getenv("PLUGIN_RECORD")

	// Stderr overflow mode: emit a large burst of stderr on startup so the
	// host-side ring buffer is exercised. The size is controlled via the
	// PLUGIN_STDERR_BYTES env var so the test controls exactly how much to
	// overflow. We then sit idle until stdin closes.
	if mode == "stderr_overflow" {
		if n, err := strconv.Atoi(os.Getenv("PLUGIN_STDERR_BYTES")); err == nil && n > 0 {
			// Build a byte pattern including a byte index so the host can
			// identify which tail slice was kept. We write in chunks so the
			// Go runtime doesn't buffer it into a single read.
			chunk := make([]byte, 1024)
			for i := range chunk {
				chunk[i] = 'x'
			}
			written := 0
			for written < n {
				w := len(chunk)
				if written+w > n {
					w = n - written
				}
				// Last chunk carries a sentinel so the host can confirm tail.
				if written+w >= n {
					tail := fmt.Sprintf("TAIL-%d", n)
					if w >= len(tail) {
						copy(chunk[w-len(tail):w], []byte(tail))
					}
				}
				_, _ = os.Stderr.Write(chunk[:w])
				written += w
			}
		}
	}

	for {
		method, id, params, isResp, _, err := codec.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				os.Exit(0)
			}
			os.Exit(2)
		}
		if isResp {
			continue
		}
		switch method {
		case "initialize":
			switch mode {
			case "slow_init":
				// Intentionally never respond, so the host's initialize
				// timeout (2s) fires. Blocking here is free — we do not
				// actually sleep; the test tears us down via ctx cancel or
				// closeWrites as part of cleanupExt.
				select {}
			default:
				_ = codec.WriteResponse(id, map[string]any{}, nil)
			}
			if mode == "notify_hello" {
				_ = codec.WriteNotification("ui/notify", map[string]string{
					"level":   "info",
					"message": "hello-from-plugin",
				})
			}
		case "didOpen":
			if mode == "crash_on_didopen" {
				os.Exit(1)
			}
		case "didChange":
			n := atomic.AddInt64(&changes, 1)
			if mode == "flood_didchange" && recordPath != "" {
				// Parse the DidChange to pull its Text; the host sends values
				// like "change-999" so the final count matches.
				var dc DidChange
				if err := json.Unmarshal(params, &dc); err == nil {
					// Rewrite the record file with the latest seen count+text
					// on every frame so the test can inspect the tail.
					_ = os.WriteFile(recordPath, fmt.Appendf(nil, "%d|%s", n, dc.Text), 0o644)
				}
			}
		case "shutdown":
			if mode == "hang" {
				// Ignore shutdown; block until kill.
				select {}
			}
			os.Exit(0)
		case "executeCommand":
			var ec ExecuteCommand
			_ = json.Unmarshal(params, &ec)
			_ = codec.WriteResponse(id, map[string]string{
				"ok": "1",
				"id": ec.ID,
			}, nil)
		}
	}
}

// writePluginManifest writes a manifest that points at the test binary. The
// child inherits PLUGIN_MODE via env, and the manifest lives under the root
// path's .nistru/plugins/<name>/plugin.json so host.manifestDir finds its Dir.
func writePluginManifest(t *testing.T, rootPath, name, mode string, extraEnv map[string]string) *Manifest {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	pluginDir := filepath.Join(rootPath, ".nistru", "plugins", name)
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatalf("mkdir plugin dir: %v", err)
	}

	// Wrapper script: exec the test binary with PLUGIN_MODE set. Using a
	// shell wrapper keeps manifest.Cmd language-agnostic while letting us
	// pass per-test env into the child.
	wrapper := filepath.Join(pluginDir, "run.sh")
	var envLines strings.Builder
	fmt.Fprintf(&envLines, "export PLUGIN_MODE=%s\n", shellQuote(mode))
	for k, v := range extraEnv {
		fmt.Fprintf(&envLines, "export %s=%s\n", k, shellQuote(v))
	}
	script := "#!/bin/sh\n" + envLines.String() + fmt.Sprintf("exec %s -test.run=DoesNotExistXXX\n", shellQuote(exe))
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatalf("write wrapper: %v", err)
	}

	m := &Manifest{
		Name:       name,
		Version:    "0.0.1",
		Cmd:        []string{wrapper},
		Activation: []string{"onLanguage:go"},
	}
	return m
}

func shellQuote(s string) string {
	// Minimal POSIX shell single-quote escape.
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// newHostWithManifest sets up a host whose registry contains only the given
// external manifest. Returns the host and its root directory (so the caller
// can read PLUGIN_RECORD files, etc.).
func newHostWithManifest(t *testing.T, mode string, extraEnv map[string]string) (*Host, string, *Manifest) {
	t.Helper()
	return newHostWithNamedManifest(t, "fake", mode, extraEnv)
}

// newHostWithNamedManifest is the name-parameterized form of
// newHostWithManifest — needed when a test wants >1 plugin registered.
func newHostWithNamedManifest(t *testing.T, name, mode string, extraEnv map[string]string) (*Host, string, *Manifest) {
	t.Helper()
	root := t.TempDir()
	m := writePluginManifest(t, root, name, mode, extraEnv)
	reg := NewRegistry()
	reg.manifests = append(reg.manifests, m)
	h := NewHost(reg)
	if err := h.Start(root); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return h, root, m
}

// recv blocks up to timeout for an inbound PluginMsg from h.
func recv(t *testing.T, h *Host, timeout time.Duration) PluginMsg {
	t.Helper()
	select {
	case msg := <-h.inbound:
		h.handleInternal(msg)
		return msg
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for plugin msg after %s", timeout)
		return nil
	}
}

// waitForStarted drains inbound messages until a PluginStartedMsg for the
// given name is seen, or the deadline expires. Other messages are consumed
// via handleInternal so host bookkeeping stays consistent.
func waitForStarted(t *testing.T, h *Host, name string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case msg := <-h.inbound:
			h.handleInternal(msg)
			if s, ok := msg.(PluginStartedMsg); ok && s.Name == name {
				return
			}
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Fatalf("plugin %q never started within %s", name, timeout)
}

// parseRecord parses "<count>|<text>" written by runAsPlugin's flood_didchange
// mode. Returns zero/empty on malformed input.
func parseRecord(s string) (int64, string) {
	prefix, text, ok := strings.Cut(s, "|")
	if !ok {
		return 0, ""
	}
	n, _ := strconv.ParseInt(prefix, 10, 64)
	return n, text
}

// waitUntil polls pred until it returns true or the timeout elapses. Fails
// the test with an attempt count + elapsed time on timeout. Under
// testing.Short(), the timeout is halved so the fast suite stays fast. Poll
// interval is 20ms.
func waitUntil(t *testing.T, timeout time.Duration, pred func() bool) {
	t.Helper()
	if testing.Short() {
		timeout = timeout / 2
	}
	const pollEvery = 20 * time.Millisecond
	start := time.Now()
	deadline := start.Add(timeout)
	attempts := 0
	for time.Now().Before(deadline) {
		attempts++
		if pred() {
			return
		}
		time.Sleep(pollEvery)
	}
	// One final attempt so we don't miss a true flip in the last interval.
	attempts++
	if pred() {
		return
	}
	t.Fatalf("waitUntil: condition not met after %d attempts over %s (timeout %s)",
		attempts, time.Since(start).Round(time.Millisecond), timeout)
}
