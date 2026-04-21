// Package autoupdate provides the in-process auto-update plugin for nistru.
//
// Version resolution order for Current():
//  1. The ldflags-injected value registered via SetVersion — the nistru
//     main package calls SetVersion(Version) at startup so `go build`
//     binaries stamped via `-X main.Version=...` resolve correctly. The
//     literal "dev" sentinel is treated as "not injected" so an un-stamped
//     binary does not poison the comparison.
//  2. runtime/debug.ReadBuildInfo() — uses info.Main.Version when it is
//     non-empty and not the placeholder "(devel)". This is the value set
//     by `go install github.com/savimcio/nistru/cmd/nistru@<tag>`.
//  3. Fallback: "unknown" — returned for local `go build` (which yields
//     "(devel)") or when build info is unavailable for any reason.
//
// The two-source arrangement fixes the "infinite install loop" regression
// where a locally-built binary reported "unknown" to the checker but
// printed the correct tag to `-version`: every release then compared as
// newer and re-installed on every check.
package autoupdate

import (
	"runtime/debug"
	"sync/atomic"
)

// injected holds the SetVersion-recorded string. Stored in an atomic so
// concurrent readers (the checker goroutine, the palette install command)
// see a consistent value without taking the plugin mutex. The zero value
// (empty string) means "no injection has happened yet".
var injected atomic.Value // stores string

// SetVersion records the version string the running binary reports. It is
// typically called from cmd/nistru/main.go with the ldflags-stamped
// Version symbol before the plugin starts. Passing the literal "dev"
// sentinel clears the injection so Current() falls back to ReadBuildInfo.
// Safe to call concurrently; last writer wins.
func SetVersion(v string) {
	if v == "dev" {
		v = ""
	}
	injected.Store(v)
}

// Current returns a best-effort current version string for the running
// nistru binary. See package doc for resolution order.
func Current() string {
	if v, _ := injected.Load().(string); v != "" {
		return v
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	v := info.Main.Version
	if v == "" || v == "(devel)" {
		return "unknown"
	}
	return v
}
