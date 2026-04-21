package plugin

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestHost_ExecuteCommand_OutOfProc_RoundTrip drives two overlapping
// out-of-proc executeCommand calls and asserts that their responses are
// routed back to the right waiter by request ID.
func TestHost_ExecuteCommand_OutOfProc_RoundTrip(t *testing.T) {
	h, _, _ := newHostWithManifest(t, "ok", nil)
	defer h.Shutdown(time.Second)

	// Bring the plugin online and register two commands it owns.
	h.Emit(DidOpen{Path: "/a.go", Lang: "go", Text: ""})
	waitForStarted(t, h, "fake", 5*time.Second)
	h.registerCommand("fake", "cmdA", "A")
	h.registerCommand("fake", "cmdB", "B")

	args := func(k, v string) json.RawMessage {
		b, _ := json.Marshal(map[string]string{k: v})
		return b
	}
	a := h.ExecuteCommand("cmdA", args("id", "A"))
	b := h.ExecuteCommand("cmdB", args("id", "B"))
	if a.Async == nil || b.Async == nil {
		t.Fatalf("expected async results; got sync (a=%+v b=%+v)", a.Sync, b.Sync)
	}

	// Resolve both asynchronously to stress ID matching.
	type out struct {
		name string
		msg  PluginResponseMsg
	}
	ch := make(chan out, 2)
	go func() { ch <- out{"a", a.Async().(PluginResponseMsg)} }()
	go func() { ch <- out{"b", b.Async().(PluginResponseMsg)} }()
	deadline := time.After(5 * time.Second)
	seen := map[string]PluginResponseMsg{}
	for len(seen) < 2 {
		select {
		case o := <-ch:
			seen[o.name] = o.msg
		case <-deadline:
			t.Fatalf("timed out waiting for responses (got %d/2)", len(seen))
		}
	}
	if seen["a"].Err != nil || seen["b"].Err != nil {
		t.Fatalf("unexpected RPC errors: a=%v b=%v", seen["a"].Err, seen["b"].Err)
	}
	var ra, rb map[string]string
	if err := json.Unmarshal(seen["a"].Result, &ra); err != nil {
		t.Fatalf("unmarshal a result: %v", err)
	}
	if err := json.Unmarshal(seen["b"].Result, &rb); err != nil {
		t.Fatalf("unmarshal b result: %v", err)
	}
	if ra["id"] != "cmdA" || rb["id"] != "cmdB" {
		t.Fatalf("id mismatch: a=%q b=%q, want cmdA/cmdB", ra["id"], rb["id"])
	}
	if seen["a"].ID == seen["b"].ID {
		t.Fatalf("request IDs collided: %v", seen["a"].ID)
	}
}

// TestHost_StderrRingBuf_Overflow writes more bytes to plugin stderr than the
// ring buffer can hold and asserts the host keeps only the latest slice
// without blocking the reader or the plugin.
func TestHost_StderrRingBuf_Overflow(t *testing.T) {
	bytesToWrite := stderrCap * 2 // overflow by 2x
	h, _, _ := newHostWithManifest(t, "stderr_overflow", map[string]string{
		"PLUGIN_STDERR_BYTES": strconv.Itoa(bytesToWrite),
	})
	defer h.Shutdown(time.Second)

	h.Emit(DidOpen{Path: "/a.go", Lang: "go", Text: ""})
	waitForStarted(t, h, "fake", 5*time.Second)

	// Wait for the stderr pump to settle: we want the buffer at capacity with
	// the sentinel "TAIL-<n>" visible.
	wantSentinel := fmt.Sprintf("TAIL-%d", bytesToWrite)
	h.mu.RLock()
	ext := h.running["fake"]
	h.mu.RUnlock()
	if ext == nil {
		t.Fatalf("ext not running")
	}
	waitUntil(t, 5*time.Second, func() bool {
		tail := ext.stderrBuf.Bytes()
		return len(tail) == stderrCap && strings.Contains(string(tail), wantSentinel)
	})

	tail := ext.stderrBuf.Bytes()
	if len(tail) != stderrCap {
		t.Fatalf("stderr tail len = %d, want capped at %d", len(tail), stderrCap)
	}
	if !strings.Contains(string(tail), wantSentinel) {
		t.Fatalf("stderr tail missing sentinel %q; last 64 bytes: %q", wantSentinel, string(tail[max(len(tail)-64, 0):]))
	}
}

