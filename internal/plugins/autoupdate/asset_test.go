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
