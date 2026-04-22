package autoupdate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/savimcio/nistru/plugin"
)

// -----------------------------------------------------------------------------
// Shared test helpers (also used by checker_test.go).

// newTestHost builds a plugin.Host with p registered as the only in-proc
// plugin. The cleanup closure stops the plugin (so its goroutine exits)
// before any t.TempDir registered earlier in the caller is removed.
func newTestHost(t *testing.T, p *Plugin) *plugin.Host {
	t.Helper()
	reg := plugin.NewRegistry()
	reg.RegisterInProc(p)
	h := plugin.NewHost(reg)
	if err := h.Start(""); err != nil {
		t.Fatalf("host.Start: %v", err)
	}
	t.Cleanup(func() {
		// Stop the plugin first. Host.Shutdown only dispatches Plugin.Shutdown
		// for activated plugins, and we activate via direct OnEvent calls,
		// so the host's own bookkeeping would otherwise skip us.
		_ = p.Shutdown()
		_ = h.Shutdown(time.Second)
	})
	return h
}

// nextNotif returns the next PluginNotifMsg on h's inbound channel, or nil
// on timeout. One goroutine per call is fine — these tests drain tens of
// messages at most.
func nextNotif(h *plugin.Host, timeout time.Duration) *plugin.PluginNotifMsg {
	out := make(chan plugin.PluginMsg, 1)
	go func() {
		if v := h.Recv()(); v != nil {
			if m, ok := v.(plugin.PluginMsg); ok {
				out <- m
			}
		}
		close(out)
	}()
	select {
	case m, ok := <-out:
		if !ok {
			return nil
		}
		if n, ok := m.(plugin.PluginNotifMsg); ok {
			return &n
		}
		return nil
	case <-time.After(timeout):
		return nil
	}
}

// waitForNotif pulls messages off h's inbound channel until match(n) returns
// true or timeout elapses. Returns the matching notification or nil.
func waitForNotif(h *plugin.Host, timeout time.Duration, match func(plugin.PluginNotifMsg) bool) *plugin.PluginNotifMsg {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		n := nextNotif(h, time.Until(deadline))
		if n == nil {
			return nil
		}
		if match(*n) {
			return n
		}
	}
	return nil
}

// waitUntil polls pred until true or timeout. Fails the test on timeout.
func waitUntil(t *testing.T, timeout time.Duration, pred func() bool) {
	t.Helper()
	const pollEvery = 10 * time.Millisecond
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(pollEvery)
	}
	if pred() {
		return
	}
	t.Fatalf("waitUntil: condition not met within %s", timeout)
}

// newReleaseServerJSON serves body at every request and counts hits.
func newReleaseServerJSON(t *testing.T, body string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// newTestPlugin builds a Plugin pointed at srv with a short interval and
// a tempdir state file. Env overrides are cleared so the test is hermetic.
// rewriteTransport is shared with github_test.go.
func newTestPlugin(t *testing.T, srv *httptest.Server, current string, opts ...Option) *Plugin {
	t.Helper()
	t.Setenv(envDisable, "")
	t.Setenv(envRepo, "")
	t.Setenv(envChannel, "")
	t.Setenv(envInterval, "")

	statePath := filepath.Join(t.TempDir(), "state.json")
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: rewriteTransport{base: http.DefaultTransport, toURL: srv.URL},
	}
	base := []Option{
		WithRepo("owner/repo"),
		WithHTTPClient(client),
		WithInterval(50 * time.Millisecond),
		WithStatePath(statePath),
		WithCurrent(current),
	}
	base = append(base, opts...)
	return New(base...)
}

// -----------------------------------------------------------------------------
// Plugin-level tests (env gating, command registration, Execute dispatch,
// shutdown). Checker-specific tests live in checker_test.go.

func TestDisableEnvVarShortCircuits(t *testing.T) {
	t.Setenv(envDisable, "1")
	p := New(WithRepo("owner/repo"), WithInterval(10*time.Millisecond))
	_ = newTestHost(t, p)

	_ = p.OnEvent(plugin.Initialize{RootPath: t.TempDir()})

	p.mu.Lock()
	gotChecker := p.checker
	p.mu.Unlock()
	if gotChecker != nil {
		t.Fatalf("checker spawned despite NISTRU_AUTOUPDATE_DISABLE=1")
	}
}

