package config

// CommentedDefaults returns a TOML skeleton that lists every key in the
// v1 schema as a commented-out line prefixed with "# ", together with
// short section headers. Users uncomment the lines they want to change
// (Kitty-style). Used by the `openUser` palette command to seed a fresh
// user config file.
func CommentedDefaults() string {
	return `# Nistru configuration.
# Uncomment any line below to override the built-in default.
# Schema v1. See the Nistru repository for per-key documentation.

# [editor]
# max_file_size = "1MiB"

# [ui]
# tree_width        = 30
# saved_fade_after  = "3s"
# relative_numbers  = true

# [autosave]
# save_debounce   = "250ms"
# change_debounce = "50ms"

# [keymap]
# save       = "ctrl+s"
# quit       = "ctrl+q"
# palette    = "ctrl+p"
# focus_next = "tab"
# focus_prev = "shift+tab"
# undo       = "ctrl+z"
# redo       = "ctrl+y"
# cut_line   = "ctrl+x"
# copy_line  = "ctrl+c"
# paste      = "ctrl+v"

# [plugins.autoupdate]
# repo     = "savimcio/nistru"
# channel  = "release"
# interval = "1h"
# disable  = false

# [plugins.treepane]
# skip_dirs = [".git", "node_modules", "vendor", "dist", "build"]
`
}
