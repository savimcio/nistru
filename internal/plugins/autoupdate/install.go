package autoupdate

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/savimcio/nistru/plugin"
)

// Defaults for RealInstaller.
const (
	defaultMaxAssetBytes      = int64(100 << 20) // 100 MiB
	defaultInstallHTTPTimeout = 60 * time.Second
	maxChecksumsBodyBytes     = int64(64 << 10) // 64 KiB cap on checksums.txt

	// defaultMaxArchiveBytes caps total decompressed bytes read across all
	// tar entries — defence in depth against a "tar bomb" that compresses
	// gigabytes of zeros into kilobytes. Independent of WithMaxSize, which
	// bounds the single extracted binary.
	defaultMaxArchiveBytes = int64(500 << 20) // 500 MiB

	// defaultMaxArchiveEntries caps tar header count so a pathological
	// archive with millions of zero-byte entries cannot wedge the reader.
	defaultMaxArchiveEntries = 10_000
)

// errPreflightWindows is the internal sentinel signalling the caller to
// emit the notify-only upgrade instructions and return nil from Install.
var errPreflightWindows = errors.New("autoupdate: windows in-place swap unsupported")

// RealInstaller is the production Installer: it downloads release archives,
// verifies their SHA-256 against the release's checksums.txt, atomically
// swaps the running binary on POSIX, and leaves a ".prev" sibling for
// Rollback. On Windows it short-circuits to a notify-only fallback.
//
// RealInstaller is safe for concurrent use by the plugin's single palette
// dispatch; tests can drive Install/Rollback/Cleanup directly. All state
// persistence flows through the updater supplied via WithStateUpdater —
// this is what serialises Install/Rollback's field updates against the
// checker goroutine's own LastChecked/LastSeenVersion writes. When no
// updater is wired the installer falls back to a local read-modify-write
// against statePath, which is race-prone and only appropriate for
// stand-alone tests of the installer.
type RealInstaller struct {
	executable   func() (string, error)
	goos         string
	goarch       string
	client       *http.Client
	maxSize      int64
	maxArchive   int64
	maxEntries   int
	statePath    string
	freeBytes    func(dir string) (int64, error)
	updateStateF func(func(*State)) error
}

// InstallerOption configures a RealInstaller.
type InstallerOption func(*RealInstaller)

// WithExecutableFunc injects the function used to resolve the path of the
// running binary. Tests always pass a closure returning a path under
// t.TempDir() so no test ever touches the real os.Executable result.
func WithExecutableFunc(fn func() (string, error)) InstallerOption {
	return func(r *RealInstaller) { r.executable = fn }
}

// WithGoos overrides runtime.GOOS for testing the windows notify path and
// platform-asset mismatch flows.
func WithGoos(s string) InstallerOption {
	return func(r *RealInstaller) { r.goos = s }
}

// WithGoarch overrides runtime.GOARCH for asset-selection tests.
func WithGoarch(s string) InstallerOption {
	return func(r *RealInstaller) { r.goarch = s }
}

// WithInstallerHTTPClient injects an HTTP client. Tests point it at an
// httptest.Server and rely on absolute BrowserDownloadURLs served there.
func WithInstallerHTTPClient(c *http.Client) InstallerOption {
	return func(r *RealInstaller) { r.client = c }
}

// WithMaxSize caps the bytes we will read from the asset server. Exceeding
// maxSize mid-download aborts with an error and deletes the partial tmpfile.
func WithMaxSize(n int64) InstallerOption {
	return func(r *RealInstaller) {
		if n > 0 {
			r.maxSize = n
		}
	}
}

// WithInstallerStatePath overrides the state-file location. Tests pass
// temp-dir paths so nothing leaks into the user's config dir.
func WithInstallerStatePath(p string) InstallerOption {
	return func(r *RealInstaller) { r.statePath = p }
}

// withFreeBytesFunc is a test-only seam for injecting a fake statfs without
// introducing a package-level mutable. Not exported.
func withFreeBytesFunc(fn func(dir string) (int64, error)) InstallerOption {
	return func(r *RealInstaller) { r.freeBytes = fn }
}

