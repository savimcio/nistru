package autoupdate

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/savimcio/nistru/plugin"
)

// checker is the long-running background loop that polls GitHub for new
// releases. It lives inside Plugin and owns a single goroutine started from
// start(); stop() cancels that goroutine and waits for it to exit.
//
// Ticker cadence: one fire immediately on start so the status bar populates
// as soon as the editor launches, then every interval*(0.9..1.1) with jitter.
// The triggerNow channel short-circuits the wait for the "check now" palette
// command and for channel switches.
type checker struct {
	interval time.Duration
	now      func() time.Time
	rng      *rand.Rand
	rngMu    sync.Mutex

	client  *http.Client
	repo    string
	current string

	host *plugin.Host

	// getState reads the shared Plugin state. updateState applies a
	// read-modify-write under the plugin mutex (reloading from disk first
	// so concurrent writers composed on disjoint fields do not clobber).
	// We do not hold a long-lived pointer into Plugin.state because the
	// plugin serialises all state access under its own mutex.
	getState    func() State
	updateState func(func(*State)) error

	triggerNow chan struct{}
	stop       chan struct{}
	done       chan struct{}

	lastRelease atomic.Pointer[Release]
}

// newChecker wires up a checker with sane defaults. The caller must still
// invoke start(ctx) to kick off the goroutine.
func newChecker(p *Plugin) *checker {
	return &checker{
		interval:    p.interval,
		now:         p.now,
		rng:         rand.New(rand.NewSource(p.now().UnixNano())),
		client:      p.client,
		repo:        p.repo,
		current:     p.current,
		host:        p.host,
		getState:    p.snapshotState,
		updateState: p.updateState,
		triggerNow:  make(chan struct{}, 1),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
	}
}

// start launches the ticker goroutine. It is idempotent only relative to the
// owning Plugin, which never calls start() twice for the same checker.
func (c *checker) start(ctx context.Context) {
	go c.loop(ctx)
}

// loop is the body of the checker goroutine. It performs one immediate tick,
// then waits on a mix of timer, manual trigger, stop, and ctx.Done.
func (c *checker) loop(ctx context.Context) {
	defer close(c.done)

	c.tick(ctx)

	for {
		wait := c.nextInterval()
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-c.stop:
			timer.Stop()
			return
		case <-c.triggerNow:
			timer.Stop()
			c.tick(ctx)
		case <-timer.C:
			c.tick(ctx)
		}
	}
}

// nextInterval returns the base interval scaled by a jitter factor in
// [0.9, 1.1). The factor is drawn from a package-local *rand.Rand so the
// global math/rand stream is untouched.
func (c *checker) nextInterval() time.Duration {
	if c.interval <= 0 {
		return 0
	}
	c.rngMu.Lock()
	factor := 0.9 + c.rng.Float64()*0.2
	c.rngMu.Unlock()
	return time.Duration(float64(c.interval) * factor)
}

// nudge attempts a non-blocking send on triggerNow. If the channel already
// has a pending trigger, we silently drop the second one — the goroutine
// will still pick up the pending signal on the next iteration, which
// coalesces bursts naturally.
func (c *checker) nudge() {
	select {
	case c.triggerNow <- struct{}{}:
	default:
	}
}

// tick performs one end-to-end fetch + compare + publish cycle. Any error
// is logged to stderr and the tick returns quietly so the next interval
// still fires. State is persisted best-effort on every tick.
func (c *checker) tick(ctx context.Context) {
	owner, repo := splitRepo(c.repo)
	if owner == "" || repo == "" {
		fmt.Fprintf(os.Stderr, "autoupdate: invalid repo %q; skipping tick\n", c.repo)
		return
	}

	releases, err := FetchReleases(ctx, c.client, owner, repo)
	if err != nil {
		if rl, ok := errors.AsType[*RateLimitError](err); ok {
			fmt.Fprintf(os.Stderr, "autoupdate: rate limited, retry after %s\n", rl.RetryAfter)
			return
		}
		fmt.Fprintf(os.Stderr, "autoupdate: fetch releases: %v\n", err)
		return
	}

	st := c.getState()
	channel := st.Channel
	if channel == "" {
		channel = DefaultChannel()
	}

	latest, ok := LatestFor(channel, releases)
	if !ok {
		// Nothing to publish; still record that we checked. Goes through
		// updateState so a concurrent install/rollback cannot get its
		// PendingRestartVersion / PrevBinaryPath fields clobbered.
		now := c.now()
		if err := c.updateState(func(s *State) { s.LastChecked = now }); err != nil {
			fmt.Fprintf(os.Stderr, "autoupdate: save state: %v\n", err)
		}
		return
	}
	// Remember the latest known release so palette commands can reach it.
	relCopy := latest
	c.lastRelease.Store(&relCopy)

	tag := NormalizeVersion(latest.TagName)
	cmp := CompareVersions(c.current, latest.TagName)
	if cmp < 0 {
		// Newer release available — show it in the status bar.
		_ = c.host.PostNotif("autoupdate", "statusBar/set", map[string]string{
			"segment": "autoupdate",
			"text":    "↑ " + tag + " available",
			"color":   "green",
		})
	} else {
		// Up to date — clear any previously shown segment.
		_ = c.host.PostNotif("autoupdate", "statusBar/set", map[string]string{
			"segment": "autoupdate",
			"text":    "",
		})
	}

	now := c.now()
	latestTag := latest.TagName
	if err := c.updateState(func(s *State) {
		s.LastChecked = now
		s.LastSeenVersion = latestTag
	}); err != nil {
		fmt.Fprintf(os.Stderr, "autoupdate: save state: %v\n", err)
	}
}

// cancel closes stop and waits up to the given timeout for the goroutine to
// exit. Callers that don't care about the wait (e.g. GC cleanup) can pass 0.
func (c *checker) cancel(wait time.Duration) {
	select {
	case <-c.stop:
		// Already closed — cancel is idempotent.
	default:
		close(c.stop)
	}
	if wait <= 0 {
		return
	}
	select {
	case <-c.done:
	case <-time.After(wait):
	}
}

// splitRepo parses "owner/repo" into its two components. Empty/invalid input
// returns two empty strings and the caller skips this tick.
func splitRepo(s string) (string, string) {
	i := strings.IndexByte(s, '/')
	if i <= 0 || i == len(s)-1 {
		return "", ""
	}
	return s[:i], s[i+1:]
}
