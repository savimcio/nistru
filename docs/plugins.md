# Plugin system

Nistru has one plugin API with two transports:

- **In-process** — Go interface, linked into the binary. Used for panes that render on every keystroke (the bundled file tree is one).
- **Out-of-process** — newline-delimited JSON-RPC 2.0 over a child process's stdio. Used for language-agnostic plugins: formatters, linters, LSP adapters.

Same event names, same capabilities, same mental model on either side.

```
┌────────────────── nistru process ──────────────────┐
│  Bubble Tea Update ──▶ pluginHost                 │
│                           │                       │
│                  ┌────────┴────────┐              │
│       direct Go calls         stdio JSON-RPC      │
│                  │                  │             │
│         in-proc plugins     ext-proc plugins      │
│         (e.g. treepane)     (e.g. gofmt, LSP)     │
└───────────────────────────────────────────────────┘
```

## Using plugins

Drop a plugin into one of:

- `~/.config/nistru/plugins/<name>/plugin.json` — user-wide
- `<project>/.nistru/plugins/<name>/plugin.json` — project-local (overrides user)

Each directory needs a `plugin.json` manifest and whatever binary/script the manifest points at.

Open the command palette with **Ctrl+P** to see commands registered by active plugins.

## Manifest schema (`plugin.json`)

```json
{
  "name": "gofmt",
  "version": "0.1.0",
  "cmd": ["./gofmt-plugin"],
  "activation": ["onLanguage:go", "onCommand:gofmt"],
  "capabilities": ["formatter", "commands"]
}
```