// TestHost_ShutdownRace_WithInflightDidChange spams DidChange notifications
// then immediately shuts down to verify the writer drains coalesced frames,
// exits cleanly, and goroutines are not leaked.
func TestHost_ShutdownRace_WithInflightDidChange(t *testing.T) {
	// Count baseline goroutines so we can spot leaks caused by this test.
	runtime.GC()
	baseGor := runtime.NumGoroutine()

	h, _, _ := newHostWithManifest(t, "ok", nil)
	h.Emit(DidOpen{Path: "/a.go", Lang: "go", Text: ""})
	waitForStarted(t, h, "fake", 5*time.Second)

	// Fire DidChange events from multiple goroutines to maximize chance of a
	// race between the writer coalescing pending changes and Shutdown closing
	// writes.
	const (
		workers = 8
		perWrk  = 50
	)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range perWrk {
				h.Emit(DidChange{Path: "/a.go", Text: fmt.Sprintf("w%d-%d", w, i)})
			}
		}(w)
	}

	// Shut down while writes may still be in flight. Must complete within the
	// force-kill budget (3s here).
	wg.Wait() // ensure all Emit calls have enqueued
	start := time.Now()
	if err := h.Shutdown(3 * time.Second); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Shutdown exceeded 3s budget: %s", elapsed)
	}
	// Give the reader/writer goroutines a beat to drain after process exit.
	waitUntil(t, 3*time.Second, func() bool {
		runtime.GC()
		// Allow a small slack for the test framework's own goroutines.
		return runtime.NumGoroutine() <= baseGor+2
	})
}

// TestHost_DuplicateCommandID documents the current last-wins behavior when
// two plugins register the same command id. TODO(T3+): decide whether to
// reject duplicate registrations instead.
func TestHost_DuplicateCommandID(t *testing.T) {
	h := newHostWithPlugins()
	h.registerCommand("pluginA", "fmt", "A-fmt")
	h.registerCommand("pluginB", "fmt", "B-fmt")

	ref, ok := h.Commands()["fmt"]
	if !ok {
		t.Fatalf("fmt not registered")
	}
	// Current behavior: last-wins. If this assertion flips, the behavior
	// changed and the TODO above must be resolved.
	if ref.Plugin != "pluginB" || ref.Title != "B-fmt" {
		t.Fatalf("ref = %+v, want {pluginB, B-fmt} (current last-wins behavior)", ref)
	}
}

// TestManifest_ActivationEdgeCases exercises the activation matcher against
// realistic manifest patterns, including empty, singletons, duplicates, and
// unknown kinds.
func TestManifest_ActivationEdgeCases(t *testing.T) {
	lang := ActivationEvent{Kind: ActLanguage, Value: "go"}
	saveGo := ActivationEvent{Kind: ActSave, Value: "/x/y.go"}
	start := ActivationEvent{Kind: ActStart}

	cases := []struct {
		desc     string
		patterns []string
		event    ActivationEvent
		want     bool
		wantErr  bool
	}{
		{desc: "empty_slice_never_matches", patterns: nil, event: lang, want: false},
		{desc: "empty_slice_with_onStart_event", patterns: []string{}, event: start, want: false},
		{desc: "single_onLanguage_go_matches", patterns: []string{"onLanguage:go"}, event: lang, want: true},
		{desc: "onSave_alone_without_value_is_parse_error", patterns: []string{"onSave"}, event: saveGo, wantErr: true},
		{desc: "mixed_with_duplicates_still_matches", patterns: []string{"onStart", "onLanguage:go", "onStart", "onLanguage:go"}, event: start, want: true},
		{desc: "mixed_with_duplicates_no_match", patterns: []string{"onStart", "onStart"}, event: lang, want: false},
		{desc: "unknown_event_kind_errors", patterns: []string{"onHover:foo"}, event: lang, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			got, err := Match(tc.patterns, tc.event)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Match: expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("Match: unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Match = %v, want %v", got, tc.want)
			}
		})
	}
}

