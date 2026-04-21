package autoupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/savimcio/nistru/plugin"
)

// -----------------------------------------------------------------------------
// Scaffolding — kept local to install_test.go so the other tests in the
// package are unaffected.

// dummyExeContents is the byte pattern written to the "current binary" in
// every test before Install runs. Short-circuits confusion between "the old
// binary is in place" and "an empty file is in place".
var dummyExeContents = []byte("#!/bin/sh\necho OLD NISTRU\n")

// newInstallerSetup creates a temp dir, writes a dummy executable into it,
// and returns the dir and the exe path. The defensive check at the start
// enforces the "never touch real paths" rule from the task spec.
func newInstallerSetup(t *testing.T) (dir, exePath string) {
	t.Helper()
	dir = t.TempDir()
	exePath = filepath.Join(dir, "nistru")
	if err := os.WriteFile(exePath, dummyExeContents, 0o755); err != nil {
		t.Fatalf("seed dummy exe: %v", err)
	}

	// Defensive: explode immediately if dir is somehow under a real install
	// location. t.TempDir() returns under $TMPDIR, never under $GOPATH/bin;
	// this belt-and-braces check is cheap.
	for _, forbidden := range []string{"/nistru/bin/", "/.go/bin/", "/gopath/bin/"} {
		if strings.Contains(exePath, forbidden) {
			t.Fatalf("test exe path %q is under forbidden location %q", exePath, forbidden)
		}
	}
	return dir, exePath
}

// makeTarGz builds an in-memory gzipped tarball from entries. Each entry's
// Typeflag defaults to tar.TypeReg.
func makeTarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.Name,
			Mode:     0o755,
			Size:     int64(len(e.Body)),
			Typeflag: tar.TypeReg,
		}
		if e.Typeflag != 0 {
			hdr.Typeflag = e.Typeflag
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader %q: %v", e.Name, err)
		}
		if _, err := tw.Write(e.Body); err != nil {
			t.Fatalf("Write %q: %v", e.Name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tw.Close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gz.Close: %v", err)
	}
	return buf.Bytes()
}

type tarEntry struct {
	Name     string
	Body     []byte
	Typeflag byte
}

// sha256Hex returns the hex digest of data.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// releaseFixture bundles the Release JSON + archive bytes + checksums body
// the test needs to drive Install end-to-end.
type releaseFixture struct {
	Release       Release
	ArchiveName   string
	ArchiveBytes  []byte
	ChecksumsBody string
	newBinary     []byte
}

// defaultFixture returns a release with a single valid binary asset + a
// correct checksums.txt. Callers may mutate fields before starting the
// server.
func defaultFixture(t *testing.T) *releaseFixture {
	t.Helper()
	newBin := []byte("NEW NISTRU BINARY")
	archive := makeTarGz(t, []tarEntry{{Name: "nistru", Body: newBin}})
	name := "nistru_v99.0.0_darwin_arm64.tar.gz"
	checksums := fmt.Sprintf("%s  %s\n", sha256Hex(archive), name)

	return &releaseFixture{
		Release: Release{
			TagName: "v99.0.0",
			Name:    "99.0.0",
			Assets: []Asset{
				{Name: name, Size: int64(len(archive))},
				{Name: "checksums.txt", Size: int64(len(checksums))},
			},
		},
		ArchiveName:   name,
		ArchiveBytes:  archive,
		ChecksumsBody: checksums,
		newBinary:     newBin,
	}
}

// startAssetServer wires an httptest.Server that serves:
//
//	GET /{ArchiveName}   -> archive bytes
//	GET /checksums.txt   -> checksums body
//
// The fixture's Asset URLs are updated in place so AssetMatch returns the
// server-local URLs.
func startAssetServer(t *testing.T, fx *releaseFixture) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + fx.ArchiveName:
			w.Header().Set("Content-Type", "application/gzip")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(fx.ArchiveBytes)))
			_, _ = w.Write(fx.ArchiveBytes)
		case "/checksums.txt":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(fx.ChecksumsBody))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	// Rewire asset URLs to point at the server.
	for i := range fx.Release.Assets {
		fx.Release.Assets[i].BrowserDownloadURL = srv.URL + "/" + fx.Release.Assets[i].Name
	}
	return srv
}

// newTestInstaller builds a RealInstaller pointed at a fake executable in
// dir + a state file in dir, with GOOS/GOARCH defaulting to darwin/arm64.
func newTestInstaller(t *testing.T, dir, exePath string, opts ...InstallerOption) *RealInstaller {
	t.Helper()
	statePath := filepath.Join(dir, "state.json")
	base := []InstallerOption{
		WithExecutableFunc(func() (string, error) { return exePath, nil }),
		WithGoos("darwin"),
		WithGoarch("arm64"),
		WithInstallerStatePath(statePath),
		WithInstallerHTTPClient(&http.Client{Timeout: 5 * time.Second}),
		// Default: plenty of free space.
		withFreeBytesFunc(func(string) (int64, error) { return math.MaxInt64, nil }),
	}
	base = append(base, opts...)
	return NewInstaller(base...)
}

// readFile is a tiny helper that wraps os.ReadFile with t.Fatalf on error.
func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	return b
}

