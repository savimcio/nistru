# Roadmap

Nistru's path to v1.0. See [docs/plugins.md](plugins.md) for plugin authoring details.

## Where we're going

> **Simple to use. Modern core. Best-in-class plugin API. LSP as a first-party plugin.**

- **Core stays small and well-designed.** When a feature can reasonably live as a plugin, it does.
- **First-party plugin set is curated** — LSP, formatters, linters, syntax highlighting, auto-update. They dogfood the same protocol third-party authors use.
- **Plugin API is the product.** Simple enough to wrap a 20-year-old CLI tool in a few lines of TOML, powerful enough to host a real LSP server.
- **Keybinding ethos.** Modal editing is present via goeditor, but Nistru's *defaults* shift toward casual / modern IDE-style bindings. Vim bindings remain fully available — users who want them set them via `[keymap]` in config. Defaults aim at newcomers; power users bring their muscle memory.
- **Familiar IDE-grade UX.** Find bar with match count and regex toggles, `Shift+Shift` unified finder, `Ctrl+Shift+F` project search, `F7` find usages — the patterns that make modern IDEs feel instantly usable, in a TUI.

**Explicitly out of core (now and at v1.0):** git integration and AI. Community plugins on top of the protocol, not first-party.

**Deferred (post-1.0):** tabs / splits / multi-buffer.

## Where we are

Nistru today ships modal editing via goeditor, a file-tree sidebar, autosave, a dual-transport plugin system (in-proc Go + out-of-proc JSON-RPC) with activation rules, layered TOML config, and a first-party auto-update plugin — packaged via goreleaser with a Homebrew tap and deb/rpm artifacts. The architectural bones are solid. The next push is the "can I live in this?" surface: in-file search, Search Everywhere, syntax highlighting, themes, and the richer plugin surfaces (diagnostics, hover, completion) that unlock LSP as a first-party plugin.

## Phase 1 — Core daily-driver essentials (v0.3)

