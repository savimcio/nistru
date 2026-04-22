package editor

import (
	"testing"
	"time"
)

// TestNewModelEmitsInitializeAndRegistersAutoupdateCommands is the regression
// guard for the "NewModel never emitted Initialize in production" bug: the
// editor core constructed the plugin host and Start()ed it, but nothing fired
// an onStart-matching event, so in-proc plugins that register palette commands
// during Initialize (like autoupdate) never saw the handler run. The bug hid
// for weeks because every existing test manually called
// host.Emit(plugin.Initialize{...}) after construction, masking the gap.
//
// The assertion below constructs a production Model via NewModel and checks
// that autoupdate's five palette commands landed on m.commands. Without the
// Emit(Initialize) call in newModelWithRegistry, this test fails with an empty
// commands map.
//
// NISTRU_AUTOUPDATE_DISABLE=1 is set so the background checker goroutine and
// the real GitHub poll don't fire during the test — command registration
// happens before the disable short-circuit (see autoupdate.handleInitialize)
// so this does NOT mask the bug we're guarding against.
func TestNewModelEmitsInitializeAndRegistersAutoupdateCommands(t *testing.T) {
	t.Setenv("NISTRU_AUTOUPDATE_DISABLE", "1")
	// Clear any ambient overrides from the developer shell so the test is
	// hermetic. The DISABLE above is the only knob we want active.
	t.Setenv("NISTRU_AUTOUPDATE_REPO", "")
	t.Setenv("NISTRU_AUTOUPDATE_CHANNEL", "")
	t.Setenv("NISTRU_AUTOUPDATE_INTERVAL", "")

	root := t.TempDir()

	m, err := NewModel(root, nil)
	if err != nil {
		t.Fatalf("NewModel(%q): %v", root, err)
	}
	t.Cleanup(func() { _ = m.host.Shutdown(100 * time.Millisecond) })

	wantIDs := []string{
		"autoupdate:check",
		"autoupdate:install",
		"autoupdate:rollback",
		"autoupdate:switch-channel",
		"autoupdate:release-notes",
	}
	for _, id := range wantIDs {
		ref, ok := m.commands[id]
		if !ok {
			t.Errorf("m.commands missing %q; got keys: %v", id, keysOf(m.commands))
			continue
		}
		if ref.Plugin != "autoupdate" {
			t.Errorf("command %q: Plugin=%q, want %q", id, ref.Plugin, "autoupdate")
		}
	}

	// Spot-check a non-empty Title on at least one entry — an earlier failure
	// mode had commands register with empty titles, which broke palette fuzzy
	// matching. `autoupdate:check` is the canonical smoke-test target.
	if ref, ok := m.commands["autoupdate:check"]; ok {
		if ref.Title == "" {
			t.Errorf("autoupdate:check has empty Title; want non-empty")
		}
	}
}

// keysOf returns a slice of keys from m for failure-message pretty-printing.
// Order is not stable (Go map iteration) but the test only uses it inside
// t.Errorf, so readability > determinism.
func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
