package autoupdate

import (
	"context"
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
