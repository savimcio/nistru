package config

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestCommentedDefaultsParsesCleanly(t *testing.T) {
	s := CommentedDefaults()
	if s == "" {
		t.Fatal("empty skeleton")
	}
	// toml.Decode should accept a file that is entirely comments + blanks.
	var dst map[string]any
	if _, err := toml.Decode(s, &dst); err != nil {
		t.Fatalf("skeleton didn't parse: %v", err)
	}
	// Because every non-header line is commented out, decoding yields an
	// empty map.
	if len(dst) != 0 {
		t.Errorf("skeleton should decode to empty map, got %v", dst)
	}
}

func TestCommentedDefaultsEveryLineIsCommentOrBlank(t *testing.T) {
	s := CommentedDefaults()
	for i, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		t.Errorf("line %d is neither blank nor commented: %q", i+1, line)
	}
}

func TestCommentedDefaultsContainsKeyMarkers(t *testing.T) {
	s := CommentedDefaults()
	for _, want := range []string{
		"tree_width",
		"save_debounce",
		"[keymap]",
		"[plugins.autoupdate]",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("skeleton missing %q", want)
		}
	}
}
