package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// applyEnv applies the whitelist of NISTRU_* environment variables to cfg.
// Env beats every file-level setting. Only variables in the whitelist are
// honored; anything else in the environment is ignored. Plugin-bound
// overrides are written into cfg.Plugins.EnvOverlay so PluginConfig can
// merge them over the file-derived JSON at lookup time.
func applyEnv(cfg *Config) []Warning {
	var warnings []Warning
	if cfg.Plugins.EnvOverlay == nil {
		cfg.Plugins.EnvOverlay = map[string]map[string]any{}
	}

	// autoupdate plugin — these must keep working after the plugin migrates
	// off os.Getenv onto PluginConfig. Empty strings count as "unset"
	// across the board: a shell that exports NISTRU_AUTOUPDATE_FOO= should
	// not override a file value with "".
	if v, ok := lookup("NISTRU_AUTOUPDATE_REPO"); ok && v != "" {
		pluginSet(cfg, "autoupdate", "repo", v)
	}
	if v, ok := lookup("NISTRU_AUTOUPDATE_CHANNEL"); ok && v != "" {
		pluginSet(cfg, "autoupdate", "channel", v)
	}
	if v, ok := lookup("NISTRU_AUTOUPDATE_INTERVAL"); ok && v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			warnings = append(warnings, Warning{
				Source:  "env",
				Message: fmt.Sprintf("NISTRU_AUTOUPDATE_INTERVAL=%q is not a valid duration: %v", v, err),
			})
		} else {
			pluginSet(cfg, "autoupdate", "interval", d.String())
		}
	}
	if v, ok := lookup("NISTRU_AUTOUPDATE_DISABLE"); ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes":
			pluginSet(cfg, "autoupdate", "disable", true)
		case "":
			// empty = unset; nothing to do
		case "0", "false", "no":
			pluginSet(cfg, "autoupdate", "disable", false)
		default:
			warnings = append(warnings, Warning{
				Source:  "env",
				Message: fmt.Sprintf("NISTRU_AUTOUPDATE_DISABLE=%q is not a recognized bool", v),
			})
		}
	}
	return warnings
}

// lookup is a thin wrapper around os.LookupEnv so env.go has exactly one
// point of contact with the environment — simpler to audit and stub.
func lookup(key string) (string, bool) {
	return os.LookupEnv(key)
}

// pluginSet stores a single field override into the plugin-env overlay.
func pluginSet(cfg *Config, name, key string, value any) {
	m, ok := cfg.Plugins.EnvOverlay[name]
	if !ok {
		m = map[string]any{}
		cfg.Plugins.EnvOverlay[name] = m
	}
	m[key] = value
}
