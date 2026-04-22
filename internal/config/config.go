// Package config defines Nistru's layered TOML configuration: built-in
// defaults, a per-user file under <UserConfigDir>/nistru/config.toml, a
// per-project file at <root>/.nistru/config.toml, and a whitelist of
// NISTRU_* environment overrides (file order is lowest-to-highest
// precedence; env beats all files). Plugin sub-tables live under
// [plugins.<name>] and are decoded into generic maps so the loader can
// deep-merge user → project → env per key.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the fully-merged view of Nistru's TOML configuration. It is the
// output of Load() and the input of every subsystem that needs tunables.
type Config struct {
	Editor   Editor   `toml:"editor"`
	UI       UI       `toml:"ui"`
	Autosave Autosave `toml:"autosave"`
	Keymap   Keymap   `toml:"keymap"`
	Plugins  Plugins  `toml:"plugins"`
}

// Editor holds editor-wide tunables.
type Editor struct {
	MaxFileSize Size `toml:"max_file_size"`
}

// UI holds chrome/layout tunables.
type UI struct {
	TreeWidth       int           `toml:"tree_width"`
	SavedFadeAfter  time.Duration `toml:"saved_fade_after"`
	RelativeNumbers bool          `toml:"relative_numbers"`
}

// Autosave holds debounce knobs for the autosave subsystem.
type Autosave struct {
	SaveDebounce   time.Duration `toml:"save_debounce"`
	ChangeDebounce time.Duration `toml:"change_debounce"`
}

// Plugins holds the merged [plugins.<name>] sub-trees as plain map values.
// Each per-file decode lands in its own map[string]any; the loader
// deep-merges them per key (project overrides user) into Merged. EnvOverlay
// is a separate map so PluginConfig can re-merge it on top at lookup time —
// env always wins key-by-key.
//
// The previous design stored BurntSushi Primitives plus the MetaData that
// produced them, which silently broke when a plugin appeared in more than
// one file: Primitives can only be PrimitiveDecode'd against the MetaData
// they were parsed from, and the loader was overwriting one with the other.
type Plugins struct {
	Merged     map[string]map[string]any `toml:"-"`
	EnvOverlay map[string]map[string]any `toml:"-"`
}

// Defaults returns a fully-populated *Config with Nistru's built-in
// defaults. Plugin sub-tables are left empty — those are supplied only
// when a user or project writes them.
func Defaults() *Config {
	return &Config{
		Editor: Editor{
			MaxFileSize: 1 << 20, // 1 MiB
		},
		UI: UI{
			TreeWidth:       30,
			SavedFadeAfter:  3 * time.Second,
			RelativeNumbers: true,
		},
		Autosave: Autosave{
			SaveDebounce:   250 * time.Millisecond,
			ChangeDebounce: 50 * time.Millisecond,
		},
		Keymap: DefaultKeymap(),
		Plugins: Plugins{
			Merged:     map[string]map[string]any{},
			EnvOverlay: map[string]map[string]any{},
		},
	}
}

// Size is a byte count that parses human-friendly TOML strings like
// "1MiB" / "512KiB" / "2MB" (case-insensitive, binary units are 1024-based
// and decimal units are 1000-based) or a plain integer byte count.
type Size uint64

// UnmarshalText implements encoding.TextUnmarshaler for Size.
func (s *Size) UnmarshalText(text []byte) error {
	raw := strings.TrimSpace(string(text))
	if raw == "" {
		return fmt.Errorf("config: empty size")
	}
	// Plain integer → bytes.
	if n, err := strconv.ParseUint(raw, 10, 64); err == nil {
		*s = Size(n)
		return nil
	}
	// Otherwise find the longest trailing alphabetic suffix.
	cut := len(raw)
	for cut > 0 {
		c := raw[cut-1]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			cut--
			continue
		}
		break
	}
	if cut == 0 || cut == len(raw) {
		return fmt.Errorf("config: invalid size %q", raw)
	}
	numPart := strings.TrimSpace(raw[:cut])
	suffix := strings.ToLower(strings.TrimSpace(raw[cut:]))
	n, err := strconv.ParseUint(numPart, 10, 64)
	if err != nil {
		return fmt.Errorf("config: invalid size %q: %w", raw, err)
	}
	var mult uint64
	switch suffix {
	case "b":
		mult = 1
	case "kb":
		mult = 1_000
	case "mb":
		mult = 1_000_000
	case "gb":
		mult = 1_000_000_000
	case "kib":
		mult = 1 << 10
	case "mib":
		mult = 1 << 20
	case "gib":
		mult = 1 << 30
	default:
		return fmt.Errorf("config: unknown size suffix %q", suffix)
	}
	// Guard against uint64 wrap in n*mult. mult is always >= 1 for the
	// branches above, so the divisor is safe.
	if n > math.MaxUint64/mult {
		return fmt.Errorf("config: size %q overflows uint64", raw)
	}
	*s = Size(n * mult)
	return nil
}

