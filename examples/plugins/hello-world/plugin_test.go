package main

import (
	"testing"

	"github.com/savimcio/nistru/sdk/plugsdk/plugintest"
)

// TestHelloPlugin_RegistersCommandOnInitialize asserts the hello-world
// plugin registers its "hello" command as part of its initialize handshake.
func TestHelloPlugin_RegistersCommandOnInitialize(t *testing.T) {
	h := plugintest.New(t, &helloPlugin{})
	if _, err := h.Initialize(nil); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	cmds := h.Commands()
	if len(cmds) != 1 || cmds[0] != "hello" {
		t.Fatalf("Commands() = %v, want [hello]", cmds)
	}
}

// TestHelloPlugin_ExecuteCommandEmitsNotify asserts the plugin emits a
// ui/notify notification when the host runs its "hello" command.
func TestHelloPlugin_ExecuteCommandEmitsNotify(t *testing.T) {
	h := plugintest.New(t, &helloPlugin{})
	if _, err := h.Initialize(nil); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if _, err := h.ExecuteCommand("hello", nil); err != nil {
		t.Fatalf("ExecuteCommand(hello): %v", err)
	}
	var saw bool
	for _, n := range h.Notifications() {
		if n.Method == "ui/notify" {
			saw = true
			break
		}
	}
	if !saw {
		t.Fatalf("expected ui/notify after executing hello; got notifs=%+v", h.Notifications())
	}
}

// TestHelloPlugin_UnknownCommandFallsThrough asserts the plugin's default
// fallthrough to the embedded Base yields a nil-result success, not an
// error, for ids other than "hello".
func TestHelloPlugin_UnknownCommandFallsThrough(t *testing.T) {
	h := plugintest.New(t, &helloPlugin{})
	if _, err := h.Initialize(nil); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	res, err := h.ExecuteCommand("nope", nil)
	if err != nil {
		t.Fatalf("ExecuteCommand(nope): %v", err)
	}
	if len(res) != 0 && string(res) != "null" {
		t.Fatalf("result = %s, want empty/null", res)
	}
}
