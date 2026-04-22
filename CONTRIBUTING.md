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
make test-short                 # ~3s; the inner-loop test target
go build -o nistru ./cmd/nistru # produces ./nistru
./nistru -path .                # open the repo in the editor
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
chore(deps): bump bubbletea/v2 from 2.0.6 to 2.1.0
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

## Releasing

Maintainer-facing. Releases are tag-triggered: pushing any `v*` tag fires the GoReleaser workflow, which cross-builds archives, packages, and the Homebrew formula.

**Cutting a release.** From a merged-to-master commit:

```sh
git checkout master
git pull origin master
git tag -a vX.Y.Z -m "vX.Y.Z"
git push origin vX.Y.Z
```

Do **not** push `v*` tags from feature branches. The workflow triggers on any `v*` tag push regardless of SHA and will happily release from wherever you pointed the tag.

**Prereleases.** Tags with a SemVer prerelease identifier (`vX.Y.Z-rc.1`, `vX.Y.Z-beta.2`, etc.) are detected by `release.prerelease: auto` in `.goreleaser.yaml` and published with `prerelease: true` on GitHub. That flag is what feeds the `autoupdate` plugin's `dev` channel.

**Homebrew tap.** Stable tags (no prerelease identifier) publish `Formula/nistru.rb` to [`savimcio/homebrew-tap`](https://github.com/savimcio/homebrew-tap) using the `HOMEBREW_TAP_TOKEN` secret — a fine-grained PAT scoped to that tap, stored as a repo secret on `savimcio/nistru`. Prereleases skip the tap via `brews.skip_upload: auto`.

**Local validation before tagging.**

| Command | Effect |
|---|---|
| `make release-check` | lints the goreleaser config without building |
| `make release-snapshot` | full dry-run build into `dist/`; produces every artifact the real pipeline would |

Run both before pushing a tag. A snapshot that fails locally will fail in CI.

## Bug reports and features

Use the GitHub issue forms. The bug form asks for the Nistru commit, Go version, OS, and terminal — fill them in; those four answers unblock most triage.

For security vulnerabilities, see [SECURITY.md](SECURITY.md). **Do not** file them as public issues.

## Code of Conduct

Participation is subject to the [Code of Conduct](CODE_OF_CONDUCT.md). Treat everyone with respect; critique ideas, not people.

## License

By contributing, you agree that your contributions will be licensed under the [MIT License](LICENSE).
