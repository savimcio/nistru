package main

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/savimcio/nistru/sdk/plugsdk/plugintest"
)

// maybeSkipGofmt mirrors the e2e skip: if gofmt is not on PATH, skip.
func maybeSkipGofmt(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("gofmt"); err != nil {
		t.Skipf("gofmt not on PATH: %v", err)
	}
}

// TestGofmtPlugin_RegistersCommandOnInitialize asserts the gofmt plugin
// registers its "gofmt" command during the initialize handshake.
func TestGofmtPlugin_RegistersCommandOnInitialize(t *testing.T) {
	h := plugintest.New(t, &gofmtPlugin{})
	if _, err := h.Initialize(nil); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	cmds := h.Commands()
	if len(cmds) != 1 || cmds[0] != "gofmt" {
		t.Fatalf("Commands() = %v, want [gofmt]", cmds)
	}
}

// TestGofmtPlugin_DidSaveFormatsGoFile asserts the plugin emits a
// buffer/edit request with gofmt-formatted contents when a .go file is
// saved after being opened with unformatted contents.
func TestGofmtPlugin_DidSaveFormatsGoFile(t *testing.T) {
	maybeSkipGofmt(t)
	h := plugintest.New(t, &gofmtPlugin{})
	if _, err := h.Initialize(nil); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	const unformatted = "package x\nfunc   f (){}\n"
	h.DidOpen("/tmp/x.go", "go", unformatted)
	h.DidSave("/tmp/x.go")

	// Poll for the buffer/edit request; the plugin runs gofmt in-handler
	// but the Client.BufferEdit round-trip is blocking, so the harness
	// auto-responds as soon as the request lands.
	if !waitForRequest(h, "buffer/edit", 2*time.Second) {
		t.Fatalf("never saw buffer/edit; requests=%+v", h.Requests())
	}

	reqs := h.Requests()
	if len(reqs) == 0 {
		t.Fatalf("no requests recorded")
	}
	var got struct{ Path, Text string }
	if err := json.Unmarshal(reqs[0].Params, &got); err != nil {
		t.Fatalf("unmarshal buffer/edit params: %v", err)
	}
	if got.Path != "/tmp/x.go" {
		t.Fatalf("path = %q", got.Path)
	}
	if got.Text == unformatted {
		t.Fatalf("text unchanged — expected gofmt to reformat")
	}
	// Sanity: formatted text should contain the canonical "func f()".
	if !strings.Contains(got.Text, "func f()") {
		t.Fatalf("formatted text missing %q; got %q", "func f()", got.Text)
	}
}

// TestGofmtPlugin_DidSaveNonGoFileIgnored asserts non-.go paths do not
// trigger a buffer/edit.
func TestGofmtPlugin_DidSaveNonGoFileIgnored(t *testing.T) {
	maybeSkipGofmt(t)
	h := plugintest.New(t, &gofmtPlugin{})
	if _, err := h.Initialize(nil); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	h.DidOpen("/tmp/readme.md", "markdown", "# readme   \n")
	h.DidSave("/tmp/readme.md")

	// Give the plugin a chance to misbehave, then assert no buffer/edit.
	time.Sleep(50 * time.Millisecond)
	for _, r := range h.Requests() {
		if r.Method == "buffer/edit" {
			t.Fatalf("unexpected buffer/edit for non-Go file: %+v", r)
		}
	}
}

// TestGofmtPlugin_ExecuteCommandFormatsCurrentFileEmitsEdit asserts that
// running the "gofmt" command produces a buffer/edit request for the
// currently open file. The test does not wait for the executeCommand
// response because the plugin's handler blocks on the buffer/edit
// round-trip — the request is observable as soon as it lands on the wire,
// which is what we assert.
func TestGofmtPlugin_ExecuteCommandFormatsCurrentFileEmitsEdit(t *testing.T) {
	maybeSkipGofmt(t)
	h := plugintest.New(t, &gofmtPlugin{})
	if _, err := h.Initialize(nil); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	h.DidOpen("/tmp/a.go", "go", "package a\nfunc   g (){}\n")

	// Fire executeCommand in a goroutine; the SDK's reader loop will
	// block inside OnExecuteCommand -> BufferEdit round-trip, so the
	// executeCommand response can never be sent back. We only care that
	// the buffer/edit request showed up.
	go func() {
		_, _ = h.ExecuteCommand("gofmt", nil)
	}()
	if !waitForRequest(h, "buffer/edit", 2*time.Second) {
		t.Fatalf("never saw buffer/edit after executeCommand")
	}
}

// TestGofmtPlugin_ExecuteCommandWithoutOpenFileReturnsError asserts
// running the command with no open file produces an error response.
func TestGofmtPlugin_ExecuteCommandWithoutOpenFileReturnsError(t *testing.T) {
	maybeSkipGofmt(t)
	h := plugintest.New(t, &gofmtPlugin{})
	if _, err := h.Initialize(nil); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if _, err := h.ExecuteCommand("gofmt", nil); err == nil {
		t.Fatalf("expected error from gofmt with no open file")
	}
}

// TestGofmtPlugin_DidCloseResetsCurrentPath asserts closing the current
// file clears the plugin's currentPath so a follow-up executeCommand
// reports "no current file" rather than formatting a stale path.
func TestGofmtPlugin_DidCloseResetsCurrentPath(t *testing.T) {
	maybeSkipGofmt(t)
	h := plugintest.New(t, &gofmtPlugin{})
	if _, err := h.Initialize(nil); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	h.DidOpen("/tmp/a.go", "go", "package a\n")
	h.DidClose("/tmp/a.go")
	if _, err := h.ExecuteCommand("gofmt", nil); err == nil {
		t.Fatalf("expected error after didClose")
	}
}

// waitForRequest polls the harness for up to timeout for a request with
// the given method. Returns true if seen.
func waitForRequest(h *plugintest.Harness, method string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, r := range h.Requests() {
			if r.Method == method {
				return true
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, r := range h.Requests() {
		if r.Method == method {
			return true
		}
	}
	return false
}
