package config

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

func TestDefaultsFullyPopulated(t *testing.T) {
	c := Defaults()
	if c == nil {
		t.Fatal("Defaults() returned nil")
	}
	if c.Editor.MaxFileSize != 1<<20 {
		t.Errorf("Editor.MaxFileSize = %d, want %d", c.Editor.MaxFileSize, 1<<20)
	}
	if c.UI.TreeWidth != 30 {
		t.Errorf("UI.TreeWidth = %d, want 30", c.UI.TreeWidth)
	}
	if c.UI.SavedFadeAfter != 3*time.Second {
		t.Errorf("UI.SavedFadeAfter = %v, want 3s", c.UI.SavedFadeAfter)
	}
	if !c.UI.RelativeNumbers {
		t.Error("UI.RelativeNumbers should default true")
	}
	if c.Autosave.SaveDebounce != 250*time.Millisecond {
		t.Errorf("Autosave.SaveDebounce = %v, want 250ms", c.Autosave.SaveDebounce)
	}
	if c.Autosave.ChangeDebounce != 50*time.Millisecond {
		t.Errorf("Autosave.ChangeDebounce = %v, want 50ms", c.Autosave.ChangeDebounce)
	}
	if len(c.Keymap) != len(knownActions) {
		t.Errorf("Keymap has %d entries, want %d", len(c.Keymap), len(knownActions))
	}
	for _, a := range knownActions {
		if v, ok := c.Keymap[a]; !ok || v == "" {
			t.Errorf("Keymap[%q] missing or empty", a)
		}
	}
	if c.Plugins.Merged == nil {
		t.Error("Plugins.Merged should be non-nil on defaults")
	}
	if c.Plugins.EnvOverlay == nil {
		t.Error("Plugins.EnvOverlay should be non-nil on defaults")
	}
}

func TestSizeUnmarshalText(t *testing.T) {
	cases := []struct {
		in    string
		want  Size
		isErr bool
	}{
		{"0", 0, false},
		{"1024", 1024, false},
		{"1KiB", 1024, false},
		{"1kib", 1024, false},
		{"1MiB", 1 << 20, false},
		{"2MiB", 2 << 20, false},
		{"1GiB", 1 << 30, false},
		{"1KB", 1_000, false},
		{"2MB", 2_000_000, false},
		{"1GB", 1_000_000_000, false},
		{"512B", 512, false},
		{"", 0, true},
		{"abc", 0, true},
		{"1XB", 0, true},
		{"MiB", 0, true},
		{"-1MiB", 0, true},
		// Overflow: n * mult would wrap uint64. Must error instead of
		// silently producing a tiny byte count. math.MaxUint64 is
		// 18446744073709551615; multiplying any of those by >=2 overflows.
		{"18446744073709551615KiB", 0, true},
		{"99999999999999999GiB", 0, true},
	}
	for _, tc := range cases {
		var s Size
		err := s.UnmarshalText([]byte(tc.in))
		if tc.isErr {
			if err == nil {
				t.Errorf("Size(%q) expected error, got %d", tc.in, s)
			}
			continue
		}
		if err != nil {
			t.Errorf("Size(%q) unexpected err: %v", tc.in, err)
			continue
		}
		if s != tc.want {
			t.Errorf("Size(%q) = %d, want %d", tc.in, uint64(s), uint64(tc.want))
		}
	}
}

func TestPluginConfigRoundTrip(t *testing.T) {
	c := Defaults()
	c.Plugins.Merged["autoupdate"] = map[string]any{
		"repo":     "savimcio/nistru",
		"channel":  "release",
		"interval": "1h",
		"disable":  false,
	}

	raw := c.PluginConfig("autoupdate")
	if raw == nil {
		t.Fatal("PluginConfig returned nil for known plugin")
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["repo"] != "savimcio/nistru" {
		t.Errorf("repo = %v", got["repo"])
	}
	if got["channel"] != "release" {
		t.Errorf("channel = %v", got["channel"])
	}
	if got["disable"] != false {
		t.Errorf("disable = %v", got["disable"])
	}

	// Unknown plugin returns nil.
	if c.PluginConfig("nope") != nil {
		t.Errorf("PluginConfig(nope) should be nil")
	}
}

func TestPluginConfigMergesEnvOverlay(t *testing.T) {
	c := Defaults()
	c.Plugins.Merged["autoupdate"] = map[string]any{
		"repo": "savimcio/nistru",
	}
	c.Plugins.EnvOverlay["autoupdate"] = map[string]any{
		"repo":    "override/repo",
		"disable": true,
	}

	raw := c.PluginConfig("autoupdate")
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["repo"] != "override/repo" {
		t.Errorf("overlay didn't win: repo=%v", got["repo"])
	}
	if got["disable"] != true {
		t.Errorf("overlay key missing: disable=%v", got["disable"])
	}
}

func TestPluginConfigOverlayOnlyNoFile(t *testing.T) {
	c := Defaults()
	c.Plugins.EnvOverlay["autoupdate"] = map[string]any{"repo": "x/y"}
	raw := c.PluginConfig("autoupdate")
	if raw == nil {
		t.Fatal("overlay-only should still yield JSON")
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["repo"] != "x/y" {
		t.Errorf("got %v", got)
	}
}

func TestEncodeResolvedParses(t *testing.T) {
	c := Defaults()
	b, err := c.EncodeResolved()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("empty encode")
	}
	// Round-trip: parse back into a fresh struct that mirrors the schema.
	var back struct {
		Editor   Editor
		UI       UI
		Autosave Autosave
		Keymap   map[string]string
	}
	if _, err := toml.Decode(string(b), &back); err != nil {
		t.Fatalf("reparse: %v\nOUTPUT:\n%s", err, b)
	}
	if back.UI.TreeWidth != 30 {
		t.Errorf("round-tripped TreeWidth = %d", back.UI.TreeWidth)
	}
}

// TestEncodeResolvedIncludesPlugins guards that EncodeResolved emits the
// merged + env-overlaid plugins section so `showResolved` doesn't lie.
func TestEncodeResolvedIncludesPlugins(t *testing.T) {
	c := Defaults()
	c.Plugins.Merged["autoupdate"] = map[string]any{
		"repo":    "u/r",
		"channel": "release",
	}
	c.Plugins.EnvOverlay["autoupdate"] = map[string]any{
		"repo": "env/wins",
	}
	b, err := c.EncodeResolved()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var back struct {
		Plugins map[string]map[string]any `toml:"plugins"`
	}
	if _, err := toml.Decode(string(b), &back); err != nil {
		t.Fatalf("reparse: %v\nOUTPUT:\n%s", err, b)
	}
	au, ok := back.Plugins["autoupdate"]
	if !ok {
		t.Fatalf("autoupdate missing from resolved plugins: %s", b)
	}
	if au["repo"] != "env/wins" {
		t.Errorf("env didn't win in EncodeResolved: %v", au["repo"])
	}
	if au["channel"] != "release" {
		t.Errorf("file key lost in EncodeResolved: %v", au["channel"])
	}
}
