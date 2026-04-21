# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.0] — 2026-04-21

Initial public release.

### Editor

- Modal vim editing (via [vimtea](https://github.com/kujtimiihoxha/vimtea)) with micro-style `Ctrl+S/Q/Z/Y/X/C/V` shortcuts that transit Normal mode safely.
- 250 ms debounced autosave with atomic writes (`<path>.tmp` → `rename`); status bar shows `● unsaved` / `✓ saved`.
- Size guard (1 MiB) and binary-file guard (NUL in first 512 bytes) on open.
- Eager file-tree sidebar with collapse/expand; skips `.git`, `node_modules`, `vendor`, `dist`, `build`.
- Command palette (`Ctrl+P`) and a right-side status bar.

### Plugin system

- Two transports sharing one event API: in-process Go (for panes) and out-of-process JSON-RPC 2.0 over stdio (for formatters, linters, custom commands).
- The bundled file tree is itself an in-process plugin.
- Host features: `DidOpen` / `DidChange` / `DidSave` / `DidClose` / `Initialize` / `ExecuteCommand` / `Shutdown`; effects (`OpenFile`, `Notify`, `Focus`, `Invalidate`); `DidChange` coalescing; panic recovery; 3 s graceful-shutdown budget.
- Plugin SDK at `sdk/plugsdk`; in-memory test harness at `sdk/plugsdk/plugintest` so plugin authors can unit-test their plugins against the real SDK `Run` loop.
- Example plugins under `examples/plugins/`: `hello-world` (palette command → notification) and `gofmt` (reformat `.go` files on save).

### Testing + CI

- Trophy-tilted testing pyramid: unit (pure helpers), component (`Model.Update` / `plugin.Host` driven directly), TUI e2e (`teatest` + golden snapshots, `//go:build e2e`), plus non-functional sweeps (`-race`, `vet`, `staticcheck`, `go test -fuzz`).
- Fuzz targets for the JSON-RPC codec, manifest parser, and activation-glob matcher.
- `Makefile` with `test`, `test-short`, `race`, `cover`, `lint`, `fuzz`, `e2e`, `ci`, `fmt`, `clean`, `examples-test`.
- GitHub Actions CI runs `make ci` on Go 1.26.x, exercises the example modules, and uploads a coverage artifact.

### Docs + OSS collateral

- `README.md`, `docs/testing.md`, `docs/plugins.md`, and a project-level `CLAUDE.md` with a "when modifying X, update Y" rule table.
- `LICENSE` (MIT), `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md` (Contributor Covenant 2.1), `SECURITY.md`, GitHub issue forms, pull-request template, Dependabot config, `.editorconfig`.

[0.1.0]: https://github.com/savimcio/nistru/releases/tag/v0.1.0
