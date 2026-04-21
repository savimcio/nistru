# Contributing to Nistru

Thanks for your interest. Nistru is a small, opinionated TUI editor; contributions that sharpen what's there are welcome, as are focused bug reports and new plugins.

## Prerequisites

- Go **1.26** or newer (the module's minimum is pinned in `go.mod`).
- A terminal that renders Unicode and ANSI colour. Most modern terminals qualify.
- `make` and `gofmt` on your PATH.

## Getting the code

```sh
git clone https://github.com/savimcio/nistru.git
cd nistru
```

Sanity check:

```sh
make test-short   # ~3s; the inner-loop test target
go build          # produces ./nistru
./nistru -path .  # open the repo in the editor
```

If both work, you're set.

## The inner loop

- **While coding:** `make test-short` — fast, no race detector, no e2e.
- **Before pushing:** `make ci` — vet, staticcheck, race-enabled short tests with coverage. This is the done-gate for any code change.

## When to run the specialised targets

Quick reference — the full rules are in `CLAUDE.md` (`## Commands`):

| Situation | Command |
|---|---|
| Touching `Model.View` / `Model.Update` / plugin host lifecycle | `make e2e` |
| Touching `plugin/extproc.go` or anything with goroutines | `make race` |
| Touching `plugin/protocol.go` or `plugin/manifest.go` | `make fuzz` |
| Regenerating golden snapshots (after deliberate View changes) | `go test -tags=e2e -count=1 -update ./...` |

## Writing tests

Nistru uses a mildly trophy-tilted pyramid. Pick the right tier:

- **Unit** — pure functions, no `tea.Msg`, no I/O.
- **Component** — drives `Model.Update` or `plugin.Host` directly, no `tea.Program`.
- **E2E** — real `tea.Program` via `teatest`, behind `//go:build e2e`.

Full rubric and recipes: [docs/testing.md](docs/testing.md).

Plugin authors: the test harness for your own plugin lives at `sdk/plugsdk/plugintest`. See [docs/plugins.md](docs/plugins.md) → "Testing your plugin".

## Commit conventions

Conventional commits. Subject ≤72 chars, imperative mood:

```
feat(plugin): coalesce rapid DidChange events per path
fix: close codex review findings (save/quit safety)
test: stand up testing pyramid with CI, fuzz, and plugin harness
docs: document the in-proc pane API
chore(deps): bump bubbletea from 1.3.10 to 1.4.0
```

For examples grounded in this repo, run `git log --oneline -20`.

One logical change per commit. If you've stacked work, split it — smaller PRs get reviewed faster.

## Pull requests

- Keep them focused. If a PR grows past ~400 lines of real code, split it.
- `make ci` must be green locally before you push. CI will re-run it; that's belt-and-braces, not a substitute.
- Regenerate affected goldens with `go test -tags=e2e -update ./...` and **read the diff by hand** before committing. Blind `-update` hides regressions.
- Update docs when behaviour changes: `README.md` for user-visible features, `docs/plugins.md` for plugin-facing changes, `docs/testing.md` for test infrastructure, `CLAUDE.md` for contributor-workflow changes.
- Follow the PR template — it's short; fill every section.

## Code style

- `gofmt` + `staticcheck` are both wired into `make ci` and must stay clean.
- Prefer the stdlib. New third-party runtime deps need a clear rationale in the PR description.
- Tabs in Go; spaces in YAML/JSON/Markdown; `.editorconfig` encodes the defaults.
- Package doc comments (`// Package foo ...`) on every exported package.
- Write no comments unless the *why* is non-obvious. Don't narrate what the code already says.

## Bug reports and features

Use the GitHub issue forms. The bug form asks for the Nistru commit, Go version, OS, and terminal — fill them in; those four answers unblock most triage.

For security vulnerabilities, see [SECURITY.md](SECURITY.md). **Do not** file them as public issues.

## Code of Conduct

Participation is subject to the [Code of Conduct](CODE_OF_CONDUCT.md). Treat everyone with respect; critique ideas, not people.

## License

By contributing, you agree that your contributions will be licensed under the [MIT License](LICENSE).