func TestInitializeRegistersCommands(t *testing.T) {
	srv, _ := newReleaseServerJSON(t, `[]`)
	p := newTestPlugin(t, srv, "v0.1.0")
	h := newTestHost(t, p)

	_ = p.OnEvent(plugin.Initialize{RootPath: t.TempDir()})

	// PostNotif's handleInternal applies commands/register synchronously,
	// so Host.Commands() sees them immediately.
	want := []string{
		"autoupdate:check",
		"autoupdate:install",
		"autoupdate:rollback",
		"autoupdate:switch-channel",
		"autoupdate:release-notes",
	}
	cmds := h.Commands()
	for _, id := range want {
		ref, ok := cmds[id]
		if !ok {
			t.Fatalf("command %q not registered; have %+v", id, cmds)
		}
		if ref.Plugin != "autoupdate" {
			t.Fatalf("command %q owner = %q, want autoupdate", id, ref.Plugin)
		}
	}
}

func TestExecuteCheckTriggersFetch(t *testing.T) {
	srv, hits := newReleaseServerJSON(t, `[]`)
	p := newTestPlugin(t, srv, "v0.1.0", WithInterval(time.Hour))
	_ = newTestHost(t, p)

	_ = p.OnEvent(plugin.Initialize{RootPath: t.TempDir()})
	waitUntil(t, 2*time.Second, func() bool { return hits.Load() >= 1 })
	baseline := hits.Load()

	_ = p.OnEvent(plugin.ExecuteCommand{ID: "autoupdate:check"})
	waitUntil(t, 2*time.Second, func() bool { return hits.Load() > baseline })
}

func TestExecuteSwitchChannelPersists(t *testing.T) {
	srv, _ := newReleaseServerJSON(t, `[]`)
	p := newTestPlugin(t, srv, "v0.1.0", WithInterval(time.Hour))
	_ = newTestHost(t, p)

	_ = p.OnEvent(plugin.Initialize{RootPath: t.TempDir()})

	p.mu.Lock()
	statePath := p.statePath
	p.mu.Unlock()

	_ = p.OnEvent(plugin.ExecuteCommand{ID: "autoupdate:switch-channel"})
	waitUntil(t, 2*time.Second, func() bool {
		st, err := LoadState(statePath)
		return err == nil && st.Channel == "dev"
	})
	_ = p.OnEvent(plugin.ExecuteCommand{ID: "autoupdate:switch-channel"})
	waitUntil(t, 2*time.Second, func() bool {
		st, err := LoadState(statePath)
		return err == nil && st.Channel == "release"
	})
}

// recordingInstaller captures Install/Rollback invocations.
type recordingInstaller struct {
	mu           sync.Mutex
	installCalls []struct {
		Rel Release
		Cur string
	}
	rollbacks int
}

func (r *recordingInstaller) Install(_ context.Context, _ *plugin.Host, rel Release, cur string) error {
	r.mu.Lock()
	r.installCalls = append(r.installCalls, struct {
		Rel Release
		Cur string
	}{Rel: rel, Cur: cur})
	r.mu.Unlock()
	return nil
}

func (r *recordingInstaller) Rollback(_ context.Context, _ *plugin.Host) error {
	r.mu.Lock()
	r.rollbacks++
	r.mu.Unlock()
	return nil
}

func TestExecuteInstallCallsInstaller(t *testing.T) {
	srv, _ := newReleaseServerJSON(t, oneStableNewerJSON)
	inst := &recordingInstaller{}
	p := newTestPlugin(t, srv, "v0.1.0",
		WithInstaller(inst),
		WithInterval(time.Hour),
	)
	_ = newTestHost(t, p)

	_ = p.OnEvent(plugin.Initialize{RootPath: t.TempDir()})
	// Wait until the checker has cached a Release via its initial tick.
	waitUntil(t, 2*time.Second, func() bool {
		p.mu.Lock()
		c := p.checker
		p.mu.Unlock()
		return c != nil && c.lastRelease.Load() != nil
	})

	_ = p.OnEvent(plugin.ExecuteCommand{ID: "autoupdate:install"})
	waitUntil(t, 2*time.Second, func() bool {
		inst.mu.Lock()
		defer inst.mu.Unlock()
		return len(inst.installCalls) == 1
	})
	inst.mu.Lock()
	got := inst.installCalls[0]
	inst.mu.Unlock()
	if got.Cur != "v0.1.0" {
		t.Fatalf("installer.cur = %q, want v0.1.0", got.Cur)
	}
	if got.Rel.TagName != "v99.0.0" {
		t.Fatalf("installer.rel.TagName = %q, want v99.0.0", got.Rel.TagName)
	}

	_ = p.OnEvent(plugin.ExecuteCommand{ID: "autoupdate:rollback"})
	waitUntil(t, 2*time.Second, func() bool {
		inst.mu.Lock()
		defer inst.mu.Unlock()
		return inst.rollbacks == 1
	})
}

