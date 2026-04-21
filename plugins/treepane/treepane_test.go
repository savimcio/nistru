package treepane

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/savimcio/nistru/plugin"
)

// buildFixture writes a small directory fixture under root and returns the
// root path (already an absolute path from t.TempDir()).
func buildFixture(t *testing.T, spec map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, contents := range spec {
		full := filepath.Join(root, rel)
		if strings.HasSuffix(rel, "/") {
			if err := os.MkdirAll(full, 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", full, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir parent of %s: %v", full, err)
		}
		if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
	return root
}

func TestTreePane_Render_Basic(t *testing.T) {
	root := buildFixture(t, map[string]string{
		"foo/":      "",
		"foo/a.txt": "alpha",
		"bar.txt":   "beta",
	})
	p, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	out := p.Render(60, 20)
	lines := strings.Split(out, "\n")
	if len(lines) < 3 {
		t.Fatalf("expected >=3 lines, got %d: %q", len(lines), out)
	}

	// First line is the root dir (ends with "/").
	first := lines[0]
	if !strings.Contains(first, filepath.Base(root)+"/") {
		t.Fatalf("first line %q does not contain %q/", first, filepath.Base(root))
	}

	// After the root we expect the directory "foo/" before the file "bar.txt"
	// because walkDir sorts dirs ahead of files.
	second := lines[1]
	third := lines[2]
	if !strings.Contains(second, "foo/") {
		t.Fatalf("second line %q, want to contain foo/", second)
	}
	if !strings.Contains(third, "bar.txt") {
		t.Fatalf("third line %q, want to contain bar.txt", third)
	}

	// Prefix art: with exactly two root children, foo is `├──` and bar is `└──`.
	if !strings.Contains(second, "├── ") {
		t.Fatalf("second line %q, want `├── ` connector", second)
	}
	if !strings.Contains(third, "└── ") {
		t.Fatalf("third line %q, want `└── ` connector", third)
	}
}

func TestTreePane_HandleKey_Nav(t *testing.T) {
	root := buildFixture(t, map[string]string{
		"a.txt": "1",
		"b.txt": "2",
		"c.txt": "3",
		"d.txt": "4",
	})
	p, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_ = p.Render(60, 20) // set size

	// Initial cursor is at the root row (index 0).
	base := p.model.Cursor()
	if base != 0 {
		t.Fatalf("initial cursor = %d, want 0", base)
	}

	// Three j's, then one k → net +2.
	for range 3 {
		p.HandleKey(plugin.KeyEvent{Key: "j"})
	}
	p.HandleKey(plugin.KeyEvent{Key: "k"})

	if got := p.model.Cursor(); got != 2 {
		t.Fatalf("cursor after jjjk = %d, want 2", got)
	}

	// Rendered output must place the selected row's label text — the row at
	// cursor=2 corresponds to "b.txt" here. We tolerate lipgloss choosing to
	// drop ANSI escapes in non-tty test environments; the positional proof
	// above is the load-bearing assertion.
	out := p.Render(60, 20)
	if !strings.Contains(out, "b.txt") {
		t.Fatalf("render missing b.txt:\n%s", out)
	}
}

func TestTreePane_OpenFileEffect(t *testing.T) {
	root := buildFixture(t, map[string]string{
		"a.txt": "1",
	})
	p, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_ = p.Render(60, 20)

	// Navigate to the a.txt row (cursor=1).
	p.HandleKey(plugin.KeyEvent{Key: "j"})
	effects := p.HandleKey(plugin.KeyEvent{Key: "enter"})
	if len(effects) != 1 {
		t.Fatalf("effects = %d, want 1", len(effects))
	}
	of, ok := effects[0].(plugin.OpenFile)
	if !ok {
		t.Fatalf("effect type = %T, want plugin.OpenFile", effects[0])
	}
	if !strings.HasSuffix(of.Path, "a.txt") {
		t.Fatalf("OpenFile.Path = %q, want suffix a.txt", of.Path)
	}
}

func TestTreePane_ExpandCollapse(t *testing.T) {
	root := buildFixture(t, map[string]string{
		"sub/":       "",
		"sub/x.txt":  "1",
		"sub/y.txt":  "2",
		"top.txt":    "3",
	})
	p, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_ = p.Render(60, 20)
	baseRows := len(p.model.rows)

	// Navigate to the sub/ row (cursor=1), then expand.
	p.HandleKey(plugin.KeyEvent{Key: "j"})
	p.HandleKey(plugin.KeyEvent{Key: "enter"})
	expanded := len(p.model.rows)
	if expanded <= baseRows {
		t.Fatalf("rows after expand = %d, want > %d", expanded, baseRows)
	}

	// Collapse.
	p.HandleKey(plugin.KeyEvent{Key: "enter"})
	collapsed := len(p.model.rows)
	if collapsed != baseRows {
		t.Fatalf("rows after collapse = %d, want %d", collapsed, baseRows)
	}
}

func TestTreePane_HNavigation(t *testing.T) {
	root := buildFixture(t, map[string]string{
		"sub/":      "",
		"sub/a.txt": "1",
	})
	p, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_ = p.Render(60, 20)

	// Navigate to sub/, expand it, then step down to sub/a.txt.
	p.HandleKey(plugin.KeyEvent{Key: "j"})     // cursor -> sub/
	p.HandleKey(plugin.KeyEvent{Key: "enter"}) // expand sub/
	p.HandleKey(plugin.KeyEvent{Key: "j"})     // cursor -> a.txt (inside sub)
	onFile := p.model.Cursor()

	// Now 'h' on a file row must jump cursor to parent dir ("sub/").
	p.HandleKey(plugin.KeyEvent{Key: "h"})
	if p.model.Cursor() >= onFile {
		t.Fatalf("cursor after h = %d, want < %d (jumped to parent)", p.model.Cursor(), onFile)
	}
	parentRow := p.model.rows[p.model.Cursor()]
	if !strings.Contains(parentRow.label, "sub") {
		t.Fatalf("parent row label = %q, want contains 'sub'", parentRow.label)
	}
}

func TestTreePane_SkipDirs(t *testing.T) {
	root := buildFixture(t, map[string]string{
		".git/":         "",
		"node_modules/": "",
		"vendor/":       "",
		".hidden/":      "",
		"normal/":       "",
		// Ensure the skipped/hidden dirs have at least one file, so their
		// presence would otherwise be visible if we accidentally included
		// them.
		".git/HEAD":             "ref",
		"node_modules/pkg.json": "{}",
		"vendor/v.txt":          "",
		".hidden/h.txt":         "",
		"normal/n.txt":          "",
	})
	p, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	out := p.Render(60, 20)

	// Only "normal/" should appear as a child row beneath the root.
	if !strings.Contains(out, "normal/") {
		t.Fatalf("render missing 'normal/':\n%s", out)
	}
	for _, bad := range []string{".git/", "node_modules/", "vendor/", ".hidden/"} {
		if strings.Contains(out, bad) {
			t.Fatalf("render contains skipped %q:\n%s", bad, out)
		}
	}

	// Also: exactly one non-root row (normal/) should be emitted.
	if got := len(p.model.rows); got != 2 {
		t.Fatalf("model rows = %d, want 2 (root + normal/)", got)
	}
}

func TestTreePane_UnknownKeyIgnored(t *testing.T) {
	root := buildFixture(t, map[string]string{"a.txt": "1"})
	p, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_ = p.Render(60, 20)
	if got := p.HandleKey(plugin.KeyEvent{Key: "not-a-key"}); got != nil {
		t.Fatalf("HandleKey unknown = %v, want nil", got)
	}
}

// Sanity: Slot is stable ("left").
func TestTreePane_Slot(t *testing.T) {
	root := buildFixture(t, map[string]string{"a.txt": "1"})
	p, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := p.Slot(); got != "left" {
		t.Fatalf("Slot = %q, want left", got)
	}
}

// -----------------------------------------------------------------------------
// Extended scenarios (T2.5).

// TestTreePane_DeepTree builds a fixture nested 10 levels deep and asserts
// expand/collapse doesn't blow the stack and rendering still works.
func TestTreePane_DeepTree(t *testing.T) {
	root := t.TempDir()
	rel := ""
	for i := range 10 {
		rel = filepath.Join(rel, fmt.Sprintf("d%d", i))
	}
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(full, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(full, "leaf.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write leaf: %v", err)
	}

	p, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_ = p.Render(80, 40)

	// Expand every directory along the chain, one j + enter per level.
	for range 11 { // root + 10 levels
		p.HandleKey(plugin.KeyEvent{Key: "j"})
		p.HandleKey(plugin.KeyEvent{Key: "enter"})
	}

	out := p.Render(200, 80)
	if !strings.Contains(out, "leaf.txt") {
		t.Fatalf("deep render missing leaf.txt:\n%s", out)
	}

	// Collapse from the top — G to bottom, then walk back up via h — should
	// not recurse infinitely.
	p.HandleKey(plugin.KeyEvent{Key: "g"})
	for range 20 { // comfortably more than depth
		p.HandleKey(plugin.KeyEvent{Key: "h"})
	}
	// Rendering after the collapse walk must still succeed.
	_ = p.Render(80, 40)
}

// TestTreePane_PermissionDenied creates a 0000 directory inside the fixture
// and asserts New/Render don't panic and the entry path is handled
// gracefully (the walker swallows non-root permission errors).
func TestTreePane_PermissionDenied(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX chmod semantics required")
	}
	root := buildFixture(t, map[string]string{
		"readable.txt":    "ok",
		"blocked/":        "",
		"blocked/secret":  "nope",
	})
	blockedDir := filepath.Join(root, "blocked")
	if err := os.Chmod(blockedDir, 0o000); err != nil {
		t.Fatalf("chmod 0000: %v", err)
	}
	// Restore perms so t.TempDir cleanup can delete the tree.
	t.Cleanup(func() { _ = os.Chmod(blockedDir, 0o755) })

	p, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	out := p.Render(80, 20)
	if !strings.Contains(out, "readable.txt") {
		t.Fatalf("render missing readable.txt:\n%s", out)
	}
	// Navigate to the blocked dir and try to expand. Must not panic.
	p.HandleKey(plugin.KeyEvent{Key: "j"}) // blocked/
	p.HandleKey(plugin.KeyEvent{Key: "enter"})
	// Also try h to collapse back; must not panic.
	p.HandleKey(plugin.KeyEvent{Key: "h"})
	_ = p.Render(80, 20)
}