| ID | Title | Description | Status | Issue |
|---|---|---|---|---|
| **T1** | In-file search & replace | `Ctrl+F` find popup with match count, regex/case/word toggles, history; `Ctrl+R` replace with preview; `n`/`N` for vim users | Planned | [#16](https://github.com/savimcio/nistru/issues/16) |
| **T2** | Search Everywhere (`Shift+Shift`) | Unified fuzzy finder across files, commands, settings (and later symbols); `Ctrl+P` stays as command palette fast path | Planned | [#17](https://github.com/savimcio/nistru/issues/17) |
| **T3** | Find in Files (`Ctrl+Shift+F`) | Project grep with scope toggle (project / current dir / open files), preview pane, ripgrep when present, pure-Go fallback | Planned | [#18](https://github.com/savimcio/nistru/issues/18) |
| **T4** | Syntax highlighting plugin | First-party in-proc Chroma-based plugin bundled in the release binary; proves plugins can own core-feeling features | Planned | [#19](https://github.com/savimcio/nistru/issues/19) |
| **T5** | Theme system | Palette loaded from `[theme]` TOML with 2–3 built-ins (gruvbox, nord, solarized); T4 pulls token colors from it | Planned | [#20](https://github.com/savimcio/nistru/issues/20) |
| **T6** | Casual-default keybindings | Modern IDE-style defaults (`Ctrl+/`, `Ctrl+D`, `Shift+Shift`, `F7`, etc.); `bindings = "vim"` preset for one-line opt-in | Planned | [#21](https://github.com/savimcio/nistru/issues/21) |

## Phase 2 — Plugin API v2 (v0.4)

| ID | Title | Description | Status | Issue |
|---|---|---|---|---|
| **T7** | Declarative CLI-wrapper manifests | Pure-TOML plugin mode with a built-in runner that executes a command, parses output, emits diagnostics/status (example TOML lands with the issue) | Planned | [#22](https://github.com/savimcio/nistru/issues/22) |
| **T8** | Diagnostics overlay protocol | `PublishDiagnostics` effect; host renders inline squiggles, gutter markers, status-bar summary. Feeds T7 wrappers and LSP | Planned | [#23](https://github.com/savimcio/nistru/issues/23) |
| **T9** | Hover popup protocol | `TextDocumentHover` request; plugin returns markdown/text; host renders a floating popup | Planned | [#24](https://github.com/savimcio/nistru/issues/24) |
| **T10** | Completion dropdown protocol | `TextDocumentCompletion` request with trigger info; host renders inline dropdown with keyboard selection | Planned | [#25](https://github.com/savimcio/nistru/issues/25) |
| **T11** | Code actions & Find Usages protocol | `TextDocumentCodeAction` quick-fix menu and `TextDocumentReferences`; references render in the shared results pane with T3 | Planned | [#26](https://github.com/savimcio/nistru/issues/26) |
| **T12** | Plugin capability negotiation | Extend `Initialize` so plugins declare which of T8–T11 they implement; host routes only declared requests | Planned | [#27](https://github.com/savimcio/nistru/issues/27) |

## Phase 3 — First-party LSP + exemplars (v0.5)

| ID | Title | Description | Status | Issue |
|---|---|---|---|---|
| **T13** | `nistru-lsp` bundled plugin | Generic LSP adapter spawning servers from config (example config lands with the issue); implements T8–T11, goto-def, formatting | Planned | [#28](https://github.com/savimcio/nistru/issues/28) |
| **T14** | F7 Find Usages end-to-end | Core `F7` keybinding invokes the references protocol on the symbol under cursor; day-one feature against T13 | Planned | [#29](https://github.com/savimcio/nistru/issues/29) |
| **T15** | Bundled exemplar CLI wrappers | `prettier`, `gofmt` (upgrade), `ruff` as tiny TOML manifests proving T7 and serving as copy-paste starting points | Planned | [#30](https://github.com/savimcio/nistru/issues/30) |

## Phase 4 — Polish & ship (v0.6, on the way to v1.0)

| ID | Title | Description | Status | Issue |
|---|---|---|---|---|
| **T16** | Tree pane operations | Create / rename / delete file & directory from the sidebar | Planned | [#31](https://github.com/savimcio/nistru/issues/31) |
| **T17** | `.editorconfig` support | Indent, charset, trim trailing whitespace, final newline | Planned | [#32](https://github.com/savimcio/nistru/issues/32) |
| **T18** | Goto line (`Ctrl+G`) | "Go to Line" popup | Planned | [#33](https://github.com/savimcio/nistru/issues/33) |
| **T19** | Status bar additions | Line/col, language, encoding indicators | Planned | [#34](https://github.com/savimcio/nistru/issues/34) |
| **T20** | Asciinema cast in README | Single highest popularity-per-hour action; TODO already in README | Planned | [#35](https://github.com/savimcio/nistru/issues/35) |
| **T21** | Plugin gallery doc | `docs/plugins-gallery.md` with first-party + community plugins, screenshots/GIFs | Planned | [#36](https://github.com/savimcio/nistru/issues/36) |
| **T22** | Plugin author quickstart | 5-minute "wrap your first CLI" guide using T7, separate from the `docs/plugins.md` deep dive | Planned | [#37](https://github.com/savimcio/nistru/issues/37) |

## Deferred (post-1.0)

| ID | Title | Description |
|---|---|---|
| **D1** | Multi-buffer / tabs / splits | Nice-to-have, later |
| **D2** | Git community plugin | On top of the protocol, not first-party |
| **D3** | AI community plugin | On top of the protocol, not first-party |
| **D4** | Remote / collab / terminal pane | Remote editing, collaborative editing, embedded terminal |
| **D5** | Extra IDE bindings | `Ctrl+E` recent files, `Shift+F6` rename refactor, `Ctrl+Alt+L` format file, `Ctrl+B` goto-declaration (LSP-backed) |

## Contributing

Contributions are welcome — start with [CONTRIBUTING.md](../CONTRIBUTING.md) for the development workflow. Plugin authors should read [docs/plugins.md](plugins.md) for the protocol and SDK; a 5-minute CLI-wrapper quickstart (T22) is coming in v1.0. Comments on roadmap items belong on the linked issue once they exist — opinions on scope, priority, or alternate approaches are all fair game.
