package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUserPath(t *testing.T) {
	p, err := UserPath()
	if err != nil {
		// On hosts without a UserConfigDir this is expected. Nothing more to check.
		return
	}
	want, werr := os.UserConfigDir()
	if werr != nil {
		t.Fatalf("UserConfigDir: %v", werr)
	}
	if !strings.HasPrefix(p, want) {
		t.Errorf("UserPath %q should live under %q", p, want)
	}
	if filepath.Base(p) != "config.toml" {
		t.Errorf("UserPath base = %q, want config.toml", filepath.Base(p))
	}
	if filepath.Base(filepath.Dir(p)) != "nistru" {
		t.Errorf("UserPath dir = %q, want .../nistru/config.toml", p)
	}
}

func TestProjectPath(t *testing.T) {
	root := t.TempDir()
	p := ProjectPath(root)
	want := filepath.Join(root, ".nistru", "config.toml")
	if p != want {
		t.Errorf("ProjectPath = %q, want %q", p, want)
	}
}