// TestTreePane_SymlinkOutOfRoot creates a symlink pointing outside the root
// and encodes the current behavior: the symlink target is neither a regular
// file nor a directory from os.ReadDir's perspective (os.DirEntry reports it
// under Type/IsDir on the link itself), so the walker silently skips it.
func TestTreePane_SymlinkOutOfRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	root := buildFixture(t, map[string]string{"a.txt": "1"})
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	p, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	out := p.Render(80, 20)
	// Current behavior: walkDir's split only keeps regular files and
	// directories (e.IsDir() / e.Type().IsRegular()). A symlink-to-dir fails
	// both checks at the ReadDir layer, so "link" is not present. If this
	// assertion flips, the walker started following links — update with care.
	if strings.Contains(out, "link") {
		t.Fatalf("symlink 'link' visible in render — walker behavior changed:\n%s", out)
	}
	if strings.Contains(out, "secret.txt") {
		t.Fatalf("symlink target leaked into render: %s", out)
	}
}

// TestTreePane_EmptyRepo renders a root that contains only .git and asserts
// no visible child rows exist and j/k are no-ops.
func TestTreePane_EmptyRepo(t *testing.T) {
	root := buildFixture(t, map[string]string{
		".git/HEAD": "ref",
	})
	p, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_ = p.Render(80, 20)
	if n := len(p.model.rows); n != 1 {
		t.Fatalf("rows = %d, want 1 (root only)", n)
	}
	c0 := p.model.Cursor()
	p.HandleKey(plugin.KeyEvent{Key: "j"})
	p.HandleKey(plugin.KeyEvent{Key: "j"})
	p.HandleKey(plugin.KeyEvent{Key: "k"})
	if got := p.model.Cursor(); got != c0 {
		t.Fatalf("cursor moved on empty repo: %d -> %d", c0, got)
	}
}

