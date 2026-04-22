// Package autoupdate is the first-party plugin that watches for new Nistru
// releases and lets the user install them from the palette.
//
// Lifecycle:
//   - plugin.Plugin entry is wired through the in-proc registry at editor
//     startup; SetHost runs before Initialize is delivered.
//   - Initialize kicks off a single background goroutine (the checker) that
//     polls GitHub on an interval with jitter. The goroutine is the only
//     owner of host I/O for background status-bar updates; everything else
//     flows through PostNotif so effects arrive on the UI goroutine via the
//     standard inbound channel.
//   - Shutdown cancels the checker and waits briefly for it to exit.
//
// The install seam is intentionally minimal — T7 will replace noopInstaller
// with a real binary-swap path. This file stays stable across that work.
package autoupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/savimcio/nistru/plugin"
)

const (
	// defaultRepo is the public repository we poll when no override is
	// supplied. Tests always set their own via WithRepo.
	defaultRepo = "savimcio/nistru"

	// defaultInterval is the ticker period between release checks.
	defaultInterval = 1 * time.Hour

	// shutdownGrace is the maximum time we wait for the checker goroutine
	// to exit during Shutdown.
	shutdownGrace = 500 * time.Millisecond

	// envRepo, envChannel, envInterval, envDisable are the environment-
	// variable overrides honoured by handleInitialize. They sit above config
	// but below constructor options in the precedence ladder so shell-side
	// overrides remain an emergency kill switch.
	//
	// Deprecated: prefer the [plugins.autoupdate] TOML section. Env still wins
	// as an emergency override.
	envRepo = "NISTRU_AUTOUPDATE_REPO"
	// Deprecated: prefer the [plugins.autoupdate] TOML section. Env still wins
	// as an emergency override.
	envChannel = "NISTRU_AUTOUPDATE_CHANNEL"
	// Deprecated: prefer the [plugins.autoupdate] TOML section. Env still wins
	// as an emergency override.
	envInterval = "NISTRU_AUTOUPDATE_INTERVAL"
	// Deprecated: prefer the [plugins.autoupdate] TOML section. Env still wins
	// as an emergency override.
	envDisable = "NISTRU_AUTOUPDATE_DISABLE"
)

// Installer is the seam T7 will fill with the real binary swap. The noop
// implementation in noop_installer.go is the default until then.
type Installer interface {
	Install(ctx context.Context, host *plugin.Host, rel Release, cur string) error
	Rollback(ctx context.Context, host *plugin.Host) error
}

// Plugin is the in-process auto-update plugin. The zero value is not useful;
// construct instances via New().
type Plugin struct {
	name string

	repo     string
	channel  string
	current  string
	interval time.Duration
	disabled bool

	// optionSet records which fields were explicitly set via constructor
	// options. OnConfig and applyEnv honour this bitset so options remain
	// the highest-precedence source.
	optionSet sourceBits

	// configSet records which fields currently carry a value installed by a
	// prior OnConfig call. On reload with nil/empty raw (the user removed
	// the [plugins.autoupdate] section), OnConfig uses this set to know
	// exactly which fields to revert to defaults, leaving option- and
	// env-owned fields alone. Writes gated by p.mu like every other mutable
	// field on this struct.
	configSet sourceBits

	client    *http.Client
	installer Installer
	now       func() time.Time
	versionFn func() string
	statePath string

	mu        sync.Mutex
	host      *plugin.Host
	state     State
	checker   *checker
	shutdown  bool
	ctxCancel context.CancelFunc
}

// sourceBits tracks which fields were set via constructor options. Precedence
// (low → high): defaults → config (OnConfig) → env (applyEnv) → options.
// A true bit means the field was set by an option; config and env leave it
// alone.
type sourceBits struct {
	repo, channel, interval, disabled bool
}

// Option configures Plugin at construction.
type Option func(*Plugin)

// WithRepo overrides the "owner/repo" string polled for releases. Marks the
// field as option-owned so OnConfig and env vars cannot overwrite it.
func WithRepo(repo string) Option {
	return func(p *Plugin) {
		p.repo = repo
		p.optionSet.repo = true
	}
}

// WithHTTPClient injects a custom *http.Client (tests point it at an
// httptest.Server via a rewriting transport).
func WithHTTPClient(c *http.Client) Option {
	return func(p *Plugin) { p.client = c }
}

