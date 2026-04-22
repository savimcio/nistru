package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setupIsolatedHome points os.UserConfigDir() at a fresh temp dir by
// rewriting every env var UserConfigDir consults (Linux uses
// XDG_CONFIG_HOME, macOS uses HOME, Windows uses APPDATA). Returns the
// resolved user config path so tests can write fixture files to the
// exact place Load() will look. Also clears every NISTRU_AUTOUPDATE_*
// env var so stale values in the caller's shell don't leak in.
func setupIsolatedHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("HOME", home)
	t.Setenv("AppData", home)
	for _, k := range []string{
		"NISTRU_AUTOUPDATE_REPO",
		"NISTRU_AUTOUPDATE_CHANNEL",
		"NISTRU_AUTOUPDATE_INTERVAL",
		"NISTRU_AUTOUPDATE_DISABLE",
	} {
		unsetEnv(t, k)
	}
	userPath, err := UserPath()
	if err != nil {
		t.Fatalf("UserPath after isolation: %v", err)
	}
	return userPath
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestLoadDefaultsOnly(t *testing.T) {
	_ = setupIsolatedHome(t)
	projRoot := t.TempDir()
	c, warnings, err := Load(projRoot)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %+v", warnings)
	}
	if c.UI.TreeWidth != 30 {
		t.Errorf("TreeWidth = %d", c.UI.TreeWidth)
	}
}

func TestLoadUserOverridesDefaults(t *testing.T) {
	userCfg := setupIsolatedHome(t)
	projRoot := t.TempDir()

	writeFile(t, userCfg, `
[ui]
tree_width = 99
`)
	c, warnings, err := Load(projRoot)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings: %+v", warnings)
	}
	if c.UI.TreeWidth != 99 {
		t.Errorf("TreeWidth = %d, want 99", c.UI.TreeWidth)
	}
	// Untouched defaults survive.
	if c.UI.SavedFadeAfter != 3*time.Second {
		t.Errorf("SavedFadeAfter = %v", c.UI.SavedFadeAfter)
	}
}

func TestLoadProjectOverridesUser(t *testing.T) {
	userCfg := setupIsolatedHome(t)
	projRoot := t.TempDir()

	writeFile(t, userCfg, `
[ui]
tree_width = 10
saved_fade_after = "1s"
`)
	writeFile(t, ProjectPath(projRoot), `
[ui]
tree_width = 20
`)
	c, warnings, err := Load(projRoot)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings: %+v", warnings)
	}
	if c.UI.TreeWidth != 20 {
		t.Errorf("project didn't override user: TreeWidth = %d", c.UI.TreeWidth)
	}
	// Keys the project file doesn't touch come from the user file.
	if c.UI.SavedFadeAfter != time.Second {
		t.Errorf("SavedFadeAfter = %v, want 1s", c.UI.SavedFadeAfter)
	}
}

func TestLoadEnvOverridesProject(t *testing.T) {
	_ = setupIsolatedHome(t)
	projRoot := t.TempDir()

	writeFile(t, ProjectPath(projRoot), `
[plugins.autoupdate]
repo = "fromfile/proj"
channel = "release"
`)
	t.Setenv("NISTRU_AUTOUPDATE_REPO", "fromenv/override")
	t.Setenv("NISTRU_AUTOUPDATE_DISABLE", "true")

	c, _, err := Load(projRoot)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	raw := c.PluginConfig("autoupdate")
	if raw == nil {
		t.Fatal("PluginConfig is nil")
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["repo"] != "fromenv/override" {
		t.Errorf("env didn't beat file: repo=%v", got["repo"])
	}
	if got["disable"] != true {
		t.Errorf("env bool missing: disable=%v", got["disable"])
	}
	// Unrelated file keys survive.
	if got["channel"] != "release" {
		t.Errorf("file channel lost: channel=%v", got["channel"])
	}
}

func TestLoadMalformedTOMLIsWarning(t *testing.T) {
	_ = setupIsolatedHome(t)
	projRoot := t.TempDir()

	writeFile(t, ProjectPath(projRoot), `this is = not = valid toml`)
	c, warnings, err := Load(projRoot)
	if err != nil {
		t.Fatalf("Load error (should have been a warning): %v", err)
	}
	if c == nil {
		t.Fatal("cfg was nil despite malformed project file")
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w.Message, "skipped") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected malformed-file warning; got %+v", warnings)
	}
	// Defaults still in force.
	if c.UI.TreeWidth != 30 {
		t.Errorf("TreeWidth = %d", c.UI.TreeWidth)
	}
}

