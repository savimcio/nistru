package autoupdate

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/savimcio/nistru/plugin"
)

// Fixtures shared across checker-level tests.

// oneStableNewerJSON is a single stable release newer than v0.1.0.
const oneStableNewerJSON = `[
  {"tag_name":"v99.0.0","name":"99.0.0","body":"big release","published_at":"2026-03-10T12:00:00Z","prerelease":false,"draft":false}
]`

// oneStableSameJSON is a single stable release at v0.1.0 (matches the
// "current" we inject in tests).
const oneStableSameJSON = `[
  {"tag_name":"v0.1.0","name":"0.1.0","published_at":"2026-01-01T12:00:00Z","prerelease":false,"draft":false}
]`

// mixedReleasesJSON has one stable (v1.5.0) and one newer prerelease
// (v2.0.0-rc1). Release channel picks v1.5.0; dev channel picks v2.0.0-rc1.
const mixedReleasesJSON = `[
  {"tag_name":"v2.0.0-rc1","name":"2.0.0-rc1","published_at":"2026-03-10T12:00:00Z","prerelease":true,"draft":false},
  {"tag_name":"v1.5.0","name":"1.5.0","published_at":"2026-02-01T12:00:00Z","prerelease":false,"draft":false}
]`

// statusMatcher returns a match func for waitForNotif that accepts a
// statusBar/set with the given segment and a text containing needle. An
// empty needle matches an empty text (the "clear segment" case).
func statusMatcher(segment, needle string) func(plugin.PluginNotifMsg) bool {
	return func(n plugin.PluginNotifMsg) bool {
		if n.Method != "statusBar/set" {
			return false
		}
		var payload struct {
			Segment string `json:"segment"`
			Text    string `json:"text"`
			Color   string `json:"color"`
		}
		if err := json.Unmarshal(n.Params, &payload); err != nil {
			return false
		}
		if payload.Segment != segment {
			return false
		}
		if needle == "" {
			return payload.Text == ""
		}
		return strings.Contains(payload.Text, needle)
	}
}

func TestCheckerDetectsNewerRelease(t *testing.T) {
	srv, _ := newReleaseServerJSON(t, oneStableNewerJSON)
	p := newTestPlugin(t, srv, "v0.1.0")
	h := newTestHost(t, p)

	_ = p.OnEvent(plugin.Initialize{RootPath: t.TempDir()})

	n := waitForNotif(h, 2*time.Second, statusMatcher("autoupdate", "v99.0.0"))
	if n == nil {
		t.Fatalf("did not observe a statusBar/set mentioning v99.0.0")
	}
	// Assert the color is green so a future change to clearing semantics is
	// caught by the test.
	var payload struct {
		Color string `json:"color"`
	}
	if err := json.Unmarshal(n.Params, &payload); err != nil || payload.Color != "green" {
		t.Fatalf("statusBar/set color = %q, want green (err=%v)", payload.Color, err)
	}
}

func TestCheckerNoUpdateClearsSegment(t *testing.T) {
	srv, _ := newReleaseServerJSON(t, oneStableSameJSON)
	p := newTestPlugin(t, srv, "v0.1.0")
	h := newTestHost(t, p)

	_ = p.OnEvent(plugin.Initialize{RootPath: t.TempDir()})

	if n := waitForNotif(h, 2*time.Second, statusMatcher("autoupdate", "")); n == nil {
		t.Fatalf("did not observe a clearing statusBar/set")
	}
}

func TestCheckerRespectsChannel(t *testing.T) {
	srv, _ := newReleaseServerJSON(t, mixedReleasesJSON)
	// Current is older than both releases so the channel — not the compare
	// — decides which version the checker publishes.
	p := newTestPlugin(t, srv, "v0.5.0")
	h := newTestHost(t, p)

	_ = p.OnEvent(plugin.Initialize{RootPath: t.TempDir()})
	if n := waitForNotif(h, 2*time.Second, statusMatcher("autoupdate", "v1.5.0")); n == nil {
		t.Fatalf("release channel: expected v1.5.0 notif, got none")
	}

	// Toggle to dev — the palette command flips state and nudges the checker.
	_ = p.OnEvent(plugin.ExecuteCommand{ID: "autoupdate:switch-channel"})
	if n := waitForNotif(h, 2*time.Second, statusMatcher("autoupdate", "v2.0.0-rc1")); n == nil {
		t.Fatalf("dev channel: expected v2.0.0-rc1 notif, got none")
	}
}

func TestCheckerJitterStaysInEnvelope(t *testing.T) {
	// Pure-helper test — no goroutines, no time.Sleep. Drawing 200
	// intervals from nextInterval should always land within ±10% of the
	// base, which is the contract the checker loop relies on.
	srv, _ := newReleaseServerJSON(t, `[]`)
	p := newTestPlugin(t, srv, "v0.1.0", WithInterval(100*time.Millisecond))
	c := newChecker(p)

	base := p.interval
	low := time.Duration(float64(base) * 0.9)
	high := time.Duration(float64(base) * 1.1)
	for i := range 200 {
		got := c.nextInterval()
		if got < low || got > high {
			t.Fatalf("iter %d: nextInterval = %s, want in [%s, %s]", i, got, low, high)
		}
	}
}

func TestSplitRepo(t *testing.T) {
	cases := []struct {
		in          string
		owner, name string
		valid       bool
	}{
		{"savimcio/nistru", "savimcio", "nistru", true},
		{"", "", "", false},
		{"noslash", "", "", false},
		{"/trailing", "", "", false},
		{"leading/", "", "", false},
	}
	for _, tc := range cases {
		o, n := splitRepo(tc.in)
		got := o != "" && n != ""
		if got != tc.valid {
			t.Fatalf("splitRepo(%q) valid=%v, want %v", tc.in, got, tc.valid)
		}
		if tc.valid && (o != tc.owner || n != tc.name) {
			t.Fatalf("splitRepo(%q) = (%q,%q), want (%q,%q)", tc.in, o, n, tc.owner, tc.name)
		}
	}
}
