package editor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/savimcio/nistru/internal/plugins/autoupdate"
	"github.com/savimcio/nistru/internal/plugins/treepane"
	"github.com/savimcio/nistru/plugin"
)

// rewriteAPITransport forwards every request targeting api.github.com to the
// test server. Non-matching hosts fall through to the base transport, but in
// practice the autoupdate plugin only calls api.github.com so the fallthrough
// is defensive. Mirrors the pattern in internal/plugins/autoupdate/github_test.go
// but scoped to the api host so an accidentally-real request still fails fast.
type rewriteAPITransport struct {
	base  http.RoundTripper
	toURL string
}

func (t rewriteAPITransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	u, err := clone.URL.Parse(t.toURL)
	if err != nil {
		return nil, err
	}
	clone.URL.Scheme = u.Scheme
	clone.URL.Host = u.Host
	clone.Host = u.Host
	return t.base.RoundTrip(clone)
}

// TestAutoupdatePluginEndToEnd exercises the full pipe from the autoupdate
// plugin's background checker through Host.PostNotif, Host.inbound,
// Host.Recv(), Model.Update, handlePluginNotif, and finally into
// m.statusSegments. Component-level coverage for each hop already exists
// (autoupdate tests for the checker, plugin/host_test.go for PostNotif, and
// model_component_test.go for statusBar/set); this test closes the gap
// between the plugin and the editor model with a single end-to-end run.
func TestAutoupdatePluginEndToEnd(t *testing.T) {
	// Clear any env-var overrides that New() honours. The test construction
	// options win regardless (they apply after env), but clearing keeps the
	// test hermetic under CI where the host may have these set.
	t.Setenv("NISTRU_AUTOUPDATE_REPO", "")
	t.Setenv("NISTRU_AUTOUPDATE_CHANNEL", "")
	t.Setenv("NISTRU_AUTOUPDATE_INTERVAL", "")
	t.Setenv("NISTRU_AUTOUPDATE_DISABLE", "")

	// Workspace root with a couple of files so treepane has something real
	// to index. The contents do not matter; we never open them.
	root := t.TempDir()
	writeFile(t, root, "a.txt", "hello\n")
	writeFile(t, root, "b.go", "package main\n")

	// httptest server serving one canned release. The autoupdate checker
	// issues GET /repos/owner/nistru/releases and decodes a JSON array.
	const relJSON = `[
      {"tag_name":"v99.0.0","name":"99.0.0","body":"notes","published_at":"2099-01-01T00:00:00Z","prerelease":false,"draft":false}
    ]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(relJSON))
	}))
	t.Cleanup(srv.Close)

	// Goroutine baseline AFTER httptest.Server spins up but BEFORE the host
	// and checker start. Taken here so the final settle-check is comparing
	// like for like: both counts include the httptest listener goroutine.
	baseGoroutines := runtime.NumGoroutine()

	// Build a fresh registry that mirrors NewModel's but substitutes a
	// test-configured autoupdate plugin.
	registry := plugin.NewRegistry()
	tp, err := treepane.New(root)
	if err != nil {
		t.Fatalf("treepane.New: %v", err)
	}
	registry.RegisterInProc(tp)

	statePath := filepath.Join(t.TempDir(), "state.json")
	// Dedicated transport per test so we can call CloseIdleConnections() on
	// shutdown without side-effecting http.DefaultTransport. Disabling
	// keep-alives guarantees the RoundTrip goroutines drain immediately
	// rather than hanging around in the connection pool.
	innerTransport := &http.Transport{
		DisableKeepAlives: true,
	}
	t.Cleanup(innerTransport.CloseIdleConnections)
	au := autoupdate.New(
		autoupdate.WithRepo("owner/nistru"),
		autoupdate.WithHTTPClient(&http.Client{
			Timeout:   5 * time.Second,
			Transport: rewriteAPITransport{base: innerTransport, toURL: srv.URL},
		}),
		autoupdate.WithInterval(10*time.Millisecond),
		autoupdate.WithStatePath(statePath),
		autoupdate.WithInstaller(noopInstallerForTest{}),
		// Pin the detected current version so the comparison is
		// deterministic regardless of how `go test` was invoked. An "unknown"
		// current would also work (it sorts behind any real version), but
		// pinning is unambiguous.
		autoupdate.WithCurrent("v0.0.1"),
	)
	registry.RegisterInProc(au)

	m, err := newModelWithRegistry(root, registry)
	if err != nil {
		t.Fatalf("newModelWithRegistry: %v", err)
	}

	// Prime the host.Recv pump. Init returns tea.Batch(editor.Init,
	// host.Recv); we don't care about the editor init cmd here, but we DO
	// need to keep calling host.Recv() to drain PluginNotifMsg frames.
	//
	// newModelWithRegistry now emits Initialize internally so in-proc plugins
	// that activate on onStart (autoupdate, treepane) have their OnEvent
	// handlers invoked before we return. The checker goroutine is already
	// running by this point; we just need to drain its status-bar frames.
	_ = m.Init()

	// Pump the model's inbound chain. A single helper goroutine pulls
	// frames from host.Recv() and feeds them to the main goroutine over
	// msgCh. The main goroutine runs Update and checks for the target
	// segment. Using a single pump avoids the "one stuck Recv goroutine
	// per timeout iteration" leak pattern that a per-iteration helper
	// goroutine would accumulate (host.inbound is never closed, so a
	// blocked Recv cannot be cancelled from the outside).
	//
	// We capture host into a local so the pump goroutine does not race
	// with the main goroutine's reassignment of m. The host pointer
	// itself is immutable post-construction.
	host := m.host
	msgCh := make(chan tea.Msg, 64)
	pumpStop := make(chan struct{})
	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		for {
			recv := host.Recv()
			if recv == nil {
				return
			}
			msg := recv()
			if msg == nil {
				return
			}
			select {
			case msgCh <- msg:
			case <-pumpStop:
				return
			}
		}
	}()

	deadline := time.Now().Add(2 * time.Second)
	seenMethods := []string{}
	found := false
loop:
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		select {
		case msg := <-msgCh:
			if nm, isNotif := msg.(plugin.PluginNotifMsg); isNotif {
				seenMethods = append(seenMethods, nm.Plugin+":"+nm.Method)
			}
			newM, _ := m.Update(msg)
			m = newM.(*Model)

			for _, seg := range m.statusSegments {
				if seg.Plugin == "autoupdate" && seg.Name == "autoupdate" &&
					strings.Contains(seg.Text, "v99.0.0") && seg.Color == "green" {
					found = true
					break
				}
			}
			if found {
				break loop
			}
		case <-time.After(remaining):
			break loop
		}
	}

	if !found {
		t.Fatalf("did not observe autoupdate statusBar segment within 2s.\n"+
			"  statusSegments=%+v\n  seen notifications=%v",
			m.statusSegments, seenMethods)
	}

	// Confirm exactly one autoupdate segment with the expected shape.
	var auSegs []statusSegment
	for _, s := range m.statusSegments {
		if s.Plugin == "autoupdate" {
			auSegs = append(auSegs, s)
		}
	}
	if len(auSegs) != 1 {
		t.Fatalf("want exactly 1 autoupdate segment, got %d: %+v", len(auSegs), auSegs)
	}
	got := auSegs[0]
	if got.Name != "autoupdate" {
		t.Errorf("segment Name: got %q, want %q", got.Name, "autoupdate")
	}
	if got.Color != "green" {
		t.Errorf("segment Color: got %q, want %q", got.Color, "green")
	}
	if !strings.Contains(got.Text, "v99.0.0") {
		t.Errorf("segment Text: got %q, want substring v99.0.0", got.Text)
	}

	// Clean shutdown. Any goroutine leak surfaces below.
	if err := m.host.Shutdown(500 * time.Millisecond); err != nil {
		t.Errorf("host.Shutdown: %v", err)
	}

	// Drain the pump goroutine. After Shutdown no more PostNotif fires,
	// and host.inbound is never closed — so the pump may be blocked on
	// recv() OR on msgCh<-. Signal stop first (to unblock the send side),
	// then repeatedly PostNotif a sentinel (one per pending inbound frame)
	// so the pump's recv() unblocks and finds pumpStop closed on the next
	// select. We re-post defensively because the inbound buffer may have
	// been saturated by the checker's final burst, dropping earlier
	// sentinels silently per PostNotif's non-blocking contract.
	close(pumpStop)
	drainDeadline := time.Now().Add(1 * time.Second)
	for {
		select {
		case <-msgCh:
			// discard
		case <-pumpDone:
			goto drained
		case <-time.After(20 * time.Millisecond):
			// Either the pump is blocked on recv() with no pending
			// frames (sentinel was dropped), or the pump is blocked on
			// msgCh<- but msgCh is empty right now. Re-post a sentinel
			// and loop. The close(pumpStop) above guarantees the pump
			// returns on its next select pass, so this is bounded.
			if time.Now().After(drainDeadline) {
				t.Errorf("pump goroutine did not exit within 1s after sentinel")
				goto drained
			}
			_ = host.PostNotif("test-sentinel", "noop", nil)
		}
	}
drained:

	// Close idle connections on our dedicated transport before taking the
	// final goroutine snapshot — http.Transport keeps connection-pool
	// reapers alive for ~90s by default otherwise.
	innerTransport.CloseIdleConnections()

	// Give the runtime a brief window for stopped goroutines to be reaped.
	// Tolerance of 2 accounts for stdlib noise (scheduler goroutines that
	// transiently appear and disappear during the settle window).
	if !waitGoroutinesSettled(baseGoroutines+2, 500*time.Millisecond) {
		t.Errorf("goroutine leak: before=%d, after=%d",
			baseGoroutines, runtime.NumGoroutine())
	}
}

// waitGoroutinesSettled polls runtime.NumGoroutine() until it is <= upper
// or the deadline expires. Returns true on settle, false on timeout.
func waitGoroutinesSettled(upper int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= upper {
			return true
		}
		// Short yield; we must avoid bare time.Sleep per house rules, but
		// a tight polling loop with a deadline is acceptable and mirrors
		// the pattern already used by waitUntil in other component tests.
		time.Sleep(10 * time.Millisecond)
	}
	return runtime.NumGoroutine() <= upper
}

// noopInstallerForTest mirrors autoupdate's noopInstaller but is declared
// locally so the test does not depend on that type being exported. The
// autoupdate plugin only needs Install/Rollback to satisfy the Installer
// interface; neither is invoked under this test because we never dispatch
// an autoupdate:install command.
type noopInstallerForTest struct{}

func (noopInstallerForTest) Install(_ context.Context, _ *plugin.Host, _ autoupdate.Release, _ string) error {
	return nil
}

func (noopInstallerForTest) Rollback(_ context.Context, _ *plugin.Host) error {
	return nil
}

// Compile-time assertion so a drift in the Installer interface surfaces at
// build time rather than as a mysterious runtime behaviour.
var _ autoupdate.Installer = noopInstallerForTest{}