// String renders Size as a plain byte count — we don't try to guess the
// "nicest" unit, callers that want to pretty-print should do so themselves.
func (s Size) String() string { return strconv.FormatUint(uint64(s), 10) }

// MarshalText implements encoding.TextMarshaler so Size round-trips through
// EncodeResolved as the canonical byte count form.
func (s Size) MarshalText() ([]byte, error) {
	return []byte(s.String()), nil
}

// PluginConfig returns the raw JSON bytes for plugins.<name>, or nil when
// that plugin has neither a file entry nor an env overlay. The merged
// file-level map is overlaid with EnvOverlay (env wins key-by-key) and the
// result is marshaled as JSON.
func (c *Config) PluginConfig(name string) json.RawMessage {
	base := c.Plugins.Merged[name]
	overlay := c.Plugins.EnvOverlay[name]
	if len(base) == 0 && len(overlay) == 0 {
		return nil
	}
	merged := make(map[string]any, len(base)+len(overlay))
	maps.Copy(merged, base)
	maps.Copy(merged, overlay) // env wins key-by-key
	b, err := json.Marshal(merged)
	if err != nil {
		return nil
	}
	return b
}

// EncodeResolved dumps the fully-merged Config back to TOML. Plugin
// sub-tables come straight from the merged + env-overlaid view that
// PluginConfig returns; we deliberately re-do the merge here rather than
// calling json.Marshal/Unmarshal so int and bool values survive as TOML
// integers/booleans rather than being coerced to JSON numbers. Used by the
// `showResolved` palette command so users can see exactly what the editor
// computed.
func (c *Config) EncodeResolved() ([]byte, error) {
	type resolved struct {
		Editor   Editor                    `toml:"editor"`
		UI       UI                        `toml:"ui"`
		Autosave Autosave                  `toml:"autosave"`
		Keymap   map[string]string         `toml:"keymap"`
		Plugins  map[string]map[string]any `toml:"plugins,omitempty"`
	}
	km := make(map[string]string, len(c.Keymap))
	for a, v := range c.Keymap {
		km[string(a)] = v
	}
	r := resolved{
		Editor:   c.Editor,
		UI:       c.UI,
		Autosave: c.Autosave,
		Keymap:   km,
	}
	if len(c.Plugins.Merged) > 0 || len(c.Plugins.EnvOverlay) > 0 {
		// Collect every plugin name that appears in either map so plugins
		// that are env-only (no file entry) still show up in the dump.
		names := map[string]struct{}{}
		for name := range c.Plugins.Merged {
			names[name] = struct{}{}
		}
		for name := range c.Plugins.EnvOverlay {
			names[name] = struct{}{}
		}
		r.Plugins = map[string]map[string]any{}
		for name := range names {
			base := c.Plugins.Merged[name]
			overlay := c.Plugins.EnvOverlay[name]
			if len(base) == 0 && len(overlay) == 0 {
				continue
			}
			m := make(map[string]any, len(base)+len(overlay))
			maps.Copy(m, base)
			maps.Copy(m, overlay)
			if len(m) > 0 {
				r.Plugins[name] = m
			}
		}
		if len(r.Plugins) == 0 {
			r.Plugins = nil
		}
	}
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(r); err != nil {
		return nil, fmt.Errorf("config: encode resolved: %w", err)
	}
	return buf.Bytes(), nil
}