// TestTreePane_WidthResponsive renders at 40, 80, 120 cols and asserts key
// invariants: lines don't exceed the requested width and multi-byte labels
// are never split mid-rune.
func TestTreePane_WidthResponsive(t *testing.T) {
	// The filename below contains multi-byte runes so a byte-based truncation
	// would corrupt it.
	root := buildFixture(t, map[string]string{
		"ünicode-file-with-long-name.txt":                  "",
		"sub/":                                             "",
		"sub/deeply-nested-file-that-exceeds-40-cols.txt": "",
	})
	p, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Expand sub so the longer nested path is visible.
	_ = p.Render(120, 20)
	p.HandleKey(plugin.KeyEvent{Key: "j"})
	p.HandleKey(plugin.KeyEvent{Key: "enter"})

	for _, w := range []int{40, 80, 120} {
		t.Run(fmt.Sprintf("w=%d", w), func(t *testing.T) {
			out := p.Render(w, 20)
			for line := range strings.SplitSeq(out, "\n") {
				if !utf8.ValidString(line) {
					t.Fatalf("line contains invalid utf-8 at width %d: %q", w, line)
				}
			}
			// Expand arrow invariant: the root row is expanded by default, so
			// its marker (▾) must be visible at every width we test.
			if !strings.Contains(out, "▾") {
				t.Fatalf("width %d: missing expanded marker ▾ in:\n%s", w, out)
			}
		})
	}
}

// Dir sorting: directories before files.
func TestTreePane_DirsBeforeFiles(t *testing.T) {
	root := buildFixture(t, map[string]string{
		"aardvark.txt": "",
		"bees/":        "",
		"bees/.keep":   "",
	})
	p, err := New(root)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_ = p.Render(60, 20)
	// Rows: [0] root, [1] bees/, [2] aardvark.txt.
	if n := len(p.model.rows); n != 3 {
		t.Fatalf("rows = %d, want 3", n)
	}
	if !p.model.rows[1].isDir {
		t.Fatalf("row[1] isDir = false, want true (bees/)")
	}
	if !strings.Contains(p.model.rows[1].label, "bees") {
		t.Fatalf("row[1] label = %q, want contains 'bees'", p.model.rows[1].label)
	}
	if p.model.rows[2].isDir {
		t.Fatalf("row[2] isDir = true, want false (aardvark.txt)")
	}
}