// -----------------------------------------------------------------------------
// Host recorder. Install/Rollback use *plugin.Host to PostNotif status and
// errors; we set up a real host (with no registered plugins) and drain its
// inbound channel into a thread-safe slice the tests assert on.

type recordedNotif struct {
	Method  string
	Payload map[string]string
}

type hostRecorder struct {
	host *plugin.Host
	mu   sync.Mutex
	msgs []recordedNotif
}

// newHostRecorder constructs a plugin.Host with no registered plugins and
// returns it together with a recorder. The recorder is populated on demand
// (via drain()) rather than from a background goroutine — Host has no
// public seam for closing its inbound channel, so a background drain would
// leak. Tests call drain() before asserting on the recorder.
func newHostRecorder(t *testing.T) (*plugin.Host, *hostRecorder) {
	t.Helper()
	reg := plugin.NewRegistry()
	h := plugin.NewHost(reg)
	if err := h.Start(""); err != nil {
		t.Fatalf("host.Start: %v", err)
	}
	rec := &hostRecorder{host: h}
	t.Cleanup(func() {
		// Shutdown is a no-op without registered plugins, but we still call
		// it so the host leaves no lingering resources. There is no goroutine
		// to join.
		_ = h.Shutdown(100 * time.Millisecond)
	})
	return h, rec
}

// drain pulls every currently-buffered message off the host's inbound
// channel using a short-timeout Recv() loop. The inbound channel is
// non-blocking from PostNotif's side (drop-on-full), so once Install/
// Rollback has returned, every notification we care about is already in
// the buffer.
func (r *hostRecorder) drain(maxWait time.Duration) {
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		msg, ok := recvWithTimeout(r.host, 20*time.Millisecond)
		if !ok {
			return
		}
		n, ok := msg.(plugin.PluginNotifMsg)
		if !ok {
			continue
		}
		payload := map[string]string{}
		_ = json.Unmarshal(n.Params, &payload)
		r.mu.Lock()
		r.msgs = append(r.msgs, recordedNotif{Method: n.Method, Payload: payload})
		r.mu.Unlock()
	}
}

// recvWithTimeout pulls one message from h.Recv() with a deadline. Since
// h.Recv() returns a blocking tea.Cmd, we invoke it in a goroutine and
// race it against a timer. The goroutine that wins picks up the value
// from the channel; we return false on timeout.
//
// This helper may leak a goroutine per timeout, which is acceptable for
// test lifetimes (the host's inbound channel has drop-on-full semantics,
// so a blocked Recv does no harm beyond the stranded goroutine).
func recvWithTimeout(h *plugin.Host, timeout time.Duration) (any, bool) {
	out := make(chan any, 1)
	go func() {
		if cmd := h.Recv(); cmd != nil {
			if v := cmd(); v != nil {
				out <- v
			}
		}
		close(out)
	}()
	select {
	case v, ok := <-out:
		if !ok {
			return nil, false
		}
		return v, true
	case <-time.After(timeout):
		return nil, false
	}
}

// snapshot returns a copy of the messages recorded so far.
func (r *hostRecorder) snapshot() []recordedNotif {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recordedNotif, len(r.msgs))
	copy(out, r.msgs)
	return out
}

// waitFor drains up to timeout and returns true iff any recorded notif
// satisfies pred at any point during the drain.
func (r *hostRecorder) waitFor(timeout time.Duration, pred func(recordedNotif) bool) bool {
	r.drain(timeout)
	return slices.ContainsFunc(r.snapshot(), pred)
}

// -----------------------------------------------------------------------------
// Preflight tests.

func TestPreflightAllGood(t *testing.T) {
	dir, exePath := newInstallerSetup(t)
	fx := defaultFixture(t)

	r := newTestInstaller(t, dir, exePath)
	gotExe, bin, cs, err := r.preflight(fx.Release)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	want, _ := filepath.EvalSymlinks(exePath)
	if gotExe != want {
		t.Fatalf("preflight exe = %q, want %q", gotExe, want)
	}
	if bin.Name != fx.ArchiveName {
		t.Fatalf("binary = %q, want %q", bin.Name, fx.ArchiveName)
	}
	if cs.Name != "checksums.txt" {
		t.Fatalf("checksums = %q, want checksums.txt", cs.Name)
	}
}

func TestPreflightUnwritableDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores POSIX permissions; test relies on EACCES")
	}
	dir, exePath := newInstallerSetup(t)
	fx := defaultFixture(t)

	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod ro: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	r := newTestInstaller(t, dir, exePath)
	_, _, _, err := r.preflight(fx.Release)
	if err == nil || !strings.Contains(err.Error(), "not writable") {
		t.Fatalf("preflight err = %v, want 'not writable'", err)
	}
}

func TestPreflightNoAssetForPlatform(t *testing.T) {
	dir, exePath := newInstallerSetup(t)
	fx := defaultFixture(t)

	r := newTestInstaller(t, dir, exePath, WithGoos("linux"), WithGoarch("s390x"))
	_, _, _, err := r.preflight(fx.Release)
	if !errors.Is(err, ErrNoAssetForPlatform) {
		t.Fatalf("preflight err = %v, want ErrNoAssetForPlatform", err)
	}
}

