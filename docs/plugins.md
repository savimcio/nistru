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

## Plugin configuration

Plugins receive per-plugin config from Nistru's layered TOML file. Config lives under `[plugins.<name>]` in either:

- `~/.config/nistru/config.toml` — user-wide.
- `<root>/.nistru/config.toml` — project-local (overrides user).

The `<name>` matches `Plugin.Name()`. Unknown sub-tables are kept; Nistru does not gate on schema. See [internal/config/](../internal/config/) for the merge pipeline.

### Receiving config

Implement the `ConfigReceiver` interface. The plugin host (in-proc) and SDK (out-of-proc) call `OnConfig` with the raw JSON bytes of the `[plugins.<name>]` sub-table before the first `Initialize`:

```go
// In-process plugin: plugin.ConfigReceiver (plugin/api.go).
func (p *MyPlugin) OnConfig(raw json.RawMessage) error {
    var cfg struct {
        Enabled bool `json:"enabled"`
    }
    return json.Unmarshal(raw, &cfg)
}
```

```go
// Out-of-process plugin: plugsdk.ConfigReceiver (sdk/plugsdk/plugsdk.go).
// plugsdk.Base provides a no-op OnConfig so embedders only override when needed.
func (p *MyPlugin) OnConfig(raw json.RawMessage) error {
    var cfg struct {
        Enabled bool `json:"enabled"`
    }
    return json.Unmarshal(raw, &cfg)
}
```

`raw` is `nil` when no `[plugins.<name>]` section exists. Handle `nil` as "use defaults."

### When OnConfig fires

1. **Once before the first `Initialize`** — in-proc, synchronously on the host goroutine; out-of-proc, injected into the spawn handshake's first Initialize frame so the SDK dispatches `OnConfig` before `OnInitialize`.
2. **Again on every `Nistru: Reload Settings` palette invocation** — the host reparses both config files and re-emits `Initialize` to every already-activated plugin. `OnConfig` sees the fresh sub-tree each time.

Errors returned from `OnConfig` are logged to stderr but non-fatal. `Initialize` still proceeds so a plugin with a malformed config section starts up with whatever state it had before the bad config was applied.

### Autoupdate env vars

The `autoupdate` plugin predates the config system. Its `NISTRU_AUTOUPDATE_{REPO,CHANNEL,INTERVAL,DISABLE}` env vars are kept as an emergency override path but are deprecated — prefer `[plugins.autoupdate]` in TOML.

Precedence (low → high): defaults < config < env < construction-time options. The option layer exists purely as a test seam (`WithRepo`, `WithInterval`); env stays above config so a shell-level override can still unblock a user whose config is broken.

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

### In-process async notifications

In-process plugins run `OnEvent` on the Bubble Tea goroutine and must return quickly. When work needs to happen in the background — polling a file, watching a socket, running a ticker — spawn a goroutine and report results back to the host via `plugin.Host.PostNotif`.

Opt in by implementing the optional `HostAware` interface. The host calls `SetHost` once, before `Initialize` is dispatched:

```go
type HostAware interface {
    SetHost(h *plugin.Host)
}
```

`PostNotif` synthesizes a JSON-RPC notification and routes it through the same inbound pipeline that out-of-process plugins use, so the editor model handles it identically regardless of transport:

```go
func (h *plugin.Host) PostNotif(plugin, method string, params any) error
```

It's **safe to call from any goroutine** and **non-blocking**: if the inbound buffer is saturated the notification is dropped and an error is returned. Host-side bookkeeping for methods like `commands/register` runs synchronously before the send, so command registration is effective on return even when the send itself drops. Supported methods mirror the wire protocol: `statusBar/set`, `ui/notify`, `commands/register`, `commands/unregister`.

Use it for status-bar updates, toast notifications, and late command registration. Do not call it from an out-of-process plugin — those already have a dedicated writer goroutine.

```go
type clock struct {
    host *plugin.Host
    stop chan struct{}
}

func (c *clock) Name() string         { return "clock" }
func (c *clock) Activation() []string { return []string{"onStart"} }
func (c *clock) SetHost(h *plugin.Host) { c.host = h }

func (c *clock) OnEvent(ev any) []plugin.Effect {
    if _, ok := ev.(plugin.Initialize); ok {
        c.stop = make(chan struct{})
        go c.tick()
    }
    return nil
}

func (c *clock) tick() {
    t := time.NewTicker(time.Second)
    defer t.Stop()
    for {
        select {
        case <-c.stop:
            return
        case now := <-t.C:
            _ = c.host.PostNotif("clock", "statusBar/set", map[string]any{
                "segment": "clock",
                "text":    now.Format("15:04:05"),
            })
        }
    }
}

func (c *clock) Shutdown() error { close(c.stop); return nil }
```

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

