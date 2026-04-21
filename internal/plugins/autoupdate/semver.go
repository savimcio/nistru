package autoupdate

import (
	"strings"

	"golang.org/x/mod/semver"
)

// isUnknownVersion reports whether v should be treated as "behind everything
// else" — the empty string, the literal "unknown", or the go toolchain's
// "(devel)" placeholder for unstamped local builds.
func isUnknownVersion(v string) bool {
	switch v {
	case "", "unknown", "(devel)":
		return true
	default:
		return false
	}
}

// ensureVPrefix prepends "v" when missing so inputs survive semver.Compare,
// which requires the leading "v".
func ensureVPrefix(v string) string {
	if strings.HasPrefix(v, "v") {
		return v
	}
	return "v" + v
}

// CompareVersions returns -1, 0, or +1 depending on whether a is less than,
// equal to, or greater than b under semver ordering.
//
// Inputs are first checked for the "unknown" sentinels ("", "unknown",
// "(devel)"); an unknown version sorts strictly before any known version,
// and two unknowns compare equal.
//
// Inputs missing a leading "v" are tolerated. Any post-normalization input
// that is not valid semver per semver.IsValid is also treated as unknown.
func CompareVersions(a, b string) int {
	aUnknown := isUnknownVersion(a)
	bUnknown := isUnknownVersion(b)

	na := ensureVPrefix(a)
	nb := ensureVPrefix(b)

	if !aUnknown && !semver.IsValid(na) {
		aUnknown = true
	}
	if !bUnknown && !semver.IsValid(nb) {
		bUnknown = true
	}

	switch {
	case aUnknown && bUnknown:
		return 0
	case aUnknown:
		return -1
	case bUnknown:
		return 1
	}
	return semver.Compare(na, nb)
}

// NormalizeVersion returns the v-prefixed canonical form of v for display
// (e.g. "0.1" -> "v0.1.0"). Inputs that are not valid semver after adding
// the "v" prefix are returned unchanged so callers can still render them.
func NormalizeVersion(v string) string {
	n := ensureVPrefix(v)
	if !semver.IsValid(n) {
		return v
	}
	return semver.Canonical(n)
}