// TestPluginConfig_MergesUserAndProject is the bug fix: when each file
// sets a different key under the same plugin, both must survive the merge.
// The old Primitive-based design clobbered cfg.Plugins.Raw[name] from the
// later file, losing the earlier file's keys silently.
func TestPluginConfig_MergesUserAndProject(t *testing.T) {
	userCfg := setupIsolatedHome(t)
	projRoot := t.TempDir()

	writeFile(t, userCfg, `
[plugins.autoupdate]
repo = "u/r"
`)
	writeFile(t, ProjectPath(projRoot), `
[plugins.autoupdate]
interval = "2h"
`)
	c, _, err := Load(projRoot)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	raw := c.PluginConfig("autoupdate")
	if raw == nil {
		t.Fatal("PluginConfig is nil after cross-file merge")
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["repo"] != "u/r" {
		t.Errorf("user-only key lost: repo=%v (full=%v)", got["repo"], got)
	}
	if got["interval"] != "2h" {
		t.Errorf("project-only key lost: interval=%v (full=%v)", got["interval"], got)
	}
}

func TestPluginConfig_ProjectOverridesUser_PerKey(t *testing.T) {
	userCfg := setupIsolatedHome(t)
	projRoot := t.TempDir()

	writeFile(t, userCfg, `
[plugins.autoupdate]
repo = "user/wins"
channel = "release"
`)
	writeFile(t, ProjectPath(projRoot), `
[plugins.autoupdate]
repo = "proj/wins"
`)
	c, _, err := Load(projRoot)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(c.PluginConfig("autoupdate"), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["repo"] != "proj/wins" {
		t.Errorf("project should win on shared key: repo=%v", got["repo"])
	}
	if got["channel"] != "release" {
		t.Errorf("user-only key lost: channel=%v", got["channel"])
	}
}

func TestPluginConfig_ArrayReplacesWholesale(t *testing.T) {
	userCfg := setupIsolatedHome(t)
	projRoot := t.TempDir()

	writeFile(t, userCfg, `
[plugins.treepane]
skip_dirs = ["a"]
`)
	writeFile(t, ProjectPath(projRoot), `
[plugins.treepane]
skip_dirs = ["b"]
`)
	c, _, err := Load(projRoot)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(c.PluginConfig("treepane"), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	dirs, ok := got["skip_dirs"].([]any)
	if !ok {
		t.Fatalf("skip_dirs not a slice: %T", got["skip_dirs"])
	}
	if len(dirs) != 1 || dirs[0] != "b" {
		t.Errorf("array should replace wholesale, got %v", dirs)
	}
}

// TestPluginConfig_PluginOnlyInUser is the regression guard for the
// MetaData-mismatch bug: when a plugin appears only in the user file,
// later files (project) used to install their own MetaData onto
// cfg.Plugins.MD, breaking PrimitiveDecode for the user-file primitive.
func TestPluginConfig_PluginOnlyInUser(t *testing.T) {
	userCfg := setupIsolatedHome(t)
	projRoot := t.TempDir()

	writeFile(t, userCfg, `
[plugins.autoupdate]
repo = "u/only"
`)
	// Project file exists and parses cleanly but does not mention autoupdate.
	writeFile(t, ProjectPath(projRoot), `
[ui]
tree_width = 42
`)
	c, _, err := Load(projRoot)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	raw := c.PluginConfig("autoupdate")
	if raw == nil {
		t.Fatal("user-only plugin lost when project file present")
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["repo"] != "u/only" {
		t.Errorf("repo = %v", got["repo"])
	}
}

func TestPluginConfig_PluginOnlyInProject(t *testing.T) {
	userCfg := setupIsolatedHome(t)
	projRoot := t.TempDir()

	writeFile(t, userCfg, `
[ui]
tree_width = 42
`)
	writeFile(t, ProjectPath(projRoot), `
[plugins.autoupdate]
repo = "p/only"
`)
	c, _, err := Load(projRoot)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	raw := c.PluginConfig("autoupdate")
	if raw == nil {
		t.Fatal("project-only plugin lost when user file present")
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["repo"] != "p/only" {
		t.Errorf("repo = %v", got["repo"])
	}
}

func TestPluginConfig_EnvOverlayWinsOverFiles(t *testing.T) {
	userCfg := setupIsolatedHome(t)
	projRoot := t.TempDir()

	writeFile(t, userCfg, `
[plugins.autoupdate]
repo = "u/file"
`)
	t.Setenv("NISTRU_AUTOUPDATE_REPO", "env/wins")

	c, _, err := Load(projRoot)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(c.PluginConfig("autoupdate"), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["repo"] != "env/wins" {
		t.Errorf("env should win over file: repo=%v", got["repo"])
	}
}

func TestLoadPluginSubTable(t *testing.T) {
	_ = setupIsolatedHome(t)
	projRoot := t.TempDir()

	writeFile(t, ProjectPath(projRoot), `
[plugins.treepane]
skip_dirs = [".git", "dist"]
`)
	c, _, err := Load(projRoot)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	raw := c.PluginConfig("treepane")
	if raw == nil {
		t.Fatal("treepane plugin config was nil")
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	dirs, ok := got["skip_dirs"].([]any)
	if !ok {
		t.Fatalf("skip_dirs not a slice: %T", got["skip_dirs"])
	}
	if len(dirs) != 2 || dirs[0] != ".git" || dirs[1] != "dist" {
		t.Errorf("skip_dirs = %v", dirs)
	}
}
