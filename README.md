# Nistru

[![CI](https://github.com/savimcio/nistru/actions/workflows/ci.yml/badge.svg)](https://github.com/savimcio/nistru/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/savimcio/nistru.svg)](https://pkg.go.dev/github.com/savimcio/nistru)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

> **Status:** pre-1.0 — actively developed; APIs and plugin manifest schema may break before `v1.0`.

A minimal terminal text editor with a file-tree sidebar, modal vim-style editing, micro-inspired `Ctrl` shortcuts, and a plugin system for formatters, linters, and custom panes.

<!-- TODO: asciinema cast or screenshot here — record on release -->
<!-- Recommended: demo.gif or an asciinema embed like:
     [![asciicast](https://asciinema.org/a/<ID>.svg)](https://asciinema.org/a/<ID>) -->

## Install

### Homebrew (macOS + Linux)

```sh
brew install savimcio/tap/nistru
```

The tap repo is [`savimcio/homebrew-tap`](https://github.com/savimcio/homebrew-tap); Homebrew strips the `homebrew-` prefix, so `savimcio/tap` is the short form used with `brew`.

The formula tracks stable releases; `brew upgrade nistru` picks up new versions.

### Pre-built binaries

Archives for linux/darwin/windows × amd64/arm64 are published on the [Releases page](https://github.com/savimcio/nistru/releases) (windows/arm64 excepted). Each release ships a `checksums.txt` (SHA-256) alongside the archives.

### Debian / RPM

```sh
# Debian / Ubuntu (pick the arch matching your system)
curl -LO https://github.com/savimcio/nistru/releases/latest/download/nistru_<version>_linux_amd64.deb
sudo dpkg -i nistru_<version>_linux_amd64.deb

# Fedora / RHEL
sudo rpm -i https://github.com/savimcio/nistru/releases/download/v<version>/nistru_<version>_linux_amd64.rpm
```

### From source

```sh
go install github.com/savimcio/nistru/cmd/nistru@latest
```

Or build a checkout:

```sh
git clone https://github.com/savimcio/nistru.git
cd nistru
go build -o nistru ./cmd/nistru
./nistru
```

## Usage

```sh
./nistru -path .
```

The `-path` flag selects the root directory for the file tree. It defaults to `.` (the current working directory).

## Keybindings

### Global (every pane, every mode)

| Key | Action |
|---|---|
| `Tab` / `Shift+Tab` | swap focus (tree / editor) |
| `Ctrl+P` | open command palette (plugin commands) |
| `Ctrl+S` | save now (force flush) |
| `Ctrl+Q` | quit (flushes pending save) |

### Tree pane

| Key | Action |
|---|---|
| `j` / `k` | move cursor down / up |
| `g` / `G` | jump to first / last node |
| `Enter` / `l` / `→` | on a directory, toggle/expand; on a file, open it |
| `h` / `←` | collapse the current directory, or jump to its parent |
| `Ctrl+C` | quit |

Directories start collapsed; expand them with `Enter`, `l`, or `→`.

### Editor pane

The editor is modal. It opens in **Normal** mode (cursor navigation). Press `i`, `a`, `o`, or `O` to enter **Insert** mode (typing text); press `Esc` to return.

**Normal mode**

| Key | Action |
|---|---|
| `h` / `j` / `k` / `l` | cursor L / D / U / R |
| `w` / `b` | word forward / back |
| `0` / `$` | line start / end |
| `g g` / `G` | top / bottom of buffer |
| `i` / `a` / `o` / `O` | enter Insert mode (at / after / new line below / above) |
| `x` | delete char under cursor |
| `d d` / `y y` / `p` | delete / yank / paste line |
| `u` / `Ctrl+R` | undo / redo |
| `5 j`, `10 k`, ... | count prefix on any motion |

**Insert mode**

| Key | Action |
|---|---|
| any printable key | insert literally (including `h`/`j`/`k`/`l`) |
| `Esc` | return to Normal mode |

**micro-style `Ctrl` shortcuts (both modes)**

| Key | Action | Underlying |
|---|---|---|
| `Ctrl+S` | save | autosave flush |
| `Ctrl+Q` | quit | app-level `tea.Quit` |
| `Ctrl+Z` | undo | forwards to vim `u` |
| `Ctrl+Y` | redo | forwards to vim `Ctrl+R` |
| `Ctrl+X` | cut line | forwards to vim `dd` |
| `Ctrl+C` | copy line | forwards to vim `yy` |
| `Ctrl+V` | paste | forwards to vim `p` |

## Autosave

Autosave is enabled by default. After any buffer change, a 250 ms idle debounce schedules a write; rapid typing produces exactly one write when typing stops. Writes are atomic (write to `<path>.tmp`, then rename over the original), so a killed process never leaves a half-written file. The right side of the status bar shows `● unsaved` while dirty and `✓ saved` for about a second after each flush. A size guard refuses to open files larger than 1 MiB, and binary files are refused if a NUL byte appears in the first 512 bytes.

## Configuration

Nistru reads layered TOML from two files. Lowest-priority wins first:

1. Built-in defaults.
2. `~/.config/nistru/config.toml` — user-wide.
3. `<root>/.nistru/config.toml` — project-local (overrides user).
4. `NISTRU_*` environment variables — emergency override.

Missing files are not errors; malformed TOML is reported as a startup warning on stderr. See [internal/config/](internal/config/) for the schema.

Minimal example:

```toml
[ui]
tree_width = 40

[autosave]
save_debounce = "500ms"

[plugins.autoupdate]
channel  = "dev"
interval = "30m"
```

Four palette commands (`Ctrl+P`) manage settings:

| Command | Action |
|---|---|
| `Nistru: Open User Settings` | open `~/.config/nistru/config.toml` (seeded with commented defaults if absent) |
| `Nistru: Open Project Settings` | open `<root>/.nistru/config.toml` (seeded if absent) |
| `Nistru: Reload Settings` | reparse both files, re-emit `Initialize` to activated plugins |
| `Nistru: Show Resolved Config` | dump the fully-merged config to `<root>/.nistru/.resolved-config.toml` and open it |

**Reload behaviour.** `Nistru: Reload Settings` takes effect immediately for app-level keybindings (save/quit/palette/focus), UI sizes (tree width, status fade), debounces, and plugin config (`OnConfig` is re-fired on every activated plugin). The in-editor Ctrl bindings (`ctrl+z`/`y`/`x`/`c`/`v`) and the `ui.relative_numbers` flag are baked into the editor adapter at construction time; when any of them changes on reload, the editor instance is rebuilt in place so the new setting is in effect without opening another file. Buffer content and the open file are preserved across the rebuild, but cursor position and vim mode reset to the top of the file in Normal mode.

**Keymap unbinding.** Setting a keybinding to the empty string (e.g. `save = ""`) falls back to the default binding; there is no way to fully unbind an action today.

## Auto-update

Nistru ships with a first-party `autoupdate` plugin that watches GitHub Releases for newer versions. It is enabled by default, runs quietly in the background, and never swaps the binary without explicit palette invocation.

**Installed via a package manager?** If you installed through Homebrew, `.deb`, or `.rpm`, update through that channel instead — the built-in updater targets the single-binary install path (`go install` or a direct archive download).

**What it does.** Once an hour, the plugin issues a single unauthenticated `GET` to `api.github.com/repos/savimcio/nistru/releases`. If a newer version is available, a status-bar segment announces it; palette commands let you install it, roll back, switch channels, or view release notes. No telemetry is collected and no data is sent outbound beyond the GitHub API request itself.

**Channels.**

| Channel | Tracks |
|---|---|
| `release` (default) | stable GitHub releases only |
| `dev` | includes prereleases |

Switch with the `autoupdate:switch-channel` palette command, or start on a specific channel via `NISTRU_AUTOUPDATE_CHANNEL=dev`.

**Environment variables.**

| Variable | Effect |
|---|---|
| `NISTRU_AUTOUPDATE_DISABLE=1` | disables the plugin entirely |
| `NISTRU_AUTOUPDATE_INTERVAL=30m` | custom check interval (any Go `time.ParseDuration` value) |
| `NISTRU_AUTOUPDATE_CHANNEL=release\|dev` | start on a specific channel |
| `NISTRU_AUTOUPDATE_REPO=owner/repo` | override source repository (useful for forks) |

**Installing an update.** Open the palette with `Ctrl+P` and run `autoupdate:install`. On Linux and macOS, the new binary is downloaded, its SHA-256 is verified against the release's `checksums.txt`, and it is atomically swapped in place (the previous binary is retained for the session as `nistru.prev`). Restart nistru (`Ctrl+Q`, then relaunch) to pick up the new version. On Windows, the plugin does **not** swap the binary; it prints the appropriate `go install` command and leaves installation to you.

**Rolling back.** `Ctrl+P` → `autoupdate:rollback` restores the previous binary from `nistru.prev`. The rollback target is retained only for the current session, so restart nistru before relying on it.

**Prerequisites for in-place installation.** The target release must publish artifacts for your `GOOS/GOARCH` alongside a `checksums.txt` file. If your copy of nistru was installed via `go install ...@latest` and no matching release artifact exists, the plugin falls back to notifying you with the correct `go install` command instead of attempting a swap.

**Privacy.** One unauthenticated request per hour to GitHub's public Releases API. GitHub observes the requester's IP address (as it would for any HTTP client). No identifying information, crash data, or editor state is disclosed. To opt out entirely, set `NISTRU_AUTOUPDATE_DISABLE=1`.

## Plugins

Nistru has a plugin system with two transports (in-process Go for panes, out-of-process JSON-RPC for everything else). The bundled file tree is itself a plugin. See **[docs/plugins.md](docs/plugins.md)** for the architecture, manifest schema, SDK, and worked examples.

Two example plugins live in `examples/plugins/`:

- `hello-world/` — registers a palette command that shows a notification.
- `gofmt/` — runs `gofmt` over `.go` files on save.

## Limitations (v1)

- No syntax highlighting.
- No built-in LSP (plugins can adapt one — see `docs/plugins.md`).
- No search/replace.
- No multiple buffers, tabs, or splits.
- Eager tree walk — may be slow on very large repositories.
- Tree skips `.git`, `node_modules`, `vendor`, `dist`, and `build`.

## Testing

Nistru's tests follow a pyramid: cheap unit tests at the base, a thinner layer of integration tests, and a small set of end-to-end checks on top.

- `make test-short` — fast inner loop during development.
- `make ci` — the done-gate: vet, staticcheck, race-enabled short tests with coverage.
- `make race` — full race-detector run.
- `make fuzz` — runs every `Fuzz*` target for 10s each.
- `make e2e` — end-to-end tests gated behind the `e2e` build tag.

See [docs/testing.md](docs/testing.md) for the full tiering rubric.

## Contributing

Bug reports, feature ideas, and patches welcome. See [CONTRIBUTING.md](CONTRIBUTING.md) for dev setup, the testing rubric, and PR expectations. Security reports go to [SECURITY.md](SECURITY.md) (never the public issue tracker).

Participation is subject to the [Code of Conduct](CODE_OF_CONDUCT.md).

## Credits

Built on:

- [charm.land/bubbletea/v2](https://charm.land/bubbletea) — TUI runtime (Charm v2)
- [ionut-t/goeditor](https://github.com/ionut-t/goeditor) — modal vim editor component with grapheme-cluster-aware width handling
- [charm.land/lipgloss/v2](https://charm.land/lipgloss) — layout and styling (Charm v2)

The file tree is rendered by an in-house component loaded through the plugin system.

## License

[MIT](LICENSE) © 2026 savimcio
