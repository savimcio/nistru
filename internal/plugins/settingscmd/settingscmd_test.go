package settingscmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/savimcio/nistru/internal/config"
	"github.com/savimcio/nistru/plugin"
)

// newTestPlugin builds a settingscmd plugin wired to an in-proc host so
// SetHost + Initialize land the same way they do in production. Returns
// the plugin + host so callers can invoke ExecuteCommand synchronously.
func newTestPlugin(t *testing.T, root string, cfg *config.Config) *Plugin {
	t.Helper()
	if cfg == nil {
		cfg = config.Defaults()
	}
	p := New(root, func() *config.Config { return cfg })
	reg := plugin.NewRegistry()
	reg.RegisterInProc(p)
	h := plugin.NewHost(reg)
	if err := h.Start(root); err != nil {
		t.Fatalf("host.Start: %v", err)
	}
	t.Cleanup(func() { _ = h.Shutdown(0) })
	// Initialize drives registerCommands via PostNotif.
	h.Emit(plugin.Initialize{RootPath: root})
	return p
}

// runCommand invokes the named command through the plugin's OnEvent
// ExecuteCommand branch and returns the collected effects. Skips the host
// dispatch layer so tests exercise the plugin in isolation.
func runCommand(p *Plugin, id string) []plugin.Effect {
	return p.OnEvent(plugin.ExecuteCommand{ID: id})
}

// TestOpenUser_CreatesSkeletonWhenMissing asserts the openUser command
// creates <UserConfigDir>/nistru/config.toml when it does not already
// exist, seeding it with the commented-defaults skeleton. The test
// redirects UserConfigDir via $XDG_CONFIG_HOME so nothing escapes to the
// real user config tree.
func TestOpenUser_CreatesSkeletonWhenMissing(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	// Belt-and-suspenders for non-Linux: Go's UserConfigDir honours
	// HOME/LocalAppData on other OSes. Point HOME at the same tmp so the
	// darwin path (~/Library/Application Support) still lands under tmp.
	t.Setenv("HOME", tmp)

	p := newTestPlugin(t, t.TempDir(), nil)

	effs := runCommand(p, cmdOpenUser)
	if len(effs) != 1 {
		t.Fatalf("openUser effects = %d, want 1: %+v", len(effs), effs)
	}
	of, ok := effs[0].(plugin.OpenFile)
	if !ok {
		t.Fatalf("effect[0] = %T, want plugin.OpenFile", effs[0])
	}

	// The file must now exist on disk.
	if _, err := os.Stat(of.Path); err != nil {
		t.Fatalf("openUser did not seed %s: %v", of.Path, err)
	}
	body, err := os.ReadFile(of.Path)
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	// The skeleton advertises every section; "tree_width" is a tight
	// marker (the ui section exists and is spelled correctly).
	if !strings.Contains(string(body), "tree_width") {
		t.Fatalf("skeleton missing tree_width; body=%q", body)
	}
}

// TestOpenUser_PreservesExistingFile guarantees the seeding logic never
// clobbers an existing file. Regression guard: a user who has already
// customised their config would lose it if this slipped.
func TestOpenUser_PreservesExistingFile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("HOME", tmp)

	// Seed the file with a sentinel before running the command.
	path, err := config.UserPath()
	if err != nil {
		t.Fatalf("UserPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	want := "# hand-written\ntree_width = 99\n"
	if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	p := newTestPlugin(t, t.TempDir(), nil)
	_ = runCommand(p, cmdOpenUser)

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	if string(got) != want {
		t.Fatalf("openUser overwrote existing file: got %q, want %q", got, want)
	}
}

// TestOpenProject_CreatesSkeletonWhenMissing mirrors the openUser test
// for the per-project path. No env manipulation needed — the path is
// computed from p.root.
func TestOpenProject_CreatesSkeletonWhenMissing(t *testing.T) {
	root := t.TempDir()
	p := newTestPlugin(t, root, nil)

	effs := runCommand(p, cmdOpenProject)
	if len(effs) != 1 {
		t.Fatalf("openProject effects = %d, want 1", len(effs))
	}
	of, ok := effs[0].(plugin.OpenFile)
	if !ok {
		t.Fatalf("effect[0] = %T, want plugin.OpenFile", effs[0])
	}
	if of.Path != config.ProjectPath(root) {
		t.Fatalf("openProject path = %q, want %q", of.Path, config.ProjectPath(root))
	}
	if _, err := os.Stat(of.Path); err != nil {
		t.Fatalf("project file not created: %v", err)
	}
}

// TestShowResolved_WritesMergedToml asserts showResolved serialises the
// merged config to <root>/.nistru/.resolved-config.toml and returns the
// corresponding OpenFile effect. The output must be valid TOML with the
// expected top-level sections.
func TestShowResolved_WritesMergedToml(t *testing.T) {
	root := t.TempDir()
	cfg := config.Defaults()
	p := newTestPlugin(t, root, cfg)

	effs := runCommand(p, cmdShowResolved)
	if len(effs) != 1 {
		t.Fatalf("showResolved effects = %d, want 1", len(effs))
	}
	of, ok := effs[0].(plugin.OpenFile)
	if !ok {
		t.Fatalf("effect[0] = %T, want plugin.OpenFile", effs[0])
	}
	wantPath := filepath.Join(root, ".nistru", ".resolved-config.toml")
	if of.Path != wantPath {
		t.Fatalf("showResolved path = %q, want %q", of.Path, wantPath)
	}
	body, err := os.ReadFile(of.Path)
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	// The merged TOML always has a [keymap] section because DefaultKeymap
	// is non-empty. Sanity-check on that.
	if !strings.Contains(string(body), "[keymap]") {
		t.Fatalf("resolved TOML missing [keymap]; body=%q", body)
	}
}

// TestReload_ReturnsReloadEffect asserts the reload command returns
// plugin.ReloadConfigRequest so the Model knows to reparse configuration.
// A companion Notify is also returned; both must be present.
func TestReload_ReturnsReloadEffect(t *testing.T) {
	p := newTestPlugin(t, t.TempDir(), nil)

	effs := runCommand(p, cmdReload)
	if len(effs) != 2 {
		t.Fatalf("reload effects = %d, want 2: %+v", len(effs), effs)
	}
	if _, ok := effs[0].(plugin.ReloadConfigRequest); !ok {
		t.Fatalf("effect[0] = %T, want plugin.ReloadConfigRequest", effs[0])
	}
	n, ok := effs[1].(plugin.Notify)
	if !ok {
		t.Fatalf("effect[1] = %T, want plugin.Notify", effs[1])
	}
	if n.Message == "" {
		t.Fatalf("reload Notify message empty")
	}
}

// TestUnknownCommand_ReturnsNoEffects guards against accidental default
// behaviour — unknown IDs must be a clean no-op rather than returning a
// stale effect from the last match.
func TestUnknownCommand_ReturnsNoEffects(t *testing.T) {
	p := newTestPlugin(t, t.TempDir(), nil)

	if effs := runCommand(p, "nistru.settings.doesNotExist"); len(effs) != 0 {
		t.Fatalf("unknown command produced effects: %+v", effs)
	}
}
