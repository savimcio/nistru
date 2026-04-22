package autoupdate

import (
	"errors"
	"testing"
)

// mkAssets is a helper to construct []Asset from a list of names. Sizes and
// URLs are synthesised deterministically so callers can assert on them.
func mkAssets(names ...string) []Asset {
	out := make([]Asset, 0, len(names))
	for i, n := range names {
		out = append(out, Asset{
			Name:               n,
			BrowserDownloadURL: "https://example.invalid/" + n,
			Size:               int64(1024 * (i + 1)),
		})
	}
	return out
}

func TestAssetMatchExact(t *testing.T) {
	rel := Release{Assets: mkAssets("nistru_v0.2.0_darwin_arm64.tar.gz")}
	bin, cs, err := AssetMatch(rel, "darwin", "arm64")
	if err != nil {
		t.Fatalf("AssetMatch: unexpected err=%v", err)
	}
	if bin.Name != "nistru_v0.2.0_darwin_arm64.tar.gz" {
		t.Fatalf("binary = %q, want exact match", bin.Name)
	}
	if cs.Name != "" {
		t.Fatalf("checksums = %+v, want zero value", cs)
	}
}

func TestAssetMatchNoVersion(t *testing.T) {
	rel := Release{Assets: mkAssets("nistru_linux_amd64.tar.gz")}
	bin, _, err := AssetMatch(rel, "linux", "amd64")
	if err != nil {
		t.Fatalf("AssetMatch: unexpected err=%v", err)
	}
	if bin.Name != "nistru_linux_amd64.tar.gz" {
		t.Fatalf("binary = %q, want no-version match", bin.Name)
	}
}

func TestAssetMatchHyphenForm(t *testing.T) {
	rel := Release{Assets: mkAssets("nistru-darwin-arm64.tar.gz")}
	bin, _, err := AssetMatch(rel, "darwin", "arm64")
	if err != nil {
		t.Fatalf("AssetMatch: unexpected err=%v", err)
	}
	if bin.Name != "nistru-darwin-arm64.tar.gz" {
		t.Fatalf("binary = %q, want hyphen match", bin.Name)
	}
}

func TestAssetMatchZip(t *testing.T) {
	rel := Release{Assets: mkAssets("nistru_v0.2.0_windows_amd64.zip")}
	bin, _, err := AssetMatch(rel, "windows", "amd64")
	if err != nil {
		t.Fatalf("AssetMatch: unexpected err=%v", err)
	}
	if bin.Name != "nistru_v0.2.0_windows_amd64.zip" {
		t.Fatalf("binary = %q, want zip match", bin.Name)
	}
}

func TestAssetMatchCaseInsensitive(t *testing.T) {
	rel := Release{Assets: mkAssets("nistru_Darwin_ARM64.tar.gz")}
	bin, _, err := AssetMatch(rel, "darwin", "arm64")
	if err != nil {
		t.Fatalf("AssetMatch: unexpected err=%v", err)
	}
	if bin.Name != "nistru_Darwin_ARM64.tar.gz" {
		t.Fatalf("binary = %q, want case-insensitive match", bin.Name)
	}
}

func TestAssetMatchMissesWrongPlatform(t *testing.T) {
	rel := Release{Assets: mkAssets("nistru_linux_amd64.tar.gz")}
	_, _, err := AssetMatch(rel, "darwin", "arm64")
	if !errors.Is(err, ErrNoAssetForPlatform) {
		t.Fatalf("AssetMatch err = %v, want ErrNoAssetForPlatform", err)
	}
}

func TestAssetMatchPicksChecksums(t *testing.T) {
	rel := Release{Assets: mkAssets(
		"nistru_darwin_arm64.tar.gz",
		"checksums.txt",
	)}
	bin, cs, err := AssetMatch(rel, "darwin", "arm64")
	if err != nil {
		t.Fatalf("AssetMatch: unexpected err=%v", err)
	}
	if bin.Name != "nistru_darwin_arm64.tar.gz" {
		t.Fatalf("binary = %q", bin.Name)
	}
	if cs.Name != "checksums.txt" {
		t.Fatalf("checksums = %q, want checksums.txt", cs.Name)
	}
}

func TestAssetMatchMissingChecksums(t *testing.T) {
	rel := Release{Assets: mkAssets("nistru_darwin_arm64.tar.gz")}
	bin, cs, err := AssetMatch(rel, "darwin", "arm64")
	if err != nil {
		t.Fatalf("AssetMatch: unexpected err=%v", err)
	}
	if bin.Name == "" {
		t.Fatalf("binary empty")
	}
	if cs != (Asset{}) {
		t.Fatalf("checksums = %+v, want zero value", cs)
	}
}

func TestAssetMatchChecksumsCaseInsensitive(t *testing.T) {
	rel := Release{Assets: mkAssets(
		"nistru_darwin_arm64.tar.gz",
		"Checksums.TXT",
	)}
	_, cs, err := AssetMatch(rel, "darwin", "arm64")
	if err != nil {
		t.Fatalf("AssetMatch: unexpected err=%v", err)
	}
	if cs.Name != "Checksums.TXT" {
		t.Fatalf("checksums = %q", cs.Name)
	}
}

