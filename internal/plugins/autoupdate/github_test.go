package autoupdate

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// threeReleasesJSON has two stable releases plus one prerelease. No drafts.
const threeReleasesJSON = `[
  {"tag_name":"v1.3.0","name":"1.3.0","published_at":"2026-03-10T12:00:00Z","prerelease":false,"draft":false},
  {"tag_name":"v1.3.0-rc1","name":"1.3.0-rc1","published_at":"2026-03-05T12:00:00Z","prerelease":true,"draft":false},
  {"tag_name":"v1.2.0","name":"1.2.0","published_at":"2026-02-01T12:00:00Z","prerelease":false,"draft":false}
]`

// oneDraftJSON has a draft that must be filtered out.
const oneDraftJSON = `[
  {"tag_name":"v2.0.0","published_at":"2026-04-01T00:00:00Z","prerelease":false,"draft":false},
  {"tag_name":"v2.1.0-draft","published_at":"2026-04-15T00:00:00Z","prerelease":false,"draft":true}
]`

// newReleasesServer returns an httptest.Server that serves body with status
// and an optional Retry-After header. It also records the last observed
// request so tests can assert headers.
func newReleasesServer(t *testing.T, status int, body string, retryAfter string) (*httptest.Server, *http.Request) {
	t.Helper()
	var lastReq http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastReq = *r.Clone(r.Context())
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &lastReq
}

// fetchVia hits srv by rewriting the base URL at the http.Client transport
// layer. This avoids exposing a seam in production code for a test-only need.
func fetchVia(ctx context.Context, srv *httptest.Server) ([]Release, error) {
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: rewriteTransport{
			base:  http.DefaultTransport,
			toURL: srv.URL,
		},
	}
	return FetchReleases(ctx, client, "owner", "repo")
}

// rewriteTransport forwards every request to toURL, preserving method and
// headers. Its only job is to let tests point FetchReleases at httptest.
type rewriteTransport struct {
	base  http.RoundTripper
	toURL string
}

