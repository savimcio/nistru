package autoupdate

import "testing"

// TestCurrentNonEmpty asserts Current() returns a non-empty string. Under
// the go test binary, ReadBuildInfo() is populated, so the function must
// always return either a real version or the "unknown" fallback.
func TestCurrentNonEmpty(t *testing.T) {
	t.Cleanup(func() { SetVersion("") })
	SetVersion("")
	if got := Current(); got == "" {
		t.Fatalf("Current() = %q, want non-empty", got)
	}
}

// TestCurrentDoesNotPanic guards against future regressions in the
// resolution logic (e.g. a nil deref on info.Main).
func TestCurrentDoesNotPanic(t *testing.T) {
	t.Cleanup(func() { SetVersion("") })
	SetVersion("")
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Current() panicked: %v", r)
		}
	}()
	_ = Current()
}

// TestCurrentPrefersInjectedVersion exercises the SetVersion/Current
// contract: a non-empty, non-"dev" injection overrides ReadBuildInfo, while
// "" and the "dev" sentinel both fall back to the build-info resolution.
// This is the Finding-5 regression — without it, a locally-built nistru
// binary prints a real tag via `-version` but the checker sees "unknown"
// and loops re-installing every release on every poll.
func TestCurrentPrefersInjectedVersion(t *testing.T) {
	t.Cleanup(func() { SetVersion("") })

	SetVersion("v0.2.0")
	if got := Current(); got != "v0.2.0" {
		t.Fatalf("Current() with injection = %q, want v0.2.0", got)
	}

	// "dev" is the un-stamped ldflags default — treat as absent so a
	// release-channel comparison does not trust it.
	SetVersion("dev")
	if got := Current(); got == "v0.2.0" {
		t.Fatalf("Current() after SetVersion(dev) = %q, want fallback (not stale injection)", got)
	}

	// Empty is explicit clear.
	SetVersion("")
	if got := Current(); got == "v0.2.0" {
		t.Fatalf("Current() after SetVersion(empty) = %q, want fallback", got)
	}
}