func TestPreflightMissingChecksums(t *testing.T) {
	dir, exePath := newInstallerSetup(t)
	fx := defaultFixture(t)
	// Drop the checksums asset.
	fx.Release.Assets = fx.Release.Assets[:1]

	r := newTestInstaller(t, dir, exePath)
	_, _, _, err := r.preflight(fx.Release)
	if err == nil || !strings.Contains(err.Error(), "checksums") {
		t.Fatalf("preflight err = %v, want 'checksums' error", err)
	}
}

func TestPreflightInsufficientSpace(t *testing.T) {
	dir, exePath := newInstallerSetup(t)
	fx := defaultFixture(t)
	// Bump asset.Size so 2× > our fake free space.
	fx.Release.Assets[0].Size = 10 * 1024

	r := newTestInstaller(t, dir, exePath,
		withFreeBytesFunc(func(string) (int64, error) { return 1024, nil }),
	)
	_, _, _, err := r.preflight(fx.Release)
	if err == nil || !strings.Contains(err.Error(), "insufficient free space") {
		t.Fatalf("preflight err = %v, want 'insufficient free space'", err)
	}
}

func TestPreflightWindowsSkipsSwap(t *testing.T) {
	dir, exePath := newInstallerSetup(t)
	fx := defaultFixture(t)

	host, rec := newHostRecorder(t)
	r := newTestInstaller(t, dir, exePath, WithGoos("windows"))

	if err := r.Install(context.Background(), host, fx.Release, "v0.1.0"); err != nil {
		t.Fatalf("Install on windows returned err=%v, want nil (notify-only)", err)
	}

	if got := readFile(t, exePath); !bytes.Equal(got, dummyExeContents) {
		t.Fatalf("exe was modified on windows path; got %q", got)
	}
	if _, err := os.Stat(exePath + ".prev"); err == nil {
		t.Fatalf(".prev file exists on windows path")
	}
	want := "go install github.com/savimcio/nistru/cmd/nistru@v99.0.0"
	if !rec.waitFor(500*time.Millisecond, func(m recordedNotif) bool {
		return m.Method == "ui/notify" && strings.Contains(m.Payload["message"], want)
	}) {
		t.Fatalf("no ui/notify with %q in inbox: %+v", want, rec.snapshot())
	}
}

// -----------------------------------------------------------------------------
// Install tests.

func TestInstallHappyPath(t *testing.T) {
	dir, exePath := newInstallerSetup(t)
	fx := defaultFixture(t)
	_ = startAssetServer(t, fx)

	host, rec := newHostRecorder(t)
	r := newTestInstaller(t, dir, exePath)

	if err := r.Install(context.Background(), host, fx.Release, "v0.1.0"); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// Assertions use the resolved path — macOS maps /var -> /private/var, so
	// the installer's EvalSymlinks-normalized path may differ from the raw
	// t.TempDir() path we seeded with.
	resolvedExe, err := filepath.EvalSymlinks(exePath)
	if err != nil {
		t.Fatalf("EvalSymlinks(exe): %v", err)
	}
	if got := readFile(t, resolvedExe); !bytes.Equal(got, fx.newBinary) {
		t.Fatalf("exe contents = %q, want %q", got, fx.newBinary)
	}
	prev := resolvedExe + ".prev"
	if got := readFile(t, prev); !bytes.Equal(got, dummyExeContents) {
		t.Fatalf("prev contents = %q, want old dummy contents", got)
	}
	st, err := LoadState(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if st.PendingRestartVersion != "v99.0.0" {
		t.Fatalf("state.PendingRestartVersion = %q, want v99.0.0", st.PendingRestartVersion)
	}
	if st.PrevBinaryPath != prev {
		t.Fatalf("state.PrevBinaryPath = %q, want %q", st.PrevBinaryPath, prev)
	}
	if !rec.waitFor(500*time.Millisecond, func(m recordedNotif) bool {
		return m.Method == "statusBar/set" && strings.Contains(m.Payload["text"], "restart to apply")
	}) {
		t.Fatalf("expected statusBar/set restart message: %+v", rec.snapshot())
	}
	if !rec.waitFor(500*time.Millisecond, func(m recordedNotif) bool {
		return m.Method == "ui/notify" && m.Payload["level"] == "info" &&
			strings.Contains(m.Payload["message"], "update installed")
	}) {
		t.Fatalf("expected info notify 'update installed': %+v", rec.snapshot())
	}
}

func TestInstallWrongChecksum(t *testing.T) {
	dir, exePath := newInstallerSetup(t)
	fx := defaultFixture(t)
	fx.ChecksumsBody = "deadbeef" + strings.Repeat("0", 56) + "  " + fx.ArchiveName + "\n"
	_ = startAssetServer(t, fx)

	host, _ := newHostRecorder(t)
	r := newTestInstaller(t, dir, exePath)

	err := r.Install(context.Background(), host, fx.Release, "v0.1.0")
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("Install err = %v, want checksum mismatch", err)
	}
	if got := readFile(t, exePath); !bytes.Equal(got, dummyExeContents) {
		t.Fatalf("exe was modified on checksum failure: %q", got)
	}
	if _, err := os.Stat(exePath + ".prev"); err == nil {
		t.Fatalf(".prev file exists despite checksum failure")
	}
	st, _ := LoadState(filepath.Join(dir, "state.json"))
	if st.PendingRestartVersion != "" || st.PrevBinaryPath != "" {
		t.Fatalf("state mutated on failure: %+v", st)
	}
}