func TestAssetMatchIgnoresUnrelatedEntries(t *testing.T) {
	rel := Release{Assets: mkAssets(
		"README.md",
		"nistru_v1.0.0_linux_amd64.tar.gz",
		"source.zip",
	)}
	_, _, err := AssetMatch(rel, "darwin", "arm64")
	if !errors.Is(err, ErrNoAssetForPlatform) {
		t.Fatalf("err = %v, want ErrNoAssetForPlatform", err)
	}
}

// TestAssetNameMatchesTable exercises assetNameMatches across positive and
// negative cases, with special focus on SemVer prerelease versions produced
// by goreleaser (e.g. v0.2.0-rc.1). The parser must anchor on the trailing
// (goos, goarch) pair because a hyphenated version segment contributes extra
// tokens.
func TestAssetNameMatchesTable(t *testing.T) {
	cases := []struct {
		name   string
		goos   string
		goarch string
		want   bool
	}{
		// Prerelease positives for the 5 shipped platforms.
		{"nistru_0.2.0-rc.1_linux_amd64.tar.gz", "linux", "amd64", true},
		{"nistru_0.2.0-rc.1_linux_arm64.tar.gz", "linux", "arm64", true},
		{"nistru_0.2.0-rc.1_darwin_amd64.tar.gz", "darwin", "amd64", true},
		{"nistru_0.2.0-rc.1_darwin_arm64.tar.gz", "darwin", "arm64", true},
		{"nistru_0.2.0-rc.1_windows_amd64.zip", "windows", "amd64", true},

		// Other prerelease shapes.
		{"nistru_0.2.0-beta.2_linux_amd64.tar.gz", "linux", "amd64", true},
		{"nistru_0.2.0-alpha_darwin_arm64.tar.gz", "darwin", "arm64", true},
		// SemVer build metadata: '+' is not a separator, so the whole
		// "rc.1+build.42" survives as a single token, leaving the trailing
		// (goos, goarch) pair intact.
		{"nistru_0.2.0-rc.1+build.42_linux_amd64.tar.gz", "linux", "amd64", true},

		// Legacy positives that must keep working.
		{"nistru_0.2.0_linux_amd64.tar.gz", "linux", "amd64", true},
		{"nistru_linux_amd64.tar.gz", "linux", "amd64", true},
		{"nistru-linux-amd64.tar.gz", "linux", "amd64", true},

		// Negatives: wrong os/arch, wrong prefix, unknown extension.
		{"nistru_0.2.0-rc.1_linux_amd64.tar.gz", "darwin", "amd64", false},
		{"nistru_0.2.0-rc.1_darwin_amd64.tar.gz", "darwin", "arm64", false},
		{"hello_0.2.0_linux_amd64.tar.gz", "linux", "amd64", false},
		{"nistru_0.2.0-rc.1_linux_amd64.unknown", "linux", "amd64", false},
	}
	for _, tc := range cases {
		got := assetNameMatches(tc.name, tc.goos, tc.goarch)
		if got != tc.want {
			t.Errorf("assetNameMatches(%q, %q, %q) = %v, want %v",
				tc.name, tc.goos, tc.goarch, got, tc.want)
		}
	}
}

// TestAssetMatch_PrereleaseV020RC1 mirrors the real v0.2.0-rc.1 release
// artifact set (https://github.com/savimcio/nistru/releases/tag/v0.2.0-rc.1)
// to guard against the hyphenated-version regression that was caught during
// the initial release-pipeline smoke test.
func TestAssetMatch_PrereleaseV020RC1(t *testing.T) {
	rel := Release{Assets: mkAssets(
		"nistru_0.2.0-rc.1_linux_amd64.tar.gz",
		"nistru_0.2.0-rc.1_linux_arm64.tar.gz",
		"nistru_0.2.0-rc.1_darwin_amd64.tar.gz",
		"nistru_0.2.0-rc.1_darwin_arm64.tar.gz",
		"nistru_0.2.0-rc.1_windows_amd64.zip",
		"checksums.txt",
	)}

	platforms := []struct {
		goos    string
		goarch  string
		wantBin string
	}{
		{"linux", "amd64", "nistru_0.2.0-rc.1_linux_amd64.tar.gz"},
		{"linux", "arm64", "nistru_0.2.0-rc.1_linux_arm64.tar.gz"},
		{"darwin", "amd64", "nistru_0.2.0-rc.1_darwin_amd64.tar.gz"},
		{"darwin", "arm64", "nistru_0.2.0-rc.1_darwin_arm64.tar.gz"},
		{"windows", "amd64", "nistru_0.2.0-rc.1_windows_amd64.zip"},
	}
	for _, p := range platforms {
		bin, cs, err := AssetMatch(rel, p.goos, p.goarch)
		if err != nil {
			t.Fatalf("AssetMatch(%s/%s): unexpected err=%v", p.goos, p.goarch, err)
		}
		if bin.Name != p.wantBin {
			t.Errorf("AssetMatch(%s/%s) binary = %q, want %q",
				p.goos, p.goarch, bin.Name, p.wantBin)
		}
		if cs.Name != "checksums.txt" {
			t.Errorf("AssetMatch(%s/%s) checksums = %q, want checksums.txt",
				p.goos, p.goarch, cs.Name)
		}
	}
}
