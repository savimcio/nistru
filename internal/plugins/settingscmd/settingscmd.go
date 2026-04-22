// Package settingscmd is the first-party in-proc plugin that owns the
// palette commands for inspecting and reloading Nistru's layered TOML
// configuration. Its four commands mirror the four things a user wants
// to do after editing settings:
//
//   - nistru.settings.openUser     — open ~/.config/nistru/config.toml
//   - nistru.settings.openProject  — open <root>/.nistru/config.toml
//   - nistru.settings.reload       — reparse both files + re-emit per-plugin config
//   - nistru.settings.showResolved — dump the fully-merged config to a temp file
//
// The plugin owns no state beyond the host handle and the config getter;
// the reload flow is driven by the editor Model, which intercepts the
// plugin.ReloadConfigRequest effect the reload command emits.
package settingscmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/savimcio/nistru/internal/config"
	"github.com/savimcio/nistru/plugin"
)

// Command IDs. Namespaced under nistru.settings.* so they fit alongside
// the autoupdate plugin's nistru.autoupdate.* space.
const (
	cmdOpenUser     = "nistru.settings.openUser"
	cmdOpenProject  = "nistru.settings.openProject"
	cmdReload       = "nistru.settings.reload"
	cmdShowResolved = "nistru.settings.showResolved"
)

// Plugin is the settings orchestration plugin: it owns palette commands
// for opening, reloading, and inspecting the merged configuration.
type Plugin struct {
	host   *plugin.Host
	root   string
	getCfg func() *config.Config
}

// New constructs a plugin rooted at the workspace root. getCfg returns the
// live *config.Config at call time so showResolved reflects the current
// post-reload state. getCfg must never be nil.
func New(root string, getCfg func() *config.Config) *Plugin {
	return &Plugin{root: root, getCfg: getCfg}
}

// Name implements plugin.Plugin.
func (p *Plugin) Name() string { return "settings" }

// Activation implements plugin.Plugin. The four palette commands must be
// available from the moment the editor boots, so we activate onStart.
func (p *Plugin) Activation() []string { return []string{"onStart"} }

// SetHost implements plugin.HostAware. Called once by the host before
// Initialize is dispatched.
func (p *Plugin) SetHost(h *plugin.Host) { p.host = h }

// OnEvent implements plugin.Plugin. Initialize registers the palette
// commands via PostNotif (mirrors autoupdate's pattern). ExecuteCommand
// dispatches to the per-command handler and returns its effects.
func (p *Plugin) OnEvent(ev any) []plugin.Effect {
	switch e := ev.(type) {
	case plugin.Initialize:
		p.registerCommands()
		return nil
	case plugin.ExecuteCommand:
		return p.dispatch(e)
	}
	return nil
}

// Shutdown implements plugin.Plugin. The plugin owns no background work,
// so teardown is a no-op.
func (p *Plugin) Shutdown() error { return nil }

// registerCommands fires four commands/register notifications via
// PostNotif; the host applies them synchronously so the palette picks
// them up on the very next frame.
func (p *Plugin) registerCommands() {
	if p.host == nil {
		return
	}
	cmds := []struct {
		id, title string
	}{
		{cmdOpenUser, "Nistru: Open User Settings"},
		{cmdOpenProject, "Nistru: Open Project Settings"},
		{cmdReload, "Nistru: Reload Settings"},
		{cmdShowResolved, "Nistru: Show Resolved Config"},
	}
	for _, c := range cmds {
		_ = p.host.PostNotif("settings", "commands/register", map[string]string{
			"id":    c.id,
			"title": c.title,
		})
	}
}

// dispatch routes an ExecuteCommand to its handler. Handlers return
// []plugin.Effect so OnEvent can surface them up to the host.
func (p *Plugin) dispatch(ev plugin.ExecuteCommand) []plugin.Effect {
	switch ev.ID {
	case cmdOpenUser:
		return p.openUser()
	case cmdOpenProject:
		return p.openProject()
	case cmdReload:
		return p.reload()
	case cmdShowResolved:
		return p.showResolved()
	}
	return nil
}

// openUser resolves the per-user config path, seeds it with the commented
// defaults skeleton when absent, and asks the editor to open it.
func (p *Plugin) openUser() []plugin.Effect {
	path, err := config.UserPath()
	if err != nil {
		return errNotify(err)
	}
	if err := ensureSeeded(path, config.CommentedDefaults()); err != nil {
		return errNotify(err)
	}
	return []plugin.Effect{plugin.OpenFile{Path: path}}
}

// openProject resolves the per-project config path, seeds it with the
// commented defaults skeleton when absent, and asks the editor to open it.
func (p *Plugin) openProject() []plugin.Effect {
	path := config.ProjectPath(p.root)
	if err := ensureSeeded(path, config.CommentedDefaults()); err != nil {
		return errNotify(err)
	}
	return []plugin.Effect{plugin.OpenFile{Path: path}}
}

// reload emits the sentinel effect the Model intercepts to reparse the
// config files + re-emit per-plugin config. Paired with a user-facing
// ui/notify so the status bar acknowledges the request.
func (p *Plugin) reload() []plugin.Effect {
	return []plugin.Effect{
		plugin.ReloadConfigRequest{},
		plugin.Notify{Level: "info", Message: "Reloading settings…"},
	}
}

// showResolved marshals the fully-merged config to TOML, writes it under
// <root>/.nistru/.resolved-config.toml, and opens that file in the editor.
// The dotfile name is deliberate — it's an inspection artefact, not a
// source of truth.
func (p *Plugin) showResolved() []plugin.Effect {
	cfg := p.getCfg()
	if cfg == nil {
		return errNotify(fmt.Errorf("settings: no config loaded"))
	}
	data, err := cfg.EncodeResolved()
	if err != nil {
		return errNotify(err)
	}
	path := filepath.Join(p.root, ".nistru", ".resolved-config.toml")
	if err := writeAtomic(path, data); err != nil {
		return errNotify(err)
	}
	return []plugin.Effect{plugin.OpenFile{Path: path}}
}

// ensureSeeded writes seed to path iff the file does not already exist.
// The parent directory is created with 0755. Existing files are left
// untouched so user edits never get clobbered by a palette command.
func ensureSeeded(path, seed string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("settings: stat %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("settings: mkdir %s: %w", filepath.Dir(path), err)
	}
	return writeAtomic(path, []byte(seed))
}

// writeAtomic writes data to path via a sibling .tmp file followed by
// os.Rename. Mirrors internal/editor/autosave.go's atomicWriteFile so a
// crash mid-write cannot leave a half-written file. Duplicated here to
// avoid an editor→settingscmd layering inversion.
func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("settings: mkdir %s: %w", filepath.Dir(path), err)
	}
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("settings: write tmp %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("settings: rename %s: %w", path, err)
	}
	return nil
}

// errNotify wraps err as a single Notify effect. The framework expects
// command handlers to surface errors via effects, not returns.
func errNotify(err error) []plugin.Effect {
	return []plugin.Effect{plugin.Notify{Level: "error", Message: err.Error()}}
}

// Compile-time assertions so a missing interface surfaces at build time.
var (
	_ plugin.Plugin    = (*Plugin)(nil)
	_ plugin.HostAware = (*Plugin)(nil)
)