func TestInstallOversizedAsset(t *testing.T) {
	dir, exePath := newInstallerSetup(t)
	fx := defaultFixture(t)

	bigBin := bytes.Repeat([]byte{0x42}, 2*1024*1024)
	fx.ArchiveBytes = makeTarGz(t, []tarEntry{{Name: "nistru", Body: bigBin}})
	fx.Release.Assets[0].Size = int64(len(fx.ArchiveBytes))
	fx.ChecksumsBody = fmt.Sprintf("%s  %s\n", sha256Hex(fx.ArchiveBytes), fx.ArchiveName)
	_ = startAssetServer(t, fx)

	host, _ := newHostRecorder(t)
	r := newTestInstaller(t, dir, exePath,
		WithMaxSize(1024*1024),
	)

	err := r.Install(context.Background(), host, fx.Release, "v0.1.0")
	if err == nil || !strings.Contains(err.Error(), "exceeds max size") {
		t.Fatalf("Install err = %v, want 'exceeds max size'", err)
	}
	if got := readFile(t, exePath); !bytes.Equal(got, dummyExeContents) {
		t.Fatalf("exe was modified on oversize failure: %q", got)
	}
}

func TestInstallTruncatedDownload(t *testing.T) {
	dir, exePath := newInstallerSetup(t)
	fx := defaultFixture(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + fx.ArchiveName:
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(fx.ArchiveBytes)))
			w.WriteHeader(http.StatusOK)
			half := len(fx.ArchiveBytes) / 2
			_, _ = w.Write(fx.ArchiveBytes[:half])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			hj, ok := w.(http.Hijacker)
			if !ok {
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				return
			}
			_ = conn.Close()
		case "/checksums.txt":
			_, _ = w.Write([]byte(fx.ChecksumsBody))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	fx.Release.Assets[0].BrowserDownloadURL = srv.URL + "/" + fx.ArchiveName
	fx.Release.Assets[1].BrowserDownloadURL = srv.URL + "/checksums.txt"

	host, _ := newHostRecorder(t)
	r := newTestInstaller(t, dir, exePath)

	err := r.Install(context.Background(), host, fx.Release, "v0.1.0")
	if err == nil {
		t.Fatalf("Install err = nil, want some error (truncated stream)")
	}
	if got := readFile(t, exePath); !bytes.Equal(got, dummyExeContents) {
		t.Fatalf("exe modified on truncated download: %q", got)
	}
}

func TestInstallTarWithMultipleBinaries(t *testing.T) {
	dir, exePath := newInstallerSetup(t)
	fx := defaultFixture(t)

	fx.ArchiveBytes = makeTarGz(t, []tarEntry{
		{Name: "nistru", Body: []byte("A")},
		{Name: "pkgdir/nistru", Body: []byte("B")},
	})
	fx.ChecksumsBody = fmt.Sprintf("%s  %s\n", sha256Hex(fx.ArchiveBytes), fx.ArchiveName)
	_ = startAssetServer(t, fx)

	host, _ := newHostRecorder(t)
	r := newTestInstaller(t, dir, exePath)

	err := r.Install(context.Background(), host, fx.Release, "v0.1.0")
	if err == nil || !strings.Contains(err.Error(), "multiple candidate binaries") {
		t.Fatalf("Install err = %v, want multiple binaries", err)
	}
	if got := readFile(t, exePath); !bytes.Equal(got, dummyExeContents) {
		t.Fatalf("exe modified on multi-binary archive: %q", got)
	}
}

func TestInstallTarWithPathTraversal(t *testing.T) {
	dir, exePath := newInstallerSetup(t)
	fx := defaultFixture(t)

	fx.ArchiveBytes = makeTarGz(t, []tarEntry{
		{Name: "../../etc/passwd", Body: []byte("root::0:0:root:/root:/bin/sh\n")},
	})
	fx.ChecksumsBody = fmt.Sprintf("%s  %s\n", sha256Hex(fx.ArchiveBytes), fx.ArchiveName)
	_ = startAssetServer(t, fx)

	host, _ := newHostRecorder(t)
	r := newTestInstaller(t, dir, exePath)

	err := r.Install(context.Background(), host, fx.Release, "v0.1.0")
	if err == nil || !strings.Contains(err.Error(), "path-traversal") {
		t.Fatalf("Install err = %v, want path-traversal", err)
	}
	if got := readFile(t, exePath); !bytes.Equal(got, dummyExeContents) {
		t.Fatalf("exe modified on traversal archive: %q", got)
	}
}

func TestInstallTarWithNoBinary(t *testing.T) {
	dir, exePath := newInstallerSetup(t)
	fx := defaultFixture(t)

	fx.ArchiveBytes = makeTarGz(t, []tarEntry{
		{Name: "README.md", Body: []byte("# Nistru")},
	})
	fx.ChecksumsBody = fmt.Sprintf("%s  %s\n", sha256Hex(fx.ArchiveBytes), fx.ArchiveName)
	_ = startAssetServer(t, fx)

	host, _ := newHostRecorder(t)
	r := newTestInstaller(t, dir, exePath)

	err := r.Install(context.Background(), host, fx.Release, "v0.1.0")
	if err == nil || !strings.Contains(err.Error(), "no nistru binary") {
		t.Fatalf("Install err = %v, want no nistru binary", err)
	}
	if got := readFile(t, exePath); !bytes.Equal(got, dummyExeContents) {
		t.Fatalf("exe modified on no-binary archive: %q", got)
	}
}

