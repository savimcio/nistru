package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/BurntSushi/toml"
)

// Warning is a non-fatal complaint surfaced to the user — a bad value in
// config, a malformed file on disk, an env var that couldn't be parsed.
// The editor stays up no matter how many warnings pile up; only
// truly-unrecoverable problems (e.g. a nil Config pointer) become errors.
type Warning struct {
	Source  string
	Message string
}

// Load resolves Nistru's configuration for the given project root.
// Precedence (lowest to highest): built-in defaults → per-user file →
// per-project file → NISTRU_* environment overrides. Missing files are
// not warnings; malformed files are warnings, and whatever state was
// parsed before the first syntax error is still applied (same discipline
// autoupdate.LoadState uses for its JSON state file). Hard errors are
// reserved for situations that leave us with no usable Config at all.
func Load(root string) (*Config, []Warning, error) {
	cfg := Defaults()
	var warnings []Warning

	// 1. User file.
	userPath, err := UserPath()
	if err != nil {
		warnings = append(warnings, Warning{Source: "paths", Message: err.Error()})
	} else if lerr := decodeInto(userPath, cfg); lerr != nil {
		warnings = append(warnings, Warning{
			Source:  userPath,
			Message: fmt.Sprintf("skipped: %v", lerr),
		})
	}

	// 2. Project file. decodeInto deep-merges plugin sub-tables on top of
	//    whatever the user file installed, key-by-key.
	projPath := ProjectPath(root)
	if lerr := decodeInto(projPath, cfg); lerr != nil {
		warnings = append(warnings, Warning{
			Source:  projPath,
			Message: fmt.Sprintf("skipped: %v", lerr),
		})
	}

	// 3. Env overrides — always applied last so they beat both files.
	warnings = append(warnings, applyEnv(cfg)...)

	// 4. Keymap structural validation.
	warnings = append(warnings, cfg.Keymap.Validate()...)

	return cfg, warnings, nil
}

// decodeInto reads a TOML file into cfg. A missing file is a no-op (returns
// nil). A malformed file returns the decode error so Load can convert it
// into a Warning. Plugin sub-tables are deep-merged into cfg.Plugins.Merged
// per key — each file decodes its [plugins.<name>] tables into a fresh
// map[string]any, and mergeMap walks the result so nested tables compose
// rather than clobber.
func decodeInto(path string, cfg *Config) error {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}

	type shadow struct {
		Editor   *Editor                   `toml:"editor,omitempty"`
		UI       *UI                       `toml:"ui,omitempty"`
		Autosave *Autosave                 `toml:"autosave,omitempty"`
		Keymap   Keymap                    `toml:"keymap,omitempty"`
		Plugins  map[string]map[string]any `toml:"plugins,omitempty"`
	}
	var sh shadow
	md, err := toml.DecodeFile(path, &sh)
	if err != nil {
		return err
	}

	// Scalar-level merge: only overwrite fields the file actually set.
	// BurntSushi's IsDefined lets us distinguish "present with zero value"
	// from "absent" so a user can't clobber a default by accident.
	if md.IsDefined("editor", "max_file_size") && sh.Editor != nil {
		cfg.Editor.MaxFileSize = sh.Editor.MaxFileSize
	}
	if sh.UI != nil {
		if md.IsDefined("ui", "tree_width") {
			cfg.UI.TreeWidth = sh.UI.TreeWidth
		}
		if md.IsDefined("ui", "saved_fade_after") {
			cfg.UI.SavedFadeAfter = sh.UI.SavedFadeAfter
		}
		if md.IsDefined("ui", "relative_numbers") {
			cfg.UI.RelativeNumbers = sh.UI.RelativeNumbers
		}
	}
	if sh.Autosave != nil {
		if md.IsDefined("autosave", "save_debounce") {
			cfg.Autosave.SaveDebounce = sh.Autosave.SaveDebounce
		}
		if md.IsDefined("autosave", "change_debounce") {
			cfg.Autosave.ChangeDebounce = sh.Autosave.ChangeDebounce
		}
	}
	if sh.Keymap != nil {
		if cfg.Keymap == nil {
			cfg.Keymap = Keymap{}
		}
		for action, binding := range sh.Keymap {
			cfg.Keymap[action] = binding
		}
	}
	if len(sh.Plugins) > 0 {
		if cfg.Plugins.Merged == nil {
			cfg.Plugins.Merged = map[string]map[string]any{}
		}
		for name, src := range sh.Plugins {
			dst, ok := cfg.Plugins.Merged[name]
			if !ok {
				dst = map[string]any{}
				cfg.Plugins.Merged[name] = dst
			}
			mergeMap(dst, src)
		}
	}
	return nil
}

// mergeMap copies src into dst with per-key override semantics. Nested
// tables (map[string]any) recurse so a project file that only sets one
// inner key doesn't wipe its siblings. Arrays and scalars replace
// wholesale — Kitty-style predictability, no implicit concatenation.
func mergeMap(dst, src map[string]any) {
	for k, v := range src {
		if sub, ok := v.(map[string]any); ok {
			if existing, ok := dst[k].(map[string]any); ok {
				mergeMap(existing, sub)
				continue
			}
			// Either dst has no value, or its value is not a table; in
			// both cases the source table wins outright. Copy into a
			// fresh map so future merges into dst don't mutate src.
			fresh := make(map[string]any, len(sub))
			mergeMap(fresh, sub)
			dst[k] = fresh
			continue
		}
		dst[k] = v
	}
}