// WithInstaller swaps in a real installer once T7 lands. Tests use this to
// assert the install/rollback dispatch path without touching the binary.
func WithInstaller(i Installer) Option {
	return func(p *Plugin) { p.installer = i }
}

// WithClock injects a now-func so time-dependent behaviour is deterministic
// in tests. Defaults to time.Now.
func WithClock(now func() time.Time) Option {
	return func(p *Plugin) {
		if now != nil {
			p.now = now
		}
	}
}

// WithInterval overrides the ticker period. Tests use very short intervals
// to exercise the loop without hanging. Marks the field as option-owned so
// OnConfig and env vars cannot overwrite it.
func WithInterval(d time.Duration) Option {
	return func(p *Plugin) {
		if d > 0 {
			p.interval = d
			p.optionSet.interval = true
		}
	}
}

// WithStatePath overrides the on-disk state file location. Tests pass
// t.TempDir() paths so nothing leaks into the real user config dir.
func WithStatePath(path string) Option {
	return func(p *Plugin) { p.statePath = path }
}

// WithCurrent overrides the detected "current version" used for comparisons.
// Tests use this seam to drive the "newer/older/equal" branches without
// depending on runtime/debug.ReadBuildInfo.
func WithCurrent(v string) Option {
	return func(p *Plugin) { p.current = v }
}

// WithVersionFunc injects a function that resolves the running binary's
// version at Initialize time. Tests use this so reconciliation logic can
// be driven without depending on ldflags or ReadBuildInfo. When unset,
// the plugin falls back to the package-level Current().
func WithVersionFunc(fn func() string) Option {
	return func(p *Plugin) {
		if fn != nil {
			p.versionFn = fn
		}
	}
}

// New returns a configured plugin with defaults plus any supplied options
// applied. Precedence (low → high) is: defaults → config (via OnConfig) →
// env vars (applied in handleInitialize) → constructor options. Env and
// config reads are deferred until handleInitialize so options always win
// over both. New never performs I/O; the background goroutine and state
// load run inside OnEvent(Initialize).
func New(opts ...Option) *Plugin {
	p := &Plugin{
		name:     "autoupdate",
		repo:     defaultRepo,
		interval: defaultInterval,
		client:   &http.Client{Timeout: 10 * time.Second},
		now:      time.Now,
		current:  Current(),
	}
	p.installer = NewInstaller(WithStateUpdater(p.updateState))

	for _, opt := range opts {
		opt(p)
	}
	return p
}

