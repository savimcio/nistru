package autoupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// githubReleasesURL is the GitHub REST endpoint template for listing releases.
// We intentionally hit /releases (not /releases/latest) so callers can filter
// by channel (release vs dev) over the full set.
const githubReleasesURL = "https://api.github.com/repos/%s/%s/releases"

// maxReleasesBodyBytes caps the response body we will read from GitHub to
// guard against a hostile or misbehaving server returning an unbounded stream.
const maxReleasesBodyBytes = 1 << 20 // 1 MiB

// defaultFetchTimeout is the overall timeout applied to FetchReleases when
// the incoming context does not already carry a deadline.
const defaultFetchTimeout = 10 * time.Second

// Release is a subset of the GitHub Releases API payload. Only fields the
// auto-update flow actually consumes are decoded; everything else is
// discarded by encoding/json.
type Release struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	Prerelease  bool      `json:"prerelease"`
	Draft       bool      `json:"draft"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []Asset   `json:"assets"`
}

// Asset describes a single downloadable artifact attached to a Release.
type Asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
	ContentType        string `json:"content_type"`
}

// RateLimitError is returned by FetchReleases when the GitHub API responds
// with HTTP 429. It surfaces the server-suggested Retry-After window so the
// caller (not this package) can decide whether to back off, skip, or abort.
type RateLimitError struct {
	RetryAfter time.Duration
}

// Error implements the error interface.
func (e *RateLimitError) Error() string {
	return fmt.Sprintf("github: rate limited, retry after %s", e.RetryAfter)
}

// FetchReleases retrieves the full list of releases for owner/repo from the
// GitHub REST API. Draft releases are filtered out; prereleases are kept so
// callers on the "dev" channel can see them.
//
// Behavior:
//   - URL: https://api.github.com/repos/{owner}/{repo}/releases
//   - Headers: User-Agent, Accept, X-GitHub-Api-Version are set.
//   - If ctx has no deadline, a 10s timeout is applied.
//   - If client is nil, a fresh *http.Client with a 10s timeout is used.
//   - Response body is capped at 1 MiB via io.LimitReader.
//   - HTTP 429 -> *RateLimitError with RetryAfter parsed from the header.
//   - Other non-2xx -> error including the status.
//
// The returned slice preserves the API's order (newest first by creation
// date, per GitHub docs); callers must not assume any additional sort.
func FetchReleases(ctx context.Context, client *http.Client, owner, repo string) ([]Release, error) {
	if client == nil {
		client = &http.Client{Timeout: defaultFetchTimeout}
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultFetchTimeout)
		defer cancel()
	}

	url := fmt.Sprintf(githubReleasesURL, owner, repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("github: build request: %w", err)
	}
	req.Header.Set("User-Agent", "nistru-autoupdate")
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github: do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, &RateLimitError{RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("github: unexpected status %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}

	body := io.LimitReader(resp.Body, maxReleasesBodyBytes)
	var raw []Release
	if err := json.NewDecoder(body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("github: decode response: %w", err)
	}

	out := raw[:0]
	for _, r := range raw {
		if r.Draft {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// parseRetryAfter interprets a Retry-After header value. GitHub documents
// delta-seconds; we also accept HTTP-date per RFC 7231 for robustness. If
// the header is missing or unparseable, a zero duration is returned — the
// caller will treat that as "no server guidance".
func parseRetryAfter(h string) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(h); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// LatestFor returns the best matching release for the given channel.
//
//   - "release" -> newest non-prerelease by PublishedAt.
//   - "dev"     -> newest overall by PublishedAt, including prereleases.
//   - anything else -> (Release{}, false).
//
// The boolean is false when no release in the slice satisfies the channel.
func LatestFor(channel string, releases []Release) (Release, bool) {
	switch channel {
	case "release":
		return pickLatest(releases, func(r Release) bool { return !r.Prerelease })
	case "dev":
		return pickLatest(releases, func(Release) bool { return true })
	default:
		return Release{}, false
	}
}

// pickLatest returns the release with the greatest PublishedAt among those
// satisfying keep. Ties are broken by first-seen (stable for equal times).
func pickLatest(releases []Release, keep func(Release) bool) (Release, bool) {
	var (
		best  Release
		found bool
	)
	for _, r := range releases {
		if !keep(r) {
			continue
		}
		if !found || r.PublishedAt.After(best.PublishedAt) {
			best = r
			found = true
		}
	}
	if !found {
		return Release{}, false
	}
	return best, true
}

// Compile-time assertion that *RateLimitError satisfies error.
var _ error = (*RateLimitError)(nil)
