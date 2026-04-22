// Package plugin defines the nistru editor's plugin API contract.
//
// This file is the single source of truth for event names and payload shapes.
// The same event structs are reused by two transports:
//
//   - The in-process Plugin interface defined in this file, where events are
//     delivered by value to OnEvent on the Bubble Tea goroutine.
//   - The JSON-RPC codec defined in protocol.go (next task), which serializes
//     these structs on the wire for out-of-process plugins.
//
// Do not duplicate the event types in protocol.go — import and reuse them from
// here. Plugins must not import bubbletea; the host converts terminal input
// into the transport-neutral types declared here and in pane.go.
package plugin

import "encoding/json"

// DidOpen is emitted when the editor opens a buffer.
type DidOpen struct {
	// Path is the absolute path of the opened file.
	Path string `json:"path"`
	// Lang is the detected language identifier (e.g. "go", "markdown").
	Lang string `json:"lang"`
	// Text is the full buffer contents at open time.
	Text string `json:"text"`
}

// DidChange is emitted after the buffer text changes.
type DidChange struct {
	// Path is the absolute path of the changed file.
	Path string `json:"path"`
	// Text is the full buffer contents after the change.
	Text string `json:"text"`
}

// DidSave is emitted after the buffer is persisted to disk.
type DidSave struct {
	// Path is the absolute path of the saved file.
	Path string `json:"path"`
}

// DidClose is emitted when the editor closes a buffer.
type DidClose struct {
	// Path is the absolute path of the closed file.
	Path string `json:"path"`
}

// Initialize is the first event delivered to a plugin after activation.
type Initialize struct {
	// RootPath is the absolute path of the editor's workspace root.
	RootPath string `json:"rootPath"`
	// Capabilities advertises host-side capabilities the plugin may rely on.
	Capabilities []string `json:"capabilities"`
	// Config is the plugin's own config sub-tree, encoded as raw JSON. It is
	// nil when no host-side config lookup is installed or when the lookup
	// returns nothing for this plugin's name. Plugins that prefer to receive
	// config out of band can implement ConfigReceiver instead; both channels
	// are kept in sync and carry the same bytes.
	Config json.RawMessage `json:"config,omitempty"`
}

// Shutdown is emitted once before the plugin is torn down. Distinct from the
// Plugin.Shutdown method on the in-process interface.
type Shutdown struct{}

// ExecuteCommand is a request asking the plugin to run a registered command.
type ExecuteCommand struct {
	// ID is the command identifier declared in the plugin's manifest.
	ID string `json:"id"`
	// Args is the opaque command payload; plugins decode it themselves.
	Args json.RawMessage `json:"args,omitempty"`
}

// ExecuteCommandResult is the response to an ExecuteCommand request.
type ExecuteCommandResult struct {
	// Result is the opaque command response payload.
	Result json.RawMessage `json:"result,omitempty"`
}

// Effect is a sealed sum type describing an action the host should apply on
// the plugin's behalf. Only the concrete variants declared in this package
// satisfy the interface.
type Effect interface {
	isEffect()
}

// OpenFile asks the editor to open the file at Path in a buffer.
type OpenFile struct {
	// Path is the absolute path of the file to open.
	Path string `json:"path"`
}

func (OpenFile) isEffect() {}

// Notify asks the editor to display a transient status-bar message.
type Notify struct {
	// Level is a free-form severity tag (e.g. "info", "warn", "error").
	Level string `json:"level"`
	// Message is the text to display to the user.
	Message string `json:"message"`
}

func (Notify) isEffect() {}

// Focus asks the editor to move input focus to the named pane.
type Focus struct {
	// Pane is the slot name of the pane to focus (see Pane.Slot).
	Pane string `json:"pane"`
}

func (Focus) isEffect() {}

// Invalidate asks the editor to re-render the plugin's pane on the next tick.
type Invalidate struct{}

func (Invalidate) isEffect() {}

// ReloadConfigRequest asks the editor to reload its configuration files and
// re-emit per-plugin config via the host. Handled entirely on the editor
// side; the plugin package has no knowledge of config file layout.
type ReloadConfigRequest struct{}

func (ReloadConfigRequest) isEffect() {}

// Capability names a host-provided extension point a plugin may use.
type Capability string

// Capability constants enumerate the extension points the host advertises.
const (
	// CapCommands indicates the plugin may register ExecuteCommand handlers.
	CapCommands Capability = "commands"
	// CapFormatter indicates the plugin may register a buffer formatter.
	CapFormatter Capability = "formatter"
	// CapStatusBar indicates the plugin may contribute status-bar segments.
	CapStatusBar Capability = "status-bar"
	// CapPane indicates the plugin may own a rectangular pane (see Pane).
	CapPane Capability = "pane"
)

// Plugin is the in-process contract implemented by every nistru plugin.
//
// The host invokes OnEvent synchronously on the Bubble Tea goroutine, so
// implementations must return quickly and never block on I/O. Long-running
// work belongs in a goroutine owned by the plugin.
type Plugin interface {
	// Name returns the plugin's stable identifier, used for logging and
	// command routing. It must not change across the plugin's lifetime.
	Name() string

	// Activation returns the activation event patterns that should trigger
	// loading this plugin. The vocabulary matches out-of-process manifests:
	//   - "onStart"             — activate at editor startup
	//   - "onLanguage:<ext>"    — activate when a buffer of this language opens
	//   - "onSave:<glob>"       — activate when a file matching the glob saves
	//   - "onCommand:<id>"      — activate when the named command is invoked
	Activation() []string

	// OnEvent receives one of the event structs declared in this file and
	// returns the effects the host should apply. Implementations must return
	// promptly; the host calls this on the UI goroutine.
	OnEvent(event any) []Effect

	// Shutdown is the graceful teardown hook. It runs after the Shutdown
	// event has been delivered and is the plugin's last chance to release
	// resources. Distinct from the Shutdown event struct.
	Shutdown() error
}

// HostAware is an optional extension implemented by in-process plugins that
// need to call back into the Host from their own goroutines (e.g. to post
// asynchronous status-bar updates). The Host calls SetHost exactly once,
// before the Initialize event is dispatched.
type HostAware interface {
	SetHost(h *Host)
}

// ConfigReceiver is an optional extension implemented by in-process plugins
// that want to receive their own config sub-tree out of band from the
// Initialize event. The Host calls OnConfig immediately before dispatching
// the Initialize event the first time, and again on every ReloadConfig call.
// If the plugin also reads the Config field of Initialize it will see the
// same bytes.
//
// OnConfig fires on every Initialize dispatch (initial boot and every
// subsequent reload), regardless of whether a config sub-tree is present.
// A nil raw argument means `[plugins.<name>]` is absent from the effective
// config; plugins should treat this as a reset/defaults signal. The same
// uniform contract applies to out-of-process plugins via plugsdk's
// ConfigReceiver — both transports behave identically on the wire.
// Implementations MUST treat a nil/empty raw as a reset rather than a
// no-op, otherwise removing a config section will leave stale state
// behind.
type ConfigReceiver interface {
	OnConfig(raw json.RawMessage) error
}