// OnConfig applies config values to any field not already set via a
// constructor option. Env vars still win: they are applied after OnConfig
// in handleInitialize. Safe to call before or after handleInitialize; if
// called after, fields changed here only take effect on the next check.
//
// Nil/empty-raw reset: when raw is nil or zero-length, the plugin was
// reloaded with [plugins.autoupdate] absent from config. Any field that was
// previously set by OnConfig (tracked in p.configSet) is reverted to its
// construction-time default, so removing config cleanly restores defaults
// without leaking stale overrides into the next check. Fields set via
// constructor options or env vars are left untouched — they sit higher in
// the precedence ladder and OnConfig never owned them.
func (p *Plugin) OnConfig(raw json.RawMessage) error {
	if len(raw) == 0 {
		p.resetConfigFields()
		return nil
	}
	// Shape is intentionally loose: unknown fields are ignored silently so
	// future keys don't break older plugins.
	var cfg struct {
		Repo     *string `json:"repo,omitempty"`
		Channel  *string `json:"channel,omitempty"`
		Interval *string `json:"interval,omitempty"`
		Disable  *bool   `json:"disable,omitempty"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// For each field, track whether this call installed a value. A field
	// whose key is absent from the current raw reverts to its default if a
	// prior OnConfig had set it — mirroring the nil-raw reset path but
	// scoped to just that field.
	if p.optionSet.repo {
		// Option-owned; OnConfig has no say.
	} else if cfg.Repo != nil {
		if v := strings.TrimSpace(*cfg.Repo); v != "" {
			p.repo = v
			p.configSet.repo = true
		} else if p.configSet.repo {
			// Empty/whitespace value after previously setting via config —
			// treat as a reset so users can clear a stale override by
			// writing repo = "" without deleting the whole section.
			p.repo = defaultRepo
			p.configSet.repo = false
		}
	} else if p.configSet.repo {
		p.repo = defaultRepo
		p.configSet.repo = false
	}

	if p.optionSet.channel {
		// Option-owned.
	} else if cfg.Channel != nil {
		p.channel = strings.TrimSpace(*cfg.Channel)
		p.configSet.channel = true
	} else if p.configSet.channel {
		p.channel = ""
		p.configSet.channel = false
	}

	if p.optionSet.interval {
		// Option-owned.
	} else if cfg.Interval != nil {
		s := strings.TrimSpace(*cfg.Interval)
		if s != "" {
			if d, err := time.ParseDuration(s); err == nil && d > 0 {
				p.interval = d
				p.configSet.interval = true
			} else {
				fmt.Fprintf(os.Stderr, "plugin/autoupdate: bad interval %q in config: %v\n", s, err)
			}
		}
	} else if p.configSet.interval {
		p.interval = defaultInterval
		p.configSet.interval = false
	}

	if p.optionSet.disabled {
		// Option-owned.
	} else if cfg.Disable != nil {
		p.disabled = *cfg.Disable
		p.configSet.disabled = true
	} else if p.configSet.disabled {
		p.disabled = false
		p.configSet.disabled = false
	}
	return nil
}

// resetConfigFields reverts every field currently flagged as config-owned
// (p.configSet) back to its construction-time default, leaving option- and
// env-owned fields untouched. Called from OnConfig when raw is nil/empty,
// which signals that the [plugins.autoupdate] section was removed on reload.
// Constants like defaultRepo and defaultInterval are the source of truth;
// the empty channel and disabled=false match what New() installs before
// options are applied.
func (p *Plugin) resetConfigFields() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.configSet.repo && !p.optionSet.repo {
		p.repo = defaultRepo
	}
	if p.configSet.channel && !p.optionSet.channel {
		p.channel = ""
	}
	if p.configSet.interval && !p.optionSet.interval {
		p.interval = defaultInterval
	}
	if p.configSet.disabled && !p.optionSet.disabled {
		p.disabled = false
	}
	// Clear the bits regardless of option ownership — a field we could not
	// reset is one that the option layer now owns anyway, and leaving the
	// bit set would cause the next OnConfig to double-reset it.
	p.configSet = sourceBits{}
}

// applyEnv reads the deprecated NISTRU_AUTOUPDATE_* env vars and overlays
// them onto any field that was not set via a constructor option. Called at
// the start of handleInitialize so env always wins over OnConfig but loses
// to options. Callers must not hold p.mu — this helper acquires it.
func applyEnv(p *Plugin) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.optionSet.repo {
		if v := strings.TrimSpace(os.Getenv(envRepo)); v != "" {
			p.repo = v
		}
	}
	if !p.optionSet.channel {
		if v := strings.TrimSpace(os.Getenv(envChannel)); v != "" {
			p.channel = v
		}
	}
	if !p.optionSet.interval {
		if v := strings.TrimSpace(os.Getenv(envInterval)); v != "" {
			if d, err := time.ParseDuration(v); err == nil && d > 0 {
				p.interval = d
			}
		}
	}
	if !p.optionSet.disabled {
		if os.Getenv(envDisable) == "1" {
			p.disabled = true
		}
	}
}

// Name implements plugin.Plugin.
func (p *Plugin) Name() string { return p.name }

// Activation implements plugin.Plugin. The checker must come up at editor
// launch so the status bar populates before the user interacts with
// anything, hence onStart.
func (p *Plugin) Activation() []string { return []string{"onStart"} }

// SetHost implements plugin.HostAware. The host wires HostAware plugins
// exactly once, before Initialize is dispatched.
func (p *Plugin) SetHost(h *plugin.Host) {
	p.mu.Lock()
	p.host = h
	p.mu.Unlock()
}

// OnEvent implements plugin.Plugin. All effects are empty; side effects flow
// through the host's PostNotif channel so they arrive on the UI goroutine
// via the normal inbound queue.
func (p *Plugin) OnEvent(event any) []plugin.Effect {
	switch ev := event.(type) {
	case plugin.Initialize:
		p.handleInitialize()
		return nil
	case plugin.ExecuteCommand:
		p.handleExecute(ev)
		return nil
	case plugin.Shutdown:
		p.stopChecker()
		return nil
	}
	return nil
}

// Shutdown implements plugin.Plugin. Idempotent and safe to call without
// a prior Shutdown event.
func (p *Plugin) Shutdown() error {
	p.stopChecker()
	return nil
}

// handleInitialize loads persisted state, registers palette commands, and
// spawns the checker goroutine. When NISTRU_AUTOUPDATE_DISABLE=1 we still
// register commands (so the palette surfaces them and users can dispatch
// install/rollback manually), but skip the background checker — "disabled"
// refers to the polling loop, not the whole feature.
//
// applyEnv runs first so env vars overlay whatever OnConfig (fired earlier
// from the host's Initialize dispatch) installed. Constructor options stay
// untouched because the optionSet bits gate every write.
func (p *Plugin) handleInitialize() {
	applyEnv(p)

	// Idempotency: handleInitialize may fire a second time after a settings
	// reload (Host.ReEmitInitialize). Stop any prior checker goroutine before
	// we potentially spawn a new one so reloads do not leak goroutines, and
	// so a user flipping [plugins.autoupdate].disable = true in config then
	// reloading actually silences the background poller. stopCheckerForReload
	// does not flip p.shutdown — that is reserved for the permanent-shutdown
	// path — so the re-spawn below still runs.
	p.stopCheckerForReload()

	p.mu.Lock()
	if p.shutdown {
		p.mu.Unlock()
		return
	}

	// Resolve state path if not injected.
	if p.statePath == "" {
		if sp, err := StatePath(); err == nil {
			p.statePath = sp
		}
	}

	// Load persisted state; LoadState is forgiving and never errors on
	// missing/corrupt files.
	if p.statePath != "" {
		if st, err := LoadState(p.statePath); err == nil {
			p.state = st
		}
	}
	// Channel precedence: explicit env > persisted state > default.
	if p.channel != "" {
		p.state.Channel = p.channel
	} else if p.state.Channel == "" {
		p.state.Channel = DefaultChannel()
	}
	host := p.host
	pendingVersion := p.state.PendingRestartVersion
	prevPath := p.state.PrevBinaryPath

	if p.disabled {
		p.mu.Unlock()
		// Always register palette commands, even when the background checker
		// is disabled via env — the user should still be able to run
		// install/rollback on demand without flipping the env var.
		registerCommands(host)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	p.ctxCancel = cancel

	c := newChecker(p)
	p.checker = c
	p.mu.Unlock()

	// Reconcile a prior install that has since been applied. If the running
	// binary's version matches the pending one, the user has already
	// restarted — clear the pending state and best-effort remove the .prev
	// sibling so subsequent sessions don't leak it.
	p.finalizePendingRestart(pendingVersion, prevPath)

	registerCommands(host)
	c.start(ctx)
}

// finalizePendingRestart clears PendingRestartVersion and PrevBinaryPath
// when the running binary's version matches the pending one. Removes the
// .prev file if it still exists — this is the "success finalisation" that
// should have happened on restart. Best-effort: any error is dropped
// silently because the user cannot act on it.
func (p *Plugin) finalizePendingRestart(pendingVersion, prevPath string) {
	if pendingVersion == "" {
		return
	}
	running := p.runningVersion()
	if CompareVersions(running, pendingVersion) != 0 {
		// Still running the old binary — the restart hasn't happened yet,
		// leave everything alone so rollback keeps working.
		return
	}
	_ = p.updateState(func(s *State) {
		s.PendingRestartVersion = ""
		s.PrevBinaryPath = ""
	})
	if prevPath != "" {
		if _, err := os.Stat(prevPath); err == nil {
			_ = os.Remove(prevPath)
		}
	}
}

// runningVersion returns the current binary's version string. Prefers the
// injected versionFn seam over the package-level Current() so tests can
// drive reconciliation deterministically.
func (p *Plugin) runningVersion() string {
	if p.versionFn != nil {
		return p.versionFn()
	}
	return Current()
}

// registerCommands fires the five commands/register notifications. The host
// applies them synchronously inside PostNotif, so callers see them as
// visible immediately.
func registerCommands(host *plugin.Host) {
	if host == nil {
		return
	}
	cmds := []struct {
		id, title string
	}{
		{"autoupdate:check", "Auto-update: check now"},
		{"autoupdate:install", "Auto-update: install pending update"},
		{"autoupdate:rollback", "Auto-update: rollback last install"},
		{"autoupdate:switch-channel", "Auto-update: toggle release ↔ dev channel"},
		{"autoupdate:release-notes", "Auto-update: show latest release notes"},
	}
	for _, c := range cmds {
		_ = host.PostNotif("autoupdate", "commands/register", map[string]string{
			"id":    c.id,
			"title": c.title,
		})
	}
}

// handleExecute dispatches ExecuteCommand events to their handlers. Each
// handler is responsible for surfacing errors via PostNotif — OnEvent never
// returns effects, so we cannot propagate them through the return value.
func (p *Plugin) handleExecute(ev plugin.ExecuteCommand) {
	switch ev.ID {
	case "autoupdate:check":
		p.cmdCheck()
	case "autoupdate:install":
		p.cmdInstall()
	case "autoupdate:rollback":
		p.cmdRollback()
	case "autoupdate:switch-channel":
		p.cmdSwitchChannel()
	case "autoupdate:release-notes":
		p.cmdReleaseNotes()
	}
}

// cmdCheck nudges the checker goroutine. A no-op if the checker is not
// running (e.g. plugin disabled via env var).
func (p *Plugin) cmdCheck() {
	p.mu.Lock()
	c := p.checker
	p.mu.Unlock()
	if c == nil {
		return
	}
	c.nudge()
}

// cmdInstall dispatches to the configured installer with the last-seen
// release. Errors are posted as ui/notify.
func (p *Plugin) cmdInstall() {
	p.mu.Lock()
	host, inst, c, cur := p.host, p.installer, p.checker, p.current
	p.mu.Unlock()
	if inst == nil {
		return
	}
	var rel Release
	if c != nil {
		if r := c.lastRelease.Load(); r != nil {
			rel = *r
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := inst.Install(ctx, host, rel, cur); err != nil {
		p.postError(host, err.Error())
	}
}

// cmdRollback delegates to Installer.Rollback. Errors are surfaced via
// ui/notify just like cmdInstall.
func (p *Plugin) cmdRollback() {
	p.mu.Lock()
	host, inst := p.host, p.installer
	p.mu.Unlock()
	if inst == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := inst.Rollback(ctx, host); err != nil {
		p.postError(host, err.Error())
	}
}

// cmdSwitchChannel toggles state.Channel between "release" and "dev",
// persists the new value, confirms via ui/notify, and nudges the checker.
// All state mutation flows through updateState so a concurrent checker
// tick or install cannot clobber the channel switch.
func (p *Plugin) cmdSwitchChannel() {
	var next string
	err := p.updateState(func(s *State) {
		cur := s.Channel
		if cur == "" {
			cur = DefaultChannel()
		}
		next = "dev"
		if cur == "dev" {
			next = "release"
		}
		s.Channel = next
	})

	p.mu.Lock()
	host := p.host
	c := p.checker
	p.mu.Unlock()

	if err != nil {
		p.postError(host, "autoupdate: save channel: "+err.Error())
	}
	if host != nil {
		_ = host.PostNotif("autoupdate", "ui/notify", map[string]string{
			"level":   "info",
			"message": "auto-update channel: " + next,
		})
	}
	if c != nil {
		c.nudge()
	}
}

// cmdReleaseNotes writes the last-known release's body to a tempfile and
// asks the editor to open it. If no notes are available, posts a notify.
func (p *Plugin) cmdReleaseNotes() {
	p.mu.Lock()
	host, c := p.host, p.checker
	p.mu.Unlock()

	var rel Release
	if c != nil {
		if r := c.lastRelease.Load(); r != nil {
			rel = *r
		}
	}
	body := strings.TrimSpace(rel.Body)
	if body == "" {
		if host != nil {
			_ = host.PostNotif("autoupdate", "ui/notify", map[string]string{
				"level":   "info",
				"message": "no release notes available",
			})
		}
		return
	}

	path, err := writeReleaseNotes(rel)
	if err != nil {
		p.postError(host, "autoupdate: write release notes: "+err.Error())
		return
	}

	// OpenFile is a plugin.Effect, but OnEvent for ExecuteCommand returns no
	// effects (we're off the ExecuteCommand return path). Route it through
	// PostNotif so the model's inbound queue picks it up alongside other
	// asynchronous effects. The editor's model treats the pair as a hint to
	// open the file; tests assert on the notification directly.
	if host != nil {
		_ = host.PostNotif("autoupdate", "editor/openFile", map[string]string{
			"path": path,
		})
	}
}

// writeReleaseNotes persists rel.Body to a stable tempfile under the OS
// temp dir and returns its absolute path. The file is intentionally not
// removed; opening it in the editor requires it to stick around.
func writeReleaseNotes(rel Release) (string, error) {
	dir := os.TempDir()
	name := "nistru-release-" + sanitizeTag(rel.TagName) + ".md"
	path := dir + string(os.PathSeparator) + name
	content := rel.Body
	if title := strings.TrimSpace(rel.Name); title != "" {
		content = "# " + title + "\n\n" + content
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// sanitizeTag produces a filesystem-safe variant of a release tag. Anything
// outside [A-Za-z0-9._-] collapses to "_".
func sanitizeTag(tag string) string {
	if tag == "" {
		return "unknown"
	}
	var b strings.Builder
	b.Grow(len(tag))
	for _, r := range tag {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// postError surfaces err as an error-level ui/notify. A helper so every
// command site stays one-liner.
func (p *Plugin) postError(host *plugin.Host, msg string) {
	if host == nil {
		return
	}
	_ = host.PostNotif("autoupdate", "ui/notify", map[string]string{
		"level":   "error",
		"message": msg,
	})
}

// snapshotState returns a copy of the plugin's current state. Called by
// the checker goroutine, which must not hold p.mu while performing I/O.
func (p *Plugin) snapshotState() State {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
}

// updateState atomically reloads state from disk, applies mut, persists
// the result, and updates the in-memory cache. Serialised via p.mu so
// concurrent checker / install / rollback mutations cannot clobber each
// other. The disk reload is intentional: any writer (including a stale
// snapshot inside the checker goroutine) sees the most-recent persisted
// state before its mutation is applied, so two concurrent updates to
// disjoint fields compose rather than clobber.
//
// Returns the SaveState error, if any. An in-memory update still happens
// even if persistence fails — callers treat state as advisory.
func (p *Plugin) updateState(mut func(*State)) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.state
	if p.statePath != "" {
		if loaded, err := LoadState(p.statePath); err == nil {
			st = loaded
		}
	}
	if mut != nil {
		mut(&st)
	}
	p.state = st
	if p.statePath == "" {
		return nil
	}
	return SaveState(p.statePath, st)
}

// stopChecker cancels the background goroutine and waits for it to exit
// (up to shutdownGrace). Idempotent: a second call is a no-op.
//
// If the installer implements the optional `cleaner` interface, it is
// invoked once after the checker is stopped so stale ".prev" files from a
// prior successful restart can be garbage-collected. Errors from Cleanup
// are swallowed — it is always best-effort.
func (p *Plugin) stopChecker() {
	p.mu.Lock()
	if p.shutdown {
		p.mu.Unlock()
		return
	}
	p.shutdown = true
	cancel := p.ctxCancel
	c := p.checker
	p.checker = nil
	p.ctxCancel = nil
	inst := p.installer
	p.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if c != nil {
		c.cancel(shutdownGrace)
	}
	if cl, ok := inst.(cleaner); ok {
		ctx, cancelCleanup := context.WithTimeout(context.Background(), shutdownGrace)
		_ = cl.Cleanup(ctx)
		cancelCleanup()
	}
}

// stopCheckerForReload cancels the background goroutine and waits for it to
// exit (up to shutdownGrace) without flipping p.shutdown. Used from
// handleInitialize to guarantee idempotency under Host.ReEmitInitialize so
// a settings reload never leaks a goroutine or keeps a stale poller alive
// after the user flipped [plugins.autoupdate].disable = true.
//
// Safe to call when no checker has ever been spawned — it is a no-op in
// that case. Does not invoke the installer's Cleanup hook because this
// path is not a permanent shutdown.
func (p *Plugin) stopCheckerForReload() {
	p.mu.Lock()
	if p.shutdown {
		p.mu.Unlock()
		return
	}
	cancel := p.ctxCancel
	c := p.checker
	p.checker = nil
	p.ctxCancel = nil
	p.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if c != nil {
		c.cancel(shutdownGrace)
	}
}

// cleaner is the optional interface RealInstaller implements so the plugin
// can garbage-collect stale ".prev" binaries on Shutdown.
type cleaner interface {
	Cleanup(ctx context.Context) error
}

// Compile-time assertions so a missing interface surfaces at build time.
var (
	_ plugin.Plugin         = (*Plugin)(nil)
	_ plugin.HostAware      = (*Plugin)(nil)
	_ plugin.ConfigReceiver = (*Plugin)(nil)
)