// TestInstallSwapFailsReverts: pre-create a directory at the .prev path so
// os.Rename(exe, exe+".prev") fails (overwriting a non-empty dir with a
// regular file is not permitted). Confirm the exe is left untouched.
func TestInstallSwapFailsReverts(t *testing.T) {
	dir, exePath := newInstallerSetup(t)
	fx := defaultFixture(t)
	_ = startAssetServer(t, fx)

	prevDir := exePath + ".prev"
	if err := os.Mkdir(prevDir, 0o755); err != nil {
		t.Fatalf("mkdir prev: %v", err)
	}
	if err := os.WriteFile(filepath.Join(prevDir, "child"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed prev child: %v", err)
	}

	host, _ := newHostRecorder(t)
	r := newTestInstaller(t, dir, exePath)

	err := r.Install(context.Background(), host, fx.Release, "v0.1.0")
	if err == nil {
		t.Fatalf("Install err = nil, want swap failure")
	}
	if got := readFile(t, exePath); !bytes.Equal(got, dummyExeContents) {
		t.Fatalf("exe modified after swap failure: %q", got)
	}
}

// -----------------------------------------------------------------------------
// Rollback tests.

func TestRollbackSuccess(t *testing.T) {
	dir, exePath := newInstallerSetup(t)
	newContents := []byte("NEW NISTRU")
	if err := os.WriteFile(exePath, newContents, 0o755); err != nil {
		t.Fatalf("seed new: %v", err)
	}
	prev := exePath + ".prev"
	if err := os.WriteFile(prev, dummyExeContents, 0o755); err != nil {
		t.Fatalf("seed prev: %v", err)
	}
	statePath := filepath.Join(dir, "state.json")
	if err := SaveState(statePath, State{
		PendingRestartVersion: "v99.0.0",
		PrevBinaryPath:        prev,
	}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	host, _ := newHostRecorder(t)
	r := newTestInstaller(t, dir, exePath)

	if err := r.Rollback(context.Background(), host); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if got := readFile(t, exePath); !bytes.Equal(got, dummyExeContents) {
		t.Fatalf("exe after rollback = %q, want old contents", got)
	}
	if _, err := os.Stat(prev); err == nil {
		t.Fatalf(".prev still exists after rollback")
	}
	st, _ := LoadState(statePath)
	if st.PendingRestartVersion != "" || st.PrevBinaryPath != "" {
		t.Fatalf("state not cleared: %+v", st)
	}
}

func TestRollbackNoPrev(t *testing.T) {
	dir, exePath := newInstallerSetup(t)
	host, rec := newHostRecorder(t)
	r := newTestInstaller(t, dir, exePath)

	if err := r.Rollback(context.Background(), host); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if got := readFile(t, exePath); !bytes.Equal(got, dummyExeContents) {
		t.Fatalf("exe modified on no-prev rollback: %q", got)
	}
	if !rec.waitFor(500*time.Millisecond, func(m recordedNotif) bool {
		return m.Method == "ui/notify" && m.Payload["level"] == "warn" &&
			strings.Contains(m.Payload["message"], "no previous binary")
	}) {
		t.Fatalf("expected 'no previous binary' notify, got %+v", rec.snapshot())
	}
}

func TestRollbackIdempotent(t *testing.T) {
	dir, exePath := newInstallerSetup(t)
	prev := exePath + ".prev"
	if err := os.WriteFile(prev, dummyExeContents, 0o755); err != nil {
		t.Fatalf("seed prev: %v", err)
	}
	statePath := filepath.Join(dir, "state.json")
	if err := SaveState(statePath, State{
		PendingRestartVersion: "v99.0.0",
		PrevBinaryPath:        prev,
	}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	host, _ := newHostRecorder(t)
	r := newTestInstaller(t, dir, exePath)

	if err := r.Rollback(context.Background(), host); err != nil {
		t.Fatalf("first Rollback: %v", err)
	}
	if err := r.Rollback(context.Background(), host); err != nil {
		t.Fatalf("second Rollback: %v", err)
	}
	if got := readFile(t, exePath); !bytes.Equal(got, dummyExeContents) {
		t.Fatalf("exe after double rollback = %q", got)
	}
}

// -----------------------------------------------------------------------------
// Cleanup tests.

func TestCleanupDeletesPrevAfterPendingCleared(t *testing.T) {
	dir, exePath := newInstallerSetup(t)
	prev := exePath + ".prev"
	if err := os.WriteFile(prev, []byte("old"), 0o755); err != nil {
		t.Fatalf("seed prev: %v", err)
	}
	statePath := filepath.Join(dir, "state.json")
	if err := SaveState(statePath, State{}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	r := newTestInstaller(t, dir, exePath)
	if err := r.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if _, err := os.Stat(prev); err == nil {
		t.Fatalf(".prev still exists after cleanup")
	}
	if got := readFile(t, exePath); !bytes.Equal(got, dummyExeContents) {
		t.Fatalf("exe modified by cleanup: %q", got)
	}
}

func TestCleanupLeavesPrevIfPendingSet(t *testing.T) {
	dir, exePath := newInstallerSetup(t)
	prev := exePath + ".prev"
	if err := os.WriteFile(prev, []byte("old"), 0o755); err != nil {
		t.Fatalf("seed prev: %v", err)
	}
	statePath := filepath.Join(dir, "state.json")
	if err := SaveState(statePath, State{
		PendingRestartVersion: "v99.0.0",
		PrevBinaryPath:        prev,
	}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	r := newTestInstaller(t, dir, exePath)
	if err := r.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if _, err := os.Stat(prev); err != nil {
		t.Fatalf(".prev was removed while PendingRestartVersion set: %v", err)
	}
}

// -----------------------------------------------------------------------------
// findChecksum unit test.

func TestFindChecksum(t *testing.T) {
	body := []byte(
		"# comment line\n" +
			"abc123  nistru_darwin_arm64.tar.gz\n" +
			"def456 *nistru_linux_amd64.tar.gz\n" +
			"\n",
	)
	cases := []struct {
		name string
		want string
		ok   bool
	}{
		{"nistru_darwin_arm64.tar.gz", "abc123", true},
		{"nistru_linux_amd64.tar.gz", "def456", true},
		{"nistru_missing.tar.gz", "", false},
	}
	for _, tc := range cases {
		got, ok := findChecksum(body, tc.name)
		if ok != tc.ok || got != tc.want {
			t.Errorf("findChecksum(%q) = (%q,%v), want (%q,%v)", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

// -----------------------------------------------------------------------------
// Archive-bomb defence (Finding 4 regression).

// TestInstallRejectsTarBombDeclaredSize seeds a tar whose single header
// declares a size far larger than any sane binary. The pre-read guard must
// reject it before any bytes are copied.
func TestInstallRejectsTarBombDeclaredSize(t *testing.T) {
	dir, exePath := newInstallerSetup(t)
	fx := defaultFixture(t)

	// Build a tarball whose header claims 2 MiB but the body is a single
	// byte. The installer must reject on the header check, not during copy.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{
		Name:     "nistru",
		Mode:     0o755,
		Size:     2 * 1024 * 1024,
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	// Write one byte so the tar writer is happy; we'll short-circuit during
	// header inspection anyway.
	_, _ = tw.Write(bytes.Repeat([]byte{0}, 2*1024*1024))
	_ = tw.Close()
	_ = gz.Close()

	fx.ArchiveBytes = buf.Bytes()
	fx.ChecksumsBody = fmt.Sprintf("%s  %s\n", sha256Hex(fx.ArchiveBytes), fx.ArchiveName)
	_ = startAssetServer(t, fx)

	host, _ := newHostRecorder(t)
	// Cap the whole archive at 1 MiB so the 2 MiB entry trips the guard.
	r := newTestInstaller(t, dir, exePath,
		WithMaxArchiveBytes(1024*1024),
	)

	err := r.Install(context.Background(), host, fx.Release, "v0.1.0")
	if err == nil {
		t.Fatalf("Install err = nil, want tar-bomb rejection")
	}
	if !strings.Contains(err.Error(), "max decompressed") && !strings.Contains(err.Error(), "exceeds max size") {
		t.Fatalf("Install err = %v, want 'max decompressed' or 'exceeds max size'", err)
	}
	if got := readFile(t, exePath); !bytes.Equal(got, dummyExeContents) {
		t.Fatalf("exe modified on tar-bomb rejection: %q", got)
	}
	if _, err := os.Stat(exePath + ".prev"); err == nil {
		t.Fatalf(".prev was created for a rejected archive")
	}
}

// TestInstallRejectsTarBombEntryCount seeds a tar with more entries than
// the entry-count cap allows. Confirms the counter-based guard trips.
func TestInstallRejectsTarBombEntryCount(t *testing.T) {
	dir, exePath := newInstallerSetup(t)
	fx := defaultFixture(t)

	// 50 zero-byte entries; cap the installer at 10 so the guard trips on
	// the 11th header.
	entries := make([]tarEntry, 50)
	for i := range entries {
		entries[i] = tarEntry{Name: fmt.Sprintf("padding/e%03d", i), Body: nil}
	}
	fx.ArchiveBytes = makeTarGz(t, entries)
	fx.ChecksumsBody = fmt.Sprintf("%s  %s\n", sha256Hex(fx.ArchiveBytes), fx.ArchiveName)
	_ = startAssetServer(t, fx)

	host, _ := newHostRecorder(t)
	r := newTestInstaller(t, dir, exePath, WithMaxArchiveEntries(10))

	err := r.Install(context.Background(), host, fx.Release, "v0.1.0")
	if err == nil || !strings.Contains(err.Error(), "max entry count") {
		t.Fatalf("Install err = %v, want 'max entry count'", err)
	}
	if got := readFile(t, exePath); !bytes.Equal(got, dummyExeContents) {
		t.Fatalf("exe modified on entry-count rejection: %q", got)
	}
}

// -----------------------------------------------------------------------------
// State race regression tests (Findings 1 & 2).

// TestConcurrentCheckerAndInstallStateRace fires an install alongside a
// simulated checker tick that mutates LastChecked + LastSeenVersion. Both
// field sets must appear in the final state.json: the pre-fix read-modify-
// write race would have let the checker's stale snapshot clobber the
// installer's pending-restart write.
func TestConcurrentCheckerAndInstallStateRace(t *testing.T) {
	dir, exePath := newInstallerSetup(t)
	fx := defaultFixture(t)
	_ = startAssetServer(t, fx)

	statePath := filepath.Join(dir, "state.json")

	// Use a real Plugin so the serialised updateState helper is exercised
	// end-to-end. We skip New()'s env reads by constructing directly.
	p := &Plugin{
		name:      "autoupdate",
		repo:      "owner/repo",
		interval:  time.Hour,
		client:    &http.Client{Timeout: 5 * time.Second},
		now:       time.Now,
		current:   "v0.1.0",
		statePath: statePath,
	}
	// Wire the installer to use the plugin's serialised updater. Also
	// supply the same statePath so Rollback's read seam (which consults
	// statePath when updateStateF is absent) can observe state in future
	// stand-alone tests of the installer.
	inst := NewInstaller(
		WithExecutableFunc(func() (string, error) { return exePath, nil }),
		WithGoos("darwin"),
		WithGoarch("arm64"),
		WithInstallerHTTPClient(&http.Client{Timeout: 5 * time.Second}),
		WithInstallerStatePath(statePath),
		WithStateUpdater(p.updateState),
		withFreeBytesFunc(func(string) (int64, error) { return math.MaxInt64, nil }),
	)
	p.installer = inst

	host, _ := newHostRecorder(t)

	var wg sync.WaitGroup
	wg.Add(2)

	// Simulated checker tick: write LastChecked + LastSeenVersion via the
	// same serialised path the real checker uses.
	lastChecked := time.Now().UTC().Truncate(time.Second)
	go func() {
		defer wg.Done()
		if err := p.updateState(func(s *State) {
			s.LastChecked = lastChecked
			s.LastSeenVersion = "v0.1.0-checker"
		}); err != nil {
			t.Errorf("checker updateState: %v", err)
		}
	}()

	go func() {
		defer wg.Done()
		if err := inst.Install(context.Background(), host, fx.Release, "v0.1.0"); err != nil {
			t.Errorf("Install: %v", err)
		}
	}()

	wg.Wait()

	got, err := LoadState(statePath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got.PendingRestartVersion != "v99.0.0" {
		t.Fatalf("PendingRestartVersion = %q, want v99.0.0 (installer write lost)", got.PendingRestartVersion)
	}
	resolvedExe, _ := filepath.EvalSymlinks(exePath)
	if got.PrevBinaryPath != resolvedExe+".prev" {
		t.Fatalf("PrevBinaryPath = %q, want %q (installer write lost)", got.PrevBinaryPath, resolvedExe+".prev")
	}
	if got.LastSeenVersion != "v0.1.0-checker" {
		t.Fatalf("LastSeenVersion = %q, want v0.1.0-checker (checker write lost)", got.LastSeenVersion)
	}
	if !got.LastChecked.Equal(lastChecked) {
		t.Fatalf("LastChecked = %v, want %v (checker write lost)", got.LastChecked, lastChecked)
	}
}

// TestSwitchChannelRaceWithChecker drives a real Plugin end-to-end with a
// slow checker tick. While the tick is parked inside FetchReleases, the
// test dispatches switch-channel. After the tick unblocks and completes,
// the channel must still be "dev" and the checker's LastChecked must also
// be recorded. Pre-fix the checker and switch-channel both did their own
// read-modify-write cycles against p.state and persisted whole snapshots;
// interleaving could drop either side's fields.
func TestSwitchChannelRaceWithChecker(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	t.Setenv(envDisable, "")
	t.Setenv(envRepo, "")
	t.Setenv(envChannel, "")
	t.Setenv(envInterval, "")

	// Slow transport: first release-list request blocks on a gate so the
	// tick is observably in-flight while we dispatch switch-channel.
	gate := make(chan struct{})
	var once sync.Once
	slow := &slowReleaseTransport{
		body:  []byte(`[]`),
		gate:  gate,
		first: &once,
	}
	client := &http.Client{Transport: slow, Timeout: 5 * time.Second}

	p := New(
		WithRepo("owner/repo"),
		WithHTTPClient(client),
		WithInterval(time.Hour), // only the immediate tick matters.
		WithStatePath(statePath),
		WithCurrent("v0.1.0"),
	)
	_ = newTestHost(t, p)

	_ = p.OnEvent(plugin.Initialize{RootPath: t.TempDir()})

	// Wait for the checker to enter fetch so it is truly parked.
	waitUntil(t, 2*time.Second, func() bool { return slow.hit.Load() >= 1 })

	// Flip to dev while the tick is parked inside FetchReleases.
	_ = p.OnEvent(plugin.ExecuteCommand{ID: "autoupdate:switch-channel"})

	// The switch persists synchronously via updateState.
	waitUntil(t, 2*time.Second, func() bool {
		st, err := LoadState(statePath)
		return err == nil && st.Channel == "dev"
	})

	// Let the tick finish. With all state writes funnelled through
	// updateState, the tick's LastChecked save cannot regress the channel.
	close(gate)

	// The tick posts a statusBar update after its save returns; wait until
	// LastChecked appears on disk so we know the save landed.
	waitUntil(t, 2*time.Second, func() bool {
		st, err := LoadState(statePath)
		return err == nil && !st.LastChecked.IsZero()
	})

	st, err := LoadState(statePath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if st.Channel != "dev" {
		t.Fatalf("Channel = %q after checker finished, want dev (checker clobbered switch-channel)", st.Channel)
	}
}

// stubTransport returns a fixed body for every request. Used so tests can
// build a real Plugin without exercising the real network.
type stubTransport struct {
	body []byte
	hits atomic.Int64
}

func (s *stubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.hits.Add(1)
	return &http.Response{
		StatusCode:    http.StatusOK,
		Body:          io.NopCloser(bytes.NewReader(s.body)),
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		Request:       req,
		ContentLength: int64(len(s.body)),
	}, nil
}

// slowReleaseTransport blocks the first release-list request on gate so a
// test can inject ordered operations between fetch-start and fetch-end.
// Subsequent requests pass through immediately.
type slowReleaseTransport struct {
	body  []byte
	gate  chan struct{}
	first *sync.Once
	hit   atomic.Int64
}

func (s *slowReleaseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.hit.Add(1)
	s.first.Do(func() { <-s.gate })
	return &http.Response{
		StatusCode:    http.StatusOK,
		Body:          io.NopCloser(bytes.NewReader(s.body)),
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		Request:       req,
		ContentLength: int64(len(s.body)),
	}, nil
}

// -----------------------------------------------------------------------------
// Initialize reconciliation regression (Finding 3).

// TestInitializeFinalizesPendingRestart seeds state with a pending version
// that matches the running binary (i.e. the user already restarted).
// Initialize must clear the pending fields and remove the .prev file.
func TestInitializeFinalizesPendingRestart(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	prev := filepath.Join(dir, "nistru.prev")
	if err := os.WriteFile(prev, []byte("old binary"), 0o755); err != nil {
		t.Fatalf("seed prev: %v", err)
	}
	if err := SaveState(statePath, State{
		PendingRestartVersion: "v1.2.3",
		PrevBinaryPath:        prev,
	}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	t.Setenv(envDisable, "")
	t.Setenv(envRepo, "")
	t.Setenv(envChannel, "")
	t.Setenv(envInterval, "")

	p := New(
		WithRepo("owner/repo"),
		WithHTTPClient(&http.Client{Transport: &stubTransport{body: []byte(`[]`)}}),
		WithStatePath(statePath),
		WithCurrent("v1.2.3"),
		WithVersionFunc(func() string { return "v1.2.3" }),
		WithInstaller(&recordingInstaller{}),
		WithInterval(time.Hour),
	)
	_ = newTestHost(t, p)

	_ = p.OnEvent(plugin.Initialize{RootPath: t.TempDir()})

	waitUntil(t, 2*time.Second, func() bool {
		st, err := LoadState(statePath)
		return err == nil && st.PendingRestartVersion == "" && st.PrevBinaryPath == ""
	})
	if _, err := os.Stat(prev); err == nil {
		t.Fatalf(".prev file was not removed after successful restart reconciliation")
	}
}

// TestInitializeKeepsPendingWhenVersionMismatch confirms reconciliation is
// a no-op when the running binary is still the *old* one (user has not
// restarted yet). The .prev must stay put so rollback works.
func TestInitializeKeepsPendingWhenVersionMismatch(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	prev := filepath.Join(dir, "nistru.prev")
	if err := os.WriteFile(prev, []byte("old binary"), 0o755); err != nil {
		t.Fatalf("seed prev: %v", err)
	}
	if err := SaveState(statePath, State{
		PendingRestartVersion: "v1.2.3",
		PrevBinaryPath:        prev,
	}); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	t.Setenv(envDisable, "")
	t.Setenv(envRepo, "")
	t.Setenv(envChannel, "")
	t.Setenv(envInterval, "")

	p := New(
		WithRepo("owner/repo"),
		WithHTTPClient(&http.Client{Transport: &stubTransport{body: []byte(`[]`)}}),
		WithStatePath(statePath),
		WithCurrent("v1.2.2"),
		WithVersionFunc(func() string { return "v1.2.2" }),
		WithInstaller(&recordingInstaller{}),
		WithInterval(time.Hour),
	)
	_ = newTestHost(t, p)

	_ = p.OnEvent(plugin.Initialize{RootPath: t.TempDir()})

	// Give the checker one immediate tick of headroom so our assertion
	// isn't racing a just-started goroutine. The tick cannot mutate
	// PendingRestart/PrevBinaryPath by design (only Install/Rollback do).
	waitUntil(t, 2*time.Second, func() bool {
		st, err := LoadState(statePath)
		return err == nil && !st.LastChecked.IsZero()
	})
	st, _ := LoadState(statePath)
	if st.PendingRestartVersion != "v1.2.3" {
		t.Fatalf("PendingRestartVersion cleared despite version mismatch: %+v", st)
	}
	if _, err := os.Stat(prev); err != nil {
		t.Fatalf(".prev was removed despite version mismatch: %v", err)
	}
}

// kill unused warning for io import if trim happens.
var _ = io.Discard