func (t rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
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

func TestFetchReleasesOK(t *testing.T) {
	srv, _ := newReleasesServer(t, http.StatusOK, threeReleasesJSON, "")
	got, err := fetchVia(context.Background(), srv)
	if err != nil {
		t.Fatalf("FetchReleases error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3", len(got))
	}
	wantTags := []string{"v1.3.0", "v1.3.0-rc1", "v1.2.0"}
	for i, r := range got {
		if r.TagName != wantTags[i] {
			t.Errorf("got[%d].TagName = %q, want %q", i, r.TagName, wantTags[i])
		}
	}
	if got[0].Prerelease {
		t.Errorf("got[0].Prerelease = true, want false")
	}
	if !got[1].Prerelease {
		t.Errorf("got[1].Prerelease = false, want true")
	}
}

func TestFetchReleasesFiltersDrafts(t *testing.T) {
	srv, _ := newReleasesServer(t, http.StatusOK, oneDraftJSON, "")
	got, err := fetchVia(context.Background(), srv)
	if err != nil {
		t.Fatalf("FetchReleases error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1 (draft should be filtered)", len(got))
	}
	if got[0].TagName != "v2.0.0" {
		t.Errorf("got[0].TagName = %q, want v2.0.0", got[0].TagName)
	}
}

func TestFetchReleasesEmpty(t *testing.T) {
	srv, _ := newReleasesServer(t, http.StatusOK, `[]`, "")
	got, err := fetchVia(context.Background(), srv)
	if err != nil {
		t.Fatalf("FetchReleases error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("len(got) = %d, want 0", len(got))
	}
}

func TestFetchReleasesMalformedJSON(t *testing.T) {
	srv, _ := newReleasesServer(t, http.StatusOK, `not json`, "")
	_, err := fetchVia(context.Background(), srv)
	if err == nil {
		t.Fatalf("FetchReleases err = nil, want decode error")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("err = %v, want message containing 'decode'", err)
	}
}

func TestFetchReleasesRateLimited(t *testing.T) {
	srv, _ := newReleasesServer(t, http.StatusTooManyRequests, `{"message":"rate limited"}`, "42")
	_, err := fetchVia(context.Background(), srv)
	if err == nil {
		t.Fatalf("FetchReleases err = nil, want *RateLimitError")
	}
	var rle *RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("err type = %T, want *RateLimitError (err=%v)", err, err)
	}
	if rle.RetryAfter != 42*time.Second {
		t.Errorf("RetryAfter = %s, want 42s", rle.RetryAfter)
	}
}

func TestFetchReleases500(t *testing.T) {
	srv, _ := newReleasesServer(t, http.StatusInternalServerError, `boom`, "")
	_, err := fetchVia(context.Background(), srv)
	if err == nil {
		t.Fatalf("FetchReleases err = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("err = %v, want message mentioning 500", err)
	}
}

func TestFetchReleasesBodyCap(t *testing.T) {
	// Serve ~2 MiB of junk. JSON decode will fail because the payload is
	// both non-JSON and, after capping at 1 MiB, truncated. The assertion
	// is that we don't OOM and we do surface a decode error.
	big := strings.Repeat("x", 2<<20)
	srv, _ := newReleasesServer(t, http.StatusOK, big, "")
	_, err := fetchVia(context.Background(), srv)
	if err == nil {
		t.Fatalf("FetchReleases err = nil, want decode error from truncated payload")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("err = %v, want message containing 'decode'", err)
	}
}

func TestFetchReleasesUserAgent(t *testing.T) {
	srv, lastReq := newReleasesServer(t, http.StatusOK, `[]`, "")
	if _, err := fetchVia(context.Background(), srv); err != nil {
		t.Fatalf("FetchReleases error: %v", err)
	}
	if got := lastReq.Header.Get("User-Agent"); got != "nistru-autoupdate" {
		t.Errorf("User-Agent = %q, want nistru-autoupdate", got)
	}
	if got := lastReq.Header.Get("Accept"); got != "application/vnd.github+json" {
		t.Errorf("Accept = %q, want application/vnd.github+json", got)
	}
	if got := lastReq.Header.Get("X-GitHub-Api-Version"); got != "2022-11-28" {
		t.Errorf("X-GitHub-Api-Version = %q, want 2022-11-28", got)
	}
}

// releasesFixture builds a small slice for LatestFor tests.
func releasesFixture() []Release {
	return []Release{
		{TagName: "v1.3.0-rc1", Prerelease: true, PublishedAt: mustTime("2026-03-20T00:00:00Z")},
		{TagName: "v1.2.0", Prerelease: false, PublishedAt: mustTime("2026-02-01T00:00:00Z")},
		{TagName: "v1.3.0", Prerelease: false, PublishedAt: mustTime("2026-03-10T00:00:00Z")},
		{TagName: "v1.1.0", Prerelease: false, PublishedAt: mustTime("2026-01-01T00:00:00Z")},
	}
}

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestLatestForRelease(t *testing.T) {
	got, ok := LatestFor("release", releasesFixture())
	if !ok {
		t.Fatalf("ok = false, want true")
	}
	if got.TagName != "v1.3.0" {
		t.Errorf("TagName = %q, want v1.3.0 (latest stable)", got.TagName)
	}
}

func TestLatestForDev(t *testing.T) {
	got, ok := LatestFor("dev", releasesFixture())
	if !ok {
		t.Fatalf("ok = false, want true")
	}
	if got.TagName != "v1.3.0-rc1" {
		t.Errorf("TagName = %q, want v1.3.0-rc1 (latest overall)", got.TagName)
	}
}

func TestLatestForEmpty(t *testing.T) {
	got, ok := LatestFor("release", nil)
	if ok {
		t.Fatalf("ok = true, want false")
	}
	if got.TagName != "" || len(got.Assets) != 0 {
		t.Errorf("got = %+v, want zero Release", got)
	}
	got, ok = LatestFor("dev", []Release{})
	if ok {
		t.Fatalf("ok = true for empty dev, want false")
	}
	if got.TagName != "" || len(got.Assets) != 0 {
		t.Errorf("got = %+v, want zero Release", got)
	}
}

func TestLatestForUnknownChannel(t *testing.T) {
	got, ok := LatestFor("foo", releasesFixture())
	if ok {
		t.Fatalf("ok = true for unknown channel, want false")
	}
	if got.TagName != "" || len(got.Assets) != 0 {
		t.Errorf("got = %+v, want zero Release", got)
	}
}