// TestPlugin_ConfigPrecedence exercises the three-way precedence ladder for
// the autoupdate plugin's configuration sources: defaults < config (via
// OnConfig) < env < constructor options.
//
// Every case drives the same flow: build a Plugin, optionally set env and/or
// call OnConfig, then run handleInitialize (which applies env). The final
// field values on p are the ground truth. A disabled checker (or a stub
// HTTP transport + a tempdir state path) keeps these tests hermetic — the
// background goroutine must not hit the network or leak past t.Cleanup.
func TestPlugin_ConfigPrecedence(t *testing.T) {
	type fields struct {
		repo     string
		channel  string
		interval time.Duration
		disabled bool
	}

	cases := []struct {
		name      string
		envRepo   string // "" means leave unset
		envChan   string
		envIntvl  string
		envDis    string
		config    string // raw JSON for OnConfig; "" means skip
		extraOpts func(t *testing.T) []Option
		want      fields
	}{
		{
			name:   "config_alone",
			config: `{"repo":"c/r","channel":"dev","interval":"2h","disable":true}`,
			want: fields{
				repo:     "c/r",
				channel:  "dev",
				interval: 2 * time.Hour,
				disabled: true,
			},
		},
		{
			name:    "env_wins_over_config",
			envRepo: "env/r",
			config:  `{"repo":"cfg/r"}`,
			want: fields{
				repo:     "env/r",
				channel:  "",
				interval: defaultInterval,
				disabled: false,
			},
		},
		{
			name:    "option_wins_over_both",
			envRepo: "env/r",
			config:  `{"repo":"cfg/r"}`,
			extraOpts: func(_ *testing.T) []Option {
				return []Option{WithRepo("opt/r")}
			},
			want: fields{
				repo:     "opt/r",
				channel:  "",
				interval: defaultInterval,
				disabled: false,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Scrub env so parallel/serial runs are hermetic; then set per-case.
			t.Setenv(envRepo, tc.envRepo)
			t.Setenv(envChannel, tc.envChan)
			t.Setenv(envInterval, tc.envIntvl)
			t.Setenv(envDisable, tc.envDis)

			// Stub transport so the checker goroutine doesn't try the real
			// network even when the config doesn't disable it. An empty
			// JSON array is a valid /releases payload.
			client := &http.Client{
				Transport: &stubTransport{body: []byte(`[]`)},
				Timeout:   5 * time.Second,
			}

			statePath := filepath.Join(t.TempDir(), "s.json")
			opts := []Option{
				WithHTTPClient(client),
				WithStatePath(statePath),
				WithCurrent("v0.1.0"),
			}
			if tc.extraOpts != nil {
				opts = append(opts, tc.extraOpts(t)...)
			}

			p := New(opts...)
			// Teardown first so a half-initialised plugin still stops cleanly.
			t.Cleanup(func() { _ = p.Shutdown() })

			if tc.config != "" {
				if err := p.OnConfig(json.RawMessage(tc.config)); err != nil {
					t.Fatalf("OnConfig: %v", err)
				}
			}

			// Drive env application + (possibly) checker spawn. The stub
			// transport + tempdir state path keep it hermetic.
			p.handleInitialize()

			p.mu.Lock()
			got := fields{
				repo:     p.repo,
				channel:  p.channel,
				interval: p.interval,
				disabled: p.disabled,
			}
			p.mu.Unlock()

			if got != tc.want {
				t.Fatalf("fields = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestPlugin_Initialize_Idempotent verifies that a second handleInitialize
// call — as happens after Host.ReEmitInitialize on a settings reload — does
// not leak the previous checker goroutine. The earlier checker must be
// cancelled and its goroutine exited before the new one spawns.
func TestPlugin_Initialize_Idempotent(t *testing.T) {
	srv, _ := newReleaseServerJSON(t, `[]`)
	p := newTestPlugin(t, srv, "v0.1.0")
	_ = newTestHost(t, p)

	// First init: spawns checker #1.
	_ = p.OnEvent(plugin.Initialize{RootPath: t.TempDir()})
	p.mu.Lock()
	first := p.checker
	p.mu.Unlock()
	if first == nil {
		t.Fatalf("first handleInitialize did not spawn a checker")
	}

	// Second init: must stop checker #1 and spawn checker #2. We wait on
	// first.done to prove the goroutine actually exited — a leak would
	// show up as this channel never closing.
	_ = p.OnEvent(plugin.Initialize{RootPath: t.TempDir()})
	select {
	case <-first.done:
	case <-time.After(2 * shutdownGrace):
		t.Fatalf("prior checker goroutine did not exit after re-initialize")
	}

	p.mu.Lock()
	second := p.checker
	p.mu.Unlock()
	if second == nil {
		t.Fatalf("second handleInitialize did not spawn a fresh checker")
	}
	if second == first {
		t.Fatalf("second handleInitialize reused the stopped checker pointer")
	}

	// Shutdown must cleanly stop checker #2 and leave p.checker nil.
	if err := p.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case <-second.done:
	case <-time.After(2 * shutdownGrace):
		t.Fatalf("current checker goroutine did not exit on Shutdown")
	}
	p.mu.Lock()
	leaked := p.checker
	p.mu.Unlock()
	if leaked != nil {
		t.Fatalf("Shutdown did not clear p.checker; got %p", leaked)
	}
}

// TestPlugin_Initialize_Idempotent_DisabledAfterReload verifies the second
// fix motivation: if the user flips `disable = true` in config and reloads,
// the old checker is stopped and no new one is spawned.
func TestPlugin_Initialize_Idempotent_DisabledAfterReload(t *testing.T) {
	srv, _ := newReleaseServerJSON(t, `[]`)
	p := newTestPlugin(t, srv, "v0.1.0")
	_ = newTestHost(t, p)

	_ = p.OnEvent(plugin.Initialize{RootPath: t.TempDir()})
	p.mu.Lock()
	first := p.checker
	p.mu.Unlock()
	if first == nil {
		t.Fatalf("first handleInitialize did not spawn a checker")
	}

	// Simulate a reload where config now disables the poller.
	if err := p.OnConfig(json.RawMessage(`{"disable":true}`)); err != nil {
		t.Fatalf("OnConfig: %v", err)
	}
	_ = p.OnEvent(plugin.Initialize{RootPath: t.TempDir()})

	select {
	case <-first.done:
	case <-time.After(2 * shutdownGrace):
		t.Fatalf("prior checker goroutine did not exit after disable+reload")
	}
	p.mu.Lock()
	stillRunning := p.checker
	p.mu.Unlock()
	if stillRunning != nil {
		t.Fatalf("checker still running after disable=true reload: %p", stillRunning)
	}
}

// TestPlugin_OnConfig_Nil_ResetsConfigFields covers the Host.ReEmitInitialize
// reload path where a user has removed the [plugins.autoupdate] section
// entirely: the host now invokes OnConfig(nil) and the plugin must revert
// every field previously set via config back to its construction-time
// default, leaving option- and env-owned fields alone.
func TestPlugin_OnConfig_Nil_ResetsConfigFields(t *testing.T) {
	// Scrub env so this test is hermetic — env vars would otherwise pin
	// fields above the config layer and defeat the reset check.
	t.Setenv(envDisable, "")
	t.Setenv(envRepo, "")
	t.Setenv(envChannel, "")
	t.Setenv(envInterval, "")

	p := New()
	t.Cleanup(func() { _ = p.Shutdown() })

	// Seed repo+interval+channel+disable via config; the field values should
	// then reflect the config payload.
	payload := `{"repo":"x/y","channel":"dev","interval":"2h","disable":true}`
	if err := p.OnConfig(json.RawMessage(payload)); err != nil {
		t.Fatalf("OnConfig(payload): %v", err)
	}
	p.mu.Lock()
	gotRepo := p.repo
	gotChan := p.channel
	gotIntvl := p.interval
	gotDis := p.disabled
	p.mu.Unlock()
	if gotRepo != "x/y" {
		t.Fatalf("after payload, repo = %q, want x/y", gotRepo)
	}
	if gotChan != "dev" {
		t.Fatalf("after payload, channel = %q, want dev", gotChan)
	}
	if gotIntvl != 2*time.Hour {
		t.Fatalf("after payload, interval = %s, want 2h", gotIntvl)
	}
	if !gotDis {
		t.Fatalf("after payload, disabled = false, want true")
	}

	// Now the user deletes [plugins.autoupdate] and reloads; the host calls
	// OnConfig(nil). Every field the prior OnConfig wrote must revert to
	// its construction-time default.
	if err := p.OnConfig(nil); err != nil {
		t.Fatalf("OnConfig(nil): %v", err)
	}
	p.mu.Lock()
	gotRepo = p.repo
	gotChan = p.channel
	gotIntvl = p.interval
	gotDis = p.disabled
	p.mu.Unlock()
	if gotRepo != defaultRepo {
		t.Fatalf("after OnConfig(nil), repo = %q, want %q", gotRepo, defaultRepo)
	}
	if gotChan != "" {
		t.Fatalf("after OnConfig(nil), channel = %q, want empty", gotChan)
	}
	if gotIntvl != defaultInterval {
		t.Fatalf("after OnConfig(nil), interval = %s, want %s", gotIntvl, defaultInterval)
	}
	if gotDis {
		t.Fatalf("after OnConfig(nil), disabled = true, want false")
	}

	// Zero-length raw (json.RawMessage{}) must take the same reset path.
	if err := p.OnConfig(json.RawMessage(payload)); err != nil {
		t.Fatalf("OnConfig(restore): %v", err)
	}
	if err := p.OnConfig(json.RawMessage{}); err != nil {
		t.Fatalf("OnConfig(empty): %v", err)
	}
	p.mu.Lock()
	gotRepo = p.repo
	gotDis = p.disabled
	p.mu.Unlock()
	if gotRepo != defaultRepo {
		t.Fatalf("after OnConfig(empty), repo = %q, want %q", gotRepo, defaultRepo)
	}
	if gotDis {
		t.Fatalf("after OnConfig(empty), disabled = true, want false")
	}
}

// TestPlugin_OnConfig_Nil_LeavesOptionsAlone asserts the reset path respects
// the precedence ladder: fields set via constructor options must not be
// overwritten by the nil-raw reset.
func TestPlugin_OnConfig_Nil_LeavesOptionsAlone(t *testing.T) {
	t.Setenv(envDisable, "")
	t.Setenv(envRepo, "")
	t.Setenv(envChannel, "")
	t.Setenv(envInterval, "")

	p := New(WithRepo("opt/repo"), WithInterval(3*time.Hour))
	t.Cleanup(func() { _ = p.Shutdown() })

	// Config tries to override both, but options win.
	if err := p.OnConfig(json.RawMessage(`{"repo":"cfg/r","interval":"5h"}`)); err != nil {
		t.Fatalf("OnConfig: %v", err)
	}
	// Now remove config: option-owned fields must stay put.
	if err := p.OnConfig(nil); err != nil {
		t.Fatalf("OnConfig(nil): %v", err)
	}
	p.mu.Lock()
	gotRepo := p.repo
	gotIntvl := p.interval
	p.mu.Unlock()
	if gotRepo != "opt/repo" {
		t.Fatalf("after OnConfig(nil), repo = %q, want opt/repo (option)", gotRepo)
	}
	if gotIntvl != 3*time.Hour {
		t.Fatalf("after OnConfig(nil), interval = %s, want 3h (option)", gotIntvl)
	}
}

// TestPlugin_OnConfig_FieldRemoval_Resets asserts the key-absence reset at
// field granularity: if a [plugins.autoupdate] section is present but a
// previously-set key (e.g. interval) is deleted, OnConfig reverts that field
// to its default rather than retaining the stale value.
func TestPlugin_OnConfig_FieldRemoval_Resets(t *testing.T) {
	t.Setenv(envDisable, "")
	t.Setenv(envRepo, "")
	t.Setenv(envChannel, "")
	t.Setenv(envInterval, "")

	p := New()
	t.Cleanup(func() { _ = p.Shutdown() })

	if err := p.OnConfig(json.RawMessage(`{"repo":"x/y","interval":"2h"}`)); err != nil {
		t.Fatalf("OnConfig(seed): %v", err)
	}
	// User edits the section to remove interval only; repo survives.
	if err := p.OnConfig(json.RawMessage(`{"repo":"x/y"}`)); err != nil {
		t.Fatalf("OnConfig(drop interval): %v", err)
	}
	p.mu.Lock()
	gotRepo := p.repo
	gotIntvl := p.interval
	p.mu.Unlock()
	if gotRepo != "x/y" {
		t.Fatalf("repo clobbered on partial reload: got %q, want x/y", gotRepo)
	}
	if gotIntvl != defaultInterval {
		t.Fatalf("interval not reset on key removal: got %s, want %s", gotIntvl, defaultInterval)
	}
}

func TestShutdownStopsCheckerCleanly(t *testing.T) {
	srv, _ := newReleaseServerJSON(t, `[]`)
	p := newTestPlugin(t, srv, "v0.1.0")
	_ = newTestHost(t, p)

	_ = p.OnEvent(plugin.Initialize{RootPath: t.TempDir()})

	p.mu.Lock()
	c := p.checker
	p.mu.Unlock()
	if c == nil {
		t.Fatalf("checker did not start")
	}

	_ = p.OnEvent(plugin.Shutdown{})
	select {
	case <-c.done:
	case <-time.After(2 * shutdownGrace):
		t.Fatalf("checker did not exit within %s", 2*shutdownGrace)
	}

	if err := p.Shutdown(); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
}