// WithStateUpdater injects the plugin's serialised read-modify-write helper.
// Install/Rollback use it to mutate only PendingRestartVersion and
// PrevBinaryPath without clobbering fields the checker owns (LastChecked,
// LastSeenVersion) or the user owns (Channel). When nil, Install/Rollback
// fall back to a local LoadState/SaveState round-trip that is *not*
// serialised against concurrent checker ticks.
func WithStateUpdater(fn func(func(*State)) error) InstallerOption {
	return func(r *RealInstaller) { r.updateStateF = fn }
}

// WithMaxArchiveBytes caps the total decompressed bytes extract() will
// tolerate across every tar entry. Prevents a highly compressible archive
// from forcing gigabytes of decompression work.
func WithMaxArchiveBytes(n int64) InstallerOption {
	return func(r *RealInstaller) {
		if n > 0 {
			r.maxArchive = n
		}
	}
}

// WithMaxArchiveEntries caps the number of tar headers extract() will
// accept. Complementary to WithMaxArchiveBytes for archives with many
// empty entries.
func WithMaxArchiveEntries(n int) InstallerOption {
	return func(r *RealInstaller) {
		if n > 0 {
			r.maxEntries = n
		}
	}
}

// NewInstaller returns a RealInstaller with sensible defaults. Callers may
// override any seam via Options. A nil return is never produced.
func NewInstaller(opts ...InstallerOption) *RealInstaller {
	r := &RealInstaller{
		executable: os.Executable,
		goos:       runtime.GOOS,
		goarch:     runtime.GOARCH,
		client:     &http.Client{Timeout: defaultInstallHTTPTimeout},
		maxSize:    defaultMaxAssetBytes,
		maxArchive: defaultMaxArchiveBytes,
		maxEntries: defaultMaxArchiveEntries,
		freeBytes:  freeBytes,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Install implements Installer.Install. See the package-level design notes
// in autoupdate.go and the Install flow in docs/autoupdate.md (if present).
// On Windows this returns nil after posting a notify-only fallback.
func (r *RealInstaller) Install(ctx context.Context, host *plugin.Host, rel Release, cur string) error {
	_ = cur // retained for symmetry with Installer interface; surfaced via status text only.

	exe, asset, checksums, err := r.preflight(rel)
	if err != nil {
		if errors.Is(err, errPreflightWindows) {
			r.postWindowsNotice(host, rel)
			return nil
		}
		r.postError(host, err.Error())
		return err
	}

	r.postStatus(host, "⬇ downloading "+NormalizeVersion(rel.TagName), "yellow")

	parent := filepath.Dir(exe)

	// 1. Download the archive to a sibling tmpfile and compute its SHA-256.
	archiveTmp, sum, archiveSize, err := r.downloadArchive(ctx, asset, parent)
	if err != nil {
		r.postError(host, err.Error())
		return err
	}
	defer func() {
		// best-effort: archiveTmp is cleaned up once the binary is extracted;
		// this remove is the safety net on error paths that return before the
		// explicit remove below.
		if archiveTmp != "" {
			_ = os.Remove(archiveTmp)
		}
	}()
	_ = archiveSize // intentionally unused — we verify against Content-Length in download

	// 2. Fetch checksums.txt, find our asset's line, verify.
	if err := r.verifyChecksum(ctx, checksums, asset.Name, sum); err != nil {
		r.postError(host, err.Error())
		return err
	}

	// 3. Extract the single binary into a sibling tmpfile and chmod 0o755.
	binaryTmp, err := r.extractBinary(archiveTmp, asset.Name, parent)
	if err != nil {
		r.postError(host, err.Error())
		return err
	}

	// Archive is no longer needed.
	_ = os.Remove(archiveTmp)
	archiveTmp = "" // disarm the deferred cleanup above.

	// 4. Atomic swap: exe -> exe.prev, binaryTmp -> exe.
	prev := exe + ".prev"
	if err := os.Rename(exe, prev); err != nil {
		_ = os.Remove(binaryTmp)
		wrapped := fmt.Errorf("autoupdate: backup current binary: %w", err)
		r.postError(host, wrapped.Error())
		return wrapped
	}
	if err := os.Rename(binaryTmp, exe); err != nil {
		// Try to restore the previous binary so the editor keeps running.
		restoreErr := os.Rename(prev, exe)
		_ = os.Remove(binaryTmp)
		wrapped := fmt.Errorf("autoupdate: swap new binary into place: %w", err)
		if restoreErr != nil {
			wrapped = fmt.Errorf("%w (restore also failed: %v)", wrapped, restoreErr)
		}
		r.postError(host, wrapped.Error())
		return wrapped
	}

	// 5. Persist pending-restart state so Rollback and status-bar rendering
	// know what's in flight. We mutate only the two fields we own so a
	// concurrent checker tick's LastChecked/LastSeenVersion writes are not
	// clobbered.
	if err := r.mutateState(func(s *State) {
		s.PendingRestartVersion = rel.TagName
		s.PrevBinaryPath = prev
	}); err != nil {
		// State is advisory; we've already swapped binaries successfully.
		// Surface via notify but do not unwind.
		r.postError(host, "autoupdate: save state: "+err.Error())
	}

	r.postStatus(host, "⟳ restart to apply "+NormalizeVersion(rel.TagName), "yellow")
	if host != nil {
		_ = host.PostNotif("autoupdate", "ui/notify", map[string]string{
			"level":   "info",
			"message": "update installed — restart nistru to apply",
		})
	}
	return nil
}

// Rollback implements Installer.Rollback. It moves the ".prev" sibling back
// over the current binary, clears state, and notifies the user to restart.
func (r *RealInstaller) Rollback(_ context.Context, host *plugin.Host) error {
	prev := r.readPrevBinaryPath()
	if prev == "" || !fileExists(prev) {
		if host != nil {
			_ = host.PostNotif("autoupdate", "ui/notify", map[string]string{
				"level":   "warn",
				"message": "autoupdate: no previous binary to roll back to",
			})
		}
		return nil
	}

	exe, err := r.resolveExecutable()
	if err != nil {
		r.postError(host, err.Error())
		return err
	}

	if err := os.Rename(prev, exe); err != nil {
		wrapped := fmt.Errorf("autoupdate: restore previous binary: %w", err)
		r.postError(host, wrapped.Error())
		return wrapped
	}

	if err := r.mutateState(func(s *State) {
		s.PendingRestartVersion = ""
		s.PrevBinaryPath = ""
	}); err != nil {
		r.postError(host, "autoupdate: save state: "+err.Error())
	}

	r.postStatus(host, "", "")
	if host != nil {
		_ = host.PostNotif("autoupdate", "ui/notify", map[string]string{
			"level":   "info",
			"message": "rolled back — restart nistru to use the previous version",
		})
	}
	return nil
}

// readPrevBinaryPath returns the persisted PrevBinaryPath, using whichever
// seam is wired: the plugin's serialised updater (production path) or a
// direct LoadState (stand-alone installer tests). The updater case uses a
// no-op mutation so the path is read under the same mutex that writers use.
func (r *RealInstaller) readPrevBinaryPath() string {
	if r.updateStateF != nil {
		var out string
		_ = r.updateStateF(func(s *State) { out = s.PrevBinaryPath })
		return out
	}
	if r.statePath == "" {
		return ""
	}
	st, _ := LoadState(r.statePath)
	return st.PrevBinaryPath
}

// mutateState applies mut to the persisted state. When the installer has
// been wired with WithStateUpdater (production plugin path), the mutation
// flows through the plugin's serialised helper. Otherwise (stand-alone
// installer tests), it falls back to a local LoadState+SaveState which
// races against concurrent writers — this is only safe because those
// tests never run a checker concurrently.
func (r *RealInstaller) mutateState(mut func(*State)) error {
	if r.updateStateF != nil {
		return r.updateStateF(mut)
	}
	if r.statePath == "" {
		return nil
	}
	st, _ := LoadState(r.statePath)
	mut(&st)
	return SaveState(r.statePath, st)
}

// Cleanup deletes the .prev sibling when PendingRestartVersion has been
// cleared (e.g. the user restarted into the new binary). The plugin calls
// this on Shutdown via an interface assertion. Reads state via whichever
// seam is wired — prefers the plugin's serialised updater when present
// (production path), falls back to a direct LoadState (stand-alone tests).
func (r *RealInstaller) Cleanup(_ context.Context) error {
	var st State
	if r.updateStateF != nil {
		// Snapshot via a no-op mutation so we observe state under the
		// plugin's mutex without racing against a concurrent writer.
		_ = r.updateStateF(func(s *State) { st = *s })
	} else if r.statePath != "" {
		var err error
		if st, err = LoadState(r.statePath); err != nil {
			return nil // best-effort; forward-compat failures never block shutdown.
		}
	} else {
		return nil
	}
	if st.PendingRestartVersion != "" {
		// User has not yet restarted — keep the .prev so they can roll back.
		return nil
	}
	exe, err := r.resolveExecutable()
	if err != nil {
		return nil
	}
	prev := exe + ".prev"
	if fileExists(prev) {
		_ = os.Remove(prev)
	}
	return nil
}

// preflight validates everything Install needs before it starts touching
// the filesystem in earnest. Returns the resolved executable path plus the
// chosen binary / checksums Assets.
func (r *RealInstaller) preflight(rel Release) (string, Asset, Asset, error) {
	// Windows: bail out with the sentinel; Install renders the notify.
	if r.goos == "windows" {
		return "", Asset{}, Asset{}, errPreflightWindows
	}

	exe, err := r.resolveExecutable()
	if err != nil {
		return "", Asset{}, Asset{}, err
	}

	parent := filepath.Dir(exe)
	info, err := os.Stat(parent)
	if err != nil {
		return "", Asset{}, Asset{}, fmt.Errorf("autoupdate: stat parent dir: %w", err)
	}
	if !info.IsDir() {
		return "", Asset{}, Asset{}, fmt.Errorf("autoupdate: parent %q is not a directory", parent)
	}
	// Writability probe — creating and immediately removing a temp file is
	// the most portable test; permission errors surface here.
	probe, err := os.CreateTemp(parent, ".nistru-preflight-*")
	if err != nil {
		return "", Asset{}, Asset{}, fmt.Errorf("autoupdate: parent dir not writable: %w", err)
	}
	_ = probe.Close()
	_ = os.Remove(probe.Name())

	bin, cs, err := AssetMatch(rel, r.goos, r.goarch)
	if err != nil {
		return "", Asset{}, Asset{}, err
	}
	if cs.Name == "" {
		return "", Asset{}, Asset{}, errors.New("autoupdate: release has no checksums.txt; refusing to install")
	}

	// Disk space: require >= 2× asset.Size free. Fall back to permissive
	// behavior if the probe cannot answer.
	if r.freeBytes != nil && bin.Size > 0 {
		avail, ferr := r.freeBytes(parent)
		if ferr == nil && avail < 2*bin.Size {
			return "", Asset{}, Asset{}, fmt.Errorf(
				"autoupdate: insufficient free space in %q: need %d bytes, have %d",
				parent, 2*bin.Size, avail,
			)
		}
	}

	return exe, bin, cs, nil
}

// resolveExecutable resolves the running binary path and its symlink
// target. This matters when nistru is installed via `go install` which
// drops a symlink under $GOPATH/bin; we want to replace the real file.
func (r *RealInstaller) resolveExecutable() (string, error) {
	raw, err := r.executable()
	if err != nil {
		return "", fmt.Errorf("autoupdate: locate running binary: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(raw)
	if err != nil {
		return "", fmt.Errorf("autoupdate: resolve binary symlink %q: %w", raw, err)
	}
	return resolved, nil
}

// downloadArchive streams the asset into a tmpfile in parent and computes
// its SHA-256 in-flight. Returns the tmpfile path, the hex digest, and the
// number of bytes copied. The tmpfile is removed on any error.
func (r *RealInstaller) downloadArchive(ctx context.Context, asset Asset, parent string) (string, string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.BrowserDownloadURL, nil)
	if err != nil {
		return "", "", 0, fmt.Errorf("autoupdate: build download request: %w", err)
	}
	req.Header.Set("User-Agent", "nistru-autoupdate")
	resp, err := r.client.Do(req)
	if err != nil {
		return "", "", 0, fmt.Errorf("autoupdate: download asset: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", 0, fmt.Errorf("autoupdate: download asset: http %d", resp.StatusCode)
	}

	f, err := os.CreateTemp(parent, "nistru-download-*")
	if err != nil {
		return "", "", 0, fmt.Errorf("autoupdate: create download tmp: %w", err)
	}
	tmp := f.Name()
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	}

	h := sha256.New()
	limited := io.LimitReader(resp.Body, r.maxSize+1)
	w := io.MultiWriter(f, h)
	n, err := io.Copy(w, limited)
	if err != nil {
		cleanup()
		return "", "", 0, fmt.Errorf("autoupdate: copy asset: %w", err)
	}
	if n > r.maxSize {
		cleanup()
		return "", "", 0, fmt.Errorf("autoupdate: asset exceeds max size of %d bytes", r.maxSize)
	}
	// Content-Length sanity check: if present and positive, assert we got
	// all the bytes the server promised.
	if cl := resp.ContentLength; cl > 0 && n != cl {
		cleanup()
		return "", "", 0, fmt.Errorf("autoupdate: truncated download: got %d of %d bytes", n, cl)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", "", 0, fmt.Errorf("autoupdate: close download tmp: %w", err)
	}
	return tmp, hex.EncodeToString(h.Sum(nil)), n, nil
}

// verifyChecksum fetches checksums.txt, finds the line for assetName, and
// confirms it matches sum. checksums.txt format:
//
//	<hex sha256>  <filename>
//
// (two spaces between fields is conventional; one space is also accepted).
func (r *RealInstaller) verifyChecksum(ctx context.Context, checksums Asset, assetName, sum string) error {
	if checksums.BrowserDownloadURL == "" {
		return errors.New("autoupdate: release has no checksums.txt")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, checksums.BrowserDownloadURL, nil)
	if err != nil {
		return fmt.Errorf("autoupdate: build checksums request: %w", err)
	}
	req.Header.Set("User-Agent", "nistru-autoupdate")
	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("autoupdate: download checksums: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("autoupdate: download checksums: http %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxChecksumsBodyBytes))
	if err != nil {
		return fmt.Errorf("autoupdate: read checksums: %w", err)
	}

	expected, ok := findChecksum(body, assetName)
	if !ok {
		return fmt.Errorf("autoupdate: checksums.txt has no entry for %q", assetName)
	}
	if !strings.EqualFold(expected, sum) {
		return fmt.Errorf("autoupdate: checksum mismatch for %q: expected %s, got %s", assetName, expected, sum)
	}
	return nil
}

// findChecksum scans a checksums.txt body and returns the hex digest
// associated with name, or "" if not present.
func findChecksum(body []byte, name string) (string, bool) {
	for line := range strings.SplitSeq(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// First field is the digest; last field is the filename.
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		fname := fields[len(fields)-1]
		// Some generators emit "*<name>" with a binary-mode marker.
		fname = strings.TrimPrefix(fname, "*")
		if fname == name {
			return fields[0], true
		}
	}
	return "", false
}

// extractBinary walks a gzip-tar archive and writes the single executable
// entry to a sibling tmpfile in parent. The extracted file is chmodded 0o755.
//
// Rejections:
//   - any entry whose name contains "..".
//   - any regular-file entry larger than r.maxSize.
//   - archives containing more than one plausible binary (see isLikelyBinary).
//   - archives containing no binary entry at all.
//
// assetName is the archive's own filename, used only for the error text.
func (r *RealInstaller) extractBinary(archivePath, assetName, parent string) (string, error) {
	_ = assetName
	f, err := os.Open(archivePath)
	if err != nil {
		return "", fmt.Errorf("autoupdate: open archive: %w", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("autoupdate: gzip reader: %w", err)
	}
	defer gz.Close()

	// Cap total decompressed bytes so a tar bomb (gigabytes of zeros
	// compressed into kilobytes) cannot exhaust CPU/disk. The +1 lets us
	// distinguish "exactly at limit" from "overflow" with one extra byte.
	maxArchive := r.maxArchive
	if maxArchive <= 0 {
		maxArchive = defaultMaxArchiveBytes
	}
	maxEntries := r.maxEntries
	if maxEntries <= 0 {
		maxEntries = defaultMaxArchiveEntries
	}
	limited := &io.LimitedReader{R: gz, N: maxArchive + 1}
	tr := tar.NewReader(limited)

	out, err := os.CreateTemp(parent, "nistru-extract-*")
	if err != nil {
		return "", fmt.Errorf("autoupdate: create extract tmp: %w", err)
	}
	outPath := out.Name()
	cleanup := func() {
		_ = out.Close()
		_ = os.Remove(outPath)
	}

	var (
		found      bool
		foundCount int
		entryCount int
	)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			cleanup()
			return "", fmt.Errorf("autoupdate: read tar: %w", err)
		}
		entryCount++
		if entryCount > maxEntries {
			cleanup()
			return "", fmt.Errorf("autoupdate: archive exceeds max entry count of %d", maxEntries)
		}
		// Path-traversal guard: reject any entry whose name contains "..".
		if strings.Contains(hdr.Name, "..") {
			cleanup()
			return "", fmt.Errorf("autoupdate: rejected path-traversal entry %q", hdr.Name)
		}
		// Archive-bomb guard: any single entry whose declared size alone
		// would blow past the whole-archive cap is rejected before we
		// attempt to read it.
		if hdr.Size > maxArchive {
			cleanup()
			return "", fmt.Errorf("autoupdate: archive entry %q exceeds max decompressed size of %d bytes", hdr.Name, maxArchive)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if hdr.Size > r.maxSize {
			cleanup()
			return "", fmt.Errorf("autoupdate: archive entry %q exceeds max size", hdr.Name)
		}
		if !isLikelyBinary(hdr.Name) {
			continue
		}
		foundCount++
		if foundCount > 1 {
			cleanup()
			return "", errors.New("autoupdate: archive contains multiple candidate binaries")
		}
		if _, err := io.Copy(out, io.LimitReader(tr, r.maxSize+1)); err != nil {
			cleanup()
			return "", fmt.Errorf("autoupdate: extract binary: %w", err)
		}
		found = true
	}
	if limited.N <= 0 {
		// tar.Reader read past our budget.
		cleanup()
		return "", fmt.Errorf("autoupdate: archive exceeds max decompressed size of %d bytes", maxArchive)
	}

	if !found {
		cleanup()
		return "", errors.New("autoupdate: archive contains no nistru binary")
	}
	if err := out.Chmod(0o755); err != nil {
		cleanup()
		return "", fmt.Errorf("autoupdate: chmod extracted binary: %w", err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(outPath)
		return "", fmt.Errorf("autoupdate: close extracted binary: %w", err)
	}
	return outPath, nil
}

// isLikelyBinary returns true if the tar entry looks like the nistru
// executable. Only the basename is considered so archives that place the
// binary under a versioned dir (e.g. "nistru_v0.2.0_linux_amd64/nistru")
// still match. Anything else is ignored rather than rejected, so bundled
// README/LICENSE files don't trip the "multiple binaries" rule.
func isLikelyBinary(name string) bool {
	base := filepath.Base(name)
	return base == "nistru" || base == "nistru.exe"
}

// postStatus emits a statusBar/set notif. Empty text clears the segment.
func (r *RealInstaller) postStatus(host *plugin.Host, text, color string) {
	if host == nil {
		return
	}
	_ = host.PostNotif("autoupdate", "statusBar/set", map[string]string{
		"segment": "autoupdate",
		"text":    text,
		"color":   color,
	})
}

// postError surfaces err as an error-level ui/notify. A twin of the helper
// in autoupdate.go; kept here so install.go stays self-contained.
func (r *RealInstaller) postError(host *plugin.Host, msg string) {
	if host == nil {
		return
	}
	_ = host.PostNotif("autoupdate", "ui/notify", map[string]string{
		"level":   "error",
		"message": msg,
	})
}

// postWindowsNotice surfaces the notify-only fallback for Windows users.
// It does not touch the filesystem.
func (r *RealInstaller) postWindowsNotice(host *plugin.Host, rel Release) {
	if host == nil {
		return
	}
	tag := rel.TagName
	if tag == "" {
		tag = "latest"
	}
	cmd := fmt.Sprintf("go install github.com/savimcio/nistru/cmd/nistru@%s", tag)
	_ = host.PostNotif("autoupdate", "ui/notify", map[string]string{
		"level":   "warn",
		"message": "autoupdate: in-place install is not supported on Windows. Run: " + cmd,
	})
	_ = host.PostNotif("autoupdate", "statusBar/set", map[string]string{
		"segment": "autoupdate",
		"text":    "Run: " + cmd,
		"color":   "yellow",
	})
}

// fileExists is a small helper that returns true iff path resolves to a
// file the current user can stat. Errors (including permission denied)
// conservatively map to false so Rollback short-circuits cleanly.
func fileExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// Compile-time assertion that RealInstaller satisfies Installer.
var _ Installer = (*RealInstaller)(nil)