## Auto-update (first-party example)

The bundled `autoupdate` plugin (under `internal/plugins/autoupdate/`) is the canonical template for any in-process plugin that needs to do work on a schedule. Read it before writing your own background plugin — it exercises every piece of the `HostAware` + `PostNotif` contract end-to-end: a ticker-driven poll loop, a status-bar segment, late palette-command registration, and a `ui/notify` toast when a new release appears.

**Why this pattern exists.** In-process plugins receive events synchronously on the Bubble Tea goroutine. `OnEvent` must return quickly or it will stall the editor's `Update` loop. Anything long-running — an HTTP request, a file watcher, a timer — has to run on its own goroutine. But background goroutines can't safely touch the model directly; they need a way to push changes back through the same pipeline that delivers out-of-process plugin notifications.

That's what `HostAware` + `PostNotif` provide. Implement `SetHost` to capture the host reference, spawn a goroutine from your `Initialize` handler, and call `host.PostNotif` whenever the goroutine has something to report. Under the hood the host synthesizes a JSON-RPC notification and routes it through the same inbound channel used by external plugins, so the editor model's `handlePluginNotif` treats both transports identically.

### Minimal template

```go
type MyPlugin struct {
    host *plugin.Host
}

func (p *MyPlugin) Name() string         { return "myplugin" }
func (p *MyPlugin) Activation() []string { return []string{"onStart"} }
func (p *MyPlugin) Shutdown() error      { return nil }

func (p *MyPlugin) SetHost(h *plugin.Host) { p.host = h }

func (p *MyPlugin) OnEvent(event any) []plugin.Effect {
    if _, ok := event.(plugin.Initialize); ok {
        go p.backgroundLoop()
    }
    return nil
}

func (p *MyPlugin) backgroundLoop() {
    tick := time.NewTicker(1 * time.Minute)
    defer tick.Stop()
    for range tick.C {
        _ = p.host.PostNotif("myplugin", "statusBar/set", map[string]string{
            "segment": "status",
            "text":    "updated " + time.Now().Format(time.Kitchen),
            "color":   "cyan",
        })
    }
}
```

The real `autoupdate` plugin (`internal/plugins/autoupdate/`) fleshes this shape out with a channel-driven stop signal, configurable interval, HTTP client with retry/backoff, SHA-256 verification, and late palette-command registration on first successful poll. It's the recommended reference for any plugin whose work should survive a `Shutdown` cleanly.

### `PostNotif` contract

- **Safe to call from any goroutine.** Internally guarded; you do not need to hold a lock.
- **Non-blocking.** If the inbound buffer is saturated the notification is dropped and a non-nil error is returned. Do not retry in a tight loop; drop-on-full is the intended backpressure.
- **Host-side bookkeeping is synchronous.** For methods like `commands/register` / `commands/unregister`, the host updates its command table before the send returns, so a subsequent palette open reflects the change even if the actual notification frame was dropped.
- **Supported methods today** (see `internal/editor/model.go` `handlePluginNotif`):

  | Method | Effect |
  |---|---|
  | `statusBar/set` | upsert a status-bar segment (`{segment, text, color}`) |
  | `ui/notify` | transient toast in the status area (`{level, message}`) |
  | `commands/register` | add a palette entry (`{id, title}`) |
  | `commands/unregister` | remove a palette entry (`{id}`) |
  | `pane/invalidate` | request a re-render of your pane |

  Any other method name is ignored by the model; check `handlePluginNotif` before shipping a new verb.

Use `PostNotif` only from in-process plugins. Out-of-process plugins already have a dedicated writer goroutine and should emit notifications via `plugsdk.Client` instead.

## Reference

- Host internals: `plugin/host.go`, `plugin/extproc.go`
- Protocol codec: `plugin/protocol.go`
- API types: `plugin/api.go`, `plugin/pane.go`
- SDK: `sdk/plugsdk/plugsdk.go`
- Plugin-author harness: `sdk/plugsdk/plugintest/plugintest.go`
- Examples: `examples/plugins/{hello-world,gofmt}/`
- First-party async plugin: `internal/plugins/autoupdate/`
