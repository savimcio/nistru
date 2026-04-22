package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// UserPath returns the canonical per-user config path:
// <UserConfigDir>/nistru/config.toml. No directories are created. Mirrors
// the style of plugin.userPluginsRoot / autoupdate.StatePath.
func UserPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("config: user config dir: %w", err)
	}
	return filepath.Join(dir, "nistru", "config.toml"), nil
}

// ProjectPath returns <root>/.nistru/config.toml. It does not check for
// existence — callers decide what to do when the file is absent.
func ProjectPath(root string) string {
	return filepath.Join(root, ".nistru", "config.toml")
}