| Field | Meaning |
|---|---|
| `name` | Lowercase `[a-z0-9-]{1,64}`. Must be unique. |
| `version` | Free-form string; shown in diagnostics. |
| `cmd` | Executable argv. Resolved relative to the plugin's directory. |
| `activation` | Events that wake the plugin (see below). Listed once, matched many times. |
| `capabilities` | What the plugin contributes (advisory; v1 doesn't gate on these). |

### Activation patterns

| Pattern | Fires when |
|---|---|
| `onStart` | editor starts |
| `onLanguage:<ext>` | a file with that extension is opened (case-insensitive) |
| `onSave:<glob>` | a saved file's basename matches `<glob>` (`filepath.Match` syntax) |
| `onCommand:<id>` | `<id>` is invoked from the palette |

Plugins are **lazy**: the subprocess is only spawned when the first matching event occurs. Once activated, the plugin keeps receiving `DidOpen`/`DidChange`/`DidSave`/`DidClose` for the rest of the session.

### Capabilities

| Capability | Meaning |
|---|---|
| `commands` | Plugin registers palette commands |
| `formatter` | Plugin implements `textDocument/format` |
| `status-bar` | Plugin writes to the status bar |
| `pane` | In-process only: plugin owns a layout slot |

## Writing an out-of-process plugin

Use the Go SDK at `github.com/savimcio/nistru/sdk/plugsdk`. Any language works over the wire, but the SDK is the shortest path.

```go
package main

import (
    "encoding/json"
    "fmt"
    "os"

    "github.com/savimcio/nistru/sdk/plugsdk"
)

type hello struct{ plugsdk.Base }

func (h *hello) OnInitialize(root string, caps []string) error {
    return h.Client().RegisterCommand("hello", "Say Hello")
}

func (h *hello) OnExecuteCommand(id string, _ json.RawMessage) (any, error) {
    if id == "hello" {
        return nil, h.Client().Notify("info", "Hello from plugin!")
    }
    return nil, nil
}

func main() {
    if err := plugsdk.Run(&hello{}); err != nil {
        fmt.Fprintln(os.Stderr, err)
        os.Exit(1)
    }
}
```

Build it, put it next to its `plugin.json`, restart nistru. Ctrl+P shows `Say Hello`. See `examples/plugins/hello-world/` for a complete working example, and `examples/plugins/gofmt/` as a reference example illustrating the basic shape — note that the gofmt example currently exhibits the deadlock footgun described under Testing your plugin, so it should not be read as a finished production plugin.

### Host → plugin calls

The SDK maps these to methods on your `Plugin` implementation:

| Method | Fires on |
|---|---|
| `OnInitialize(root, caps)` | startup handshake |
| `OnDidOpen(path, lang, text)` | file opened |
| `OnDidChange(path, text)` | buffer changed (debounced 50 ms) |
| `OnDidSave(path)` | file written to disk |
| `OnDidClose(path)` | file closed (next file opened or editor quit) |
| `OnExecuteCommand(id, args) → (result, err)` | palette or host-initiated command |
| `OnShutdown()` | clean shutdown |

All calls arrive on a single reader goroutine. Long work should offload to a goroutine and communicate back via the `Client`.

### Plugin → host calls

The `plugsdk.Client` (accessed via `Base.Client()`) gives you:

| Method | Effect |
|---|---|
| `RegisterCommand(id, title)` | adds to palette |
| `UnregisterCommand(id)` | removes from palette |
| `SetStatusBar(segment, text, color)` | upserts a status-bar segment |
| `Notify(level, message)` | transient status message |
| `BufferEdit(path, text)` | replace the buffer (blocks on ACK) |
| `OpenFile(path)` | ask the editor to open a file (blocks on ACK) |

`BufferEdit` and `OpenFile` are requests — the host replies when the edit has landed.

### Wire protocol

Newline-delimited JSON-RPC 2.0 on stdio. One frame per line. The SDK handles framing, correlation, and shutdown; you shouldn't need to look at the wire unless you're writing a non-Go plugin. If you are, see `plugin/protocol.go` for the codec.

## Writing an in-process plugin

In-process plugins live inside the nistru binary. Use this path when:

- Every keystroke needs to render something (panes).
- Latency matters more than isolation.
- The plugin is stable enough to ship with the editor.

Implement `plugin.Plugin`; optionally implement `plugin.Pane` if you want to own a screen region.

```go
type Plugin interface {
    Name() string
    Activation() []string
    OnEvent(event any) []Effect  // event is any of DidOpen, DidChange, ...
    Shutdown() error
}

type Pane interface {
    Render(w, h int) string
    HandleKey(k KeyEvent) []Effect
    OnResize(w, h int)
    OnFocus(focused bool)
    Slot() string  // "left" | "right" | "bottom"
}
```

Register at startup in `NewModel`:

```go
registry.RegisterInProc(myplugin.New(root))
```

See `internal/plugins/treepane/treepane.go` for a full example.

## Effects

In-process plugin calls return effects; out-of-process plugins send equivalent JSON-RPC notifications. Both flow through the same host plumbing:

| Effect | Meaning |
|---|---|
| `OpenFile{Path}` | open a file in the editor |
| `Notify{Level, Message}` | show a transient status message |
| `Focus{Pane}` | change keyboard focus |
| `Invalidate` | request a re-render |

Plugins never touch `tea.Msg` directly; effects keep plugins portable across the transport boundary.

## Performance notes

- **Lazy spawn.** External plugins aren't launched until something matches their activation.
- **Debounced buffer events.** `DidChange` is coalesced over a 50 ms idle window — plugins see one event per burst of typing, not one per keystroke.
- **Latest-wins coalescing per path.** If a plugin's write channel fills up, older `DidChange` for the same path are dropped before the newest one.
- **Per-request timeout.** Host → plugin requests have a 5 s deadline; a hung plugin can't stall the editor.
- **Panic recovery for in-process plugins.** A panicking plugin is marked unhealthy and stops receiving events; the editor keeps running.

## Crash isolation

Out-of-process plugins run as subprocesses with the user's permissions. A crash surfaces as a status-bar message; no further events are dispatched to that plugin in the current session. The last ~16 KiB of the plugin's stderr is captured and shown alongside the exit status for diagnostics.

## Limitations (v1)

- `DidChange` sends full text, not deltas. Fine for files up to the 1 MiB open cap; incremental sync is a later optimization.
- No sandboxing beyond the OS process boundary. Plugins you install can read and write anything you can.
- Panes are in-process only. External plugins can register commands, update the status bar, and edit buffers, but can't own a screen region.
- No plugin marketplace or auto-install. You copy files by hand.

## Testing your plugin

The SDK ships an in-memory harness at [`github.com/savimcio/nistru/sdk/plugsdk/plugintest`](../sdk/plugsdk/plugintest/plugintest.go). It wires your `plugsdk.Plugin` to the real `Run` loop through an `io.Pipe` pair, so tests exercise the production JSON-RPC framing, dispatch, and correlation paths. Every outbound notification and request the plugin emits is recorded under a mutex; inspection methods return copies.

```go
package main

import (
    "testing"

    "github.com/savimcio/nistru/sdk/plugsdk/plugintest"
)

func TestHelloPlugin_RegistersCommandOnInitialize(t *testing.T) {
    h := plugintest.New(t, &helloPlugin{})
    if _, err := h.Initialize(nil); err != nil {
        t.Fatalf("Initialize: %v", err)
    }
    if cmds := h.Commands(); len(cmds) != 1 || cmds[0] != "hello" {
        t.Fatalf("Commands() = %v, want [hello]", cmds)
    }
}

func TestHelloPlugin_ExecuteCommandEmitsNotify(t *testing.T) {
    h := plugintest.New(t, &helloPlugin{})
    if _, err := h.Initialize(nil); err != nil {
        t.Fatalf("Initialize: %v", err)
    }
    if _, err := h.ExecuteCommand("hello", nil); err != nil {
        t.Fatalf("ExecuteCommand(hello): %v", err)
    }
    for _, n := range h.Notifications() {
        if n.Method == "ui/notify" {
            return
        }
    }
    t.Fatalf("expected ui/notify after executing hello; got %+v", h.Notifications())
}
```

`plugintest.New(t, p)` registers cleanup via `t.Cleanup`; tests do not need to defer anything.

### `Harness` API

Event feeders drive the plugin:

| Method | Fires on the plugin |
|---|---|
| `Initialize(params)` | initialize request; blocks for the response |
| `DidOpen(path, lang, text)` | didOpen notification |
| `DidChange(path, text)` | didChange notification |
| `DidSave(path)` | didSave notification |
| `DidClose(path)` | didClose notification |
| `ExecuteCommand(id, args)` | executeCommand request; blocks for the response |
| `Shutdown()` | shutdown request + tears down pipes |

Inspection methods read back what the plugin sent:

| Method | Returns |
|---|---|
| `Notifications()` | every notification in order |
| `Requests()` | every plugin-originated request in order |
| `StatusSegments()` | current `statusBar/set` segment map |
| `Commands()` | ordered list of currently-registered command ids |
| `LastResponseFor(id)` | the response the plugin emitted for host-request `id` |
| `SetRequestResponder(fn)` | override how the harness replies to plugin round-trips |

Worked examples: [`examples/plugins/hello-world/plugin_test.go`](../examples/plugins/hello-world/plugin_test.go), [`examples/plugins/gofmt/plugin_test.go`](../examples/plugins/gofmt/plugin_test.go).

### ⚠️ Footgun: synchronous `Client` round-trips deadlock

The SDK dispatches every `OnInitialize` / `OnDidSave` / `OnExecuteCommand` call on a **single reader goroutine**. If your handler calls `Client().BufferEdit(...)` (or any other request-response SDK method) and waits for the result on that same goroutine, the reader is blocked and can never deliver the incoming response. The handler times out; the plugin appears hung.

Workaround: spawn a goroutine for the blocking call, or structure the handler so it fires and returns without waiting for a host ACK.

```go
// Dangerous: blocks the reader until the host replies.
func (p *fmt) OnExecuteCommand(id string, _ json.RawMessage) (any, error) {
    return nil, p.Client().BufferEdit(p.current, formatted) // <- deadlocks
}

// Safer: offload the round-trip, let the reader keep draining responses.
func (p *fmt) OnExecuteCommand(id string, _ json.RawMessage) (any, error) {
    go func() { _ = p.Client().BufferEdit(p.current, formatted) }()
    return nil, nil
}
```

The bundled [`gofmt` example](../examples/plugins/gofmt/main.go) currently calls `BufferEdit` synchronously and is vulnerable to this pattern. Its tests work around it by invoking `ExecuteCommand` in a goroutine and asserting against `h.Requests()`. A follow-up should either dispatch handlers on a per-frame goroutine inside the SDK or rewrite the example to spawn.

## Reference

- Host internals: `plugin/host.go`, `plugin/extproc.go`
- Protocol codec: `plugin/protocol.go`
- API types: `plugin/api.go`, `plugin/pane.go`
- SDK: `sdk/plugsdk/plugsdk.go`
- Plugin-author harness: `sdk/plugsdk/plugintest/plugintest.go`
- Examples: `examples/plugins/{hello-world,gofmt}/`
