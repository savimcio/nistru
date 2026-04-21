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

```sh
go install github.com/savimcio/nistru@latest
```

Or build from source:

```sh
git clone https://github.com/savimcio/nistru.git
cd nistru
go build
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

- [charmbracelet/bubbletea](https://github.com/charmbracelet/bubbletea) — TUI runtime
- [kujtimiihoxha/vimtea](https://github.com/kujtimiihoxha/vimtea) — modal vim editor component
- [charmbracelet/lipgloss](https://github.com/charmbracelet/lipgloss) — layout and styling

The file tree is rendered by an in-house component loaded through the plugin system.

## License

[MIT](LICENSE) © 2026 savimcio
