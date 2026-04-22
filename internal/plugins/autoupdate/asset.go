package autoupdate

import (
	"errors"
	"strings"
)

// ErrNoAssetForPlatform is returned by AssetMatch when none of the known
// release-asset naming conventions matches the running platform. Callers
// (typically Preflight) surface this to the user as a signal to fall back
// to `go install`.
var ErrNoAssetForPlatform = errors.New("autoupdate: no release assets for this platform")

// checksumsAssetName is the exact (case-insensitive) filename goreleaser
// emits for a checksum manifest.
const checksumsAssetName = "checksums.txt"

// AssetMatch picks the best binary asset in rel for the supplied goos /
// goarch and also returns the accompanying checksums manifest if present.
//
// Supported name patterns (tried in order, case-insensitive):
//
//	nistru_<version>_<goos>_<goarch>.tar.gz    (version may be SemVer with prerelease, e.g. "0.2.0-rc.1")
//	nistru_<goos>_<goarch>.tar.gz
//	nistru-<goos>-<goarch>.tar.gz
//	nistru_<version>_<goos>_<goarch>.zip
//	nistru_<goos>_<goarch>.zip
//	nistru-<goos>-<goarch>.zip
//
// The version token is not compared — we only care that the os+arch pair is
// present. Returns the matched binary asset, the checksums asset (zero value
// if none), or an error (ErrNoAssetForPlatform if nothing matches).
func AssetMatch(rel Release, goos, goarch string) (Asset, Asset, error) {
	var (
		binary    Asset
		checksums Asset
		found     bool
	)

	goos = strings.ToLower(goos)
	goarch = strings.ToLower(goarch)

	for _, a := range rel.Assets {
		lower := strings.ToLower(a.Name)
		if lower == checksumsAssetName {
			checksums = a
			continue
		}
		if found {
			continue
		}
		if assetNameMatches(lower, goos, goarch) {
			binary = a
			found = true
		}
	}

	if !found {
		return Asset{}, Asset{}, ErrNoAssetForPlatform
	}
	return binary, checksums, nil
}

// assetNameMatches returns true if name (already lowercased) matches any of
// the supported goreleaser naming conventions for the given goos/goarch.
//
// The allowed extensions are ".tar.gz" (and its ".tgz" alias) for POSIX and
// ".zip" for Windows. Separators between project/os/arch may be underscores
// or hyphens; a middle version segment is tolerated but not required, and
// may itself span multiple tokens because SemVer prerelease identifiers
// contribute hyphens ("0.2.0-rc.1", "0.2.0-beta.2", etc.). We therefore
// anchor on the trailing two tokens as (goos, goarch) rather than counting
// the whole sequence.
func assetNameMatches(name, goos, goarch string) bool {
	ext := assetExt(name)
	if ext == "" {
		return false
	}
	stem := strings.TrimSuffix(name, ext)

	const project = "nistru"
	if !strings.HasPrefix(stem, project+"_") && !strings.HasPrefix(stem, project+"-") {
		return false
	}
	rest := stem[len(project)+1:]

	// The version segment is optional and may itself contain hyphens when
	// a SemVer prerelease identifier is present ("0.2.0-rc.1", "0.2.0-beta.2",
	// etc.), producing extra tokens between the project prefix and the
	// trailing (goos, goarch) pair. Anchor on the last two tokens rather
	// than counting the whole sequence.
	tokens := splitOnSepChars(rest, "_-")
	if len(tokens) < 2 {
		return false
	}
	return tokens[len(tokens)-2] == goos && tokens[len(tokens)-1] == goarch
}

// assetExt returns the archive extension (".tar.gz", ".tgz", ".zip") of
// name, already lowercased, or "" if none is recognised.
func assetExt(name string) string {
	switch {
	case strings.HasSuffix(name, ".tar.gz"):
		return ".tar.gz"
	case strings.HasSuffix(name, ".tgz"):
		return ".tgz"
	case strings.HasSuffix(name, ".zip"):
		return ".zip"
	default:
		return ""
	}
}

// splitOnSepChars splits s on any byte in seps. Empty tokens are discarded.
func splitOnSepChars(s, seps string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(seps, s[i]) >= 0 {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
