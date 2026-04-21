# Nistru — project guide for Claude Code

Nistru is a Bubble Tea TUI editor with an in-proc + out-of-proc plugin system. Top-level `.go` files hold the editor core ([model.go](model.go), [editor.go](editor.go), [palette.go](palette.go), [autosave.go](autosave.go), [main.go](main.go)); the host lives in [plugin/](plugin/); the plugin SDK and harness in [sdk/plugsdk/](sdk/plugsdk/); first-party plugins in [plugins/](plugins/); examples in [examples/](examples/). See [docs/testing.md](docs/testing.md) for the full testing strategy. This file is the fast rules.

## Commands

| Situation | Command |
|---|---|
| Inner-loop test while coding | `make test-short` |
| Done-gate for any code task | `make ci` |
| Before touching `Model.View` / `Model.Update` / plugin host lifecycle | `make e2e` |
| Before touching `plugin/extproc.go` or anything with goroutines | `make race` |
| Before touching `plugin/protocol.go` or `plugin/manifest.go` | `make fuzz` |
| Quick lint | `make lint` |
| Regenerate golden snapshots (after deliberate View changes) | `go test -tags=e2e -count=1 -update ./...` |

See [Makefile](Makefile) for target bodies. `make ci` runs `go vet`, `staticcheck`, and `-short -race -count=1` with coverage; it is the done-gate.

## Testing tiers

- **Unit** — pure function, no `tea.Msg`, no I/O → `*_test.go` in the same package.
- **Component** — drives `Model.Update` or `plugin.Host` directly, no `tea.Program` → [model_component_test.go](model_component_test.go), [plugin/host_test.go](plugin/host_test.go), [plugin/gaps_test.go](plugin/gaps_test.go).
- **E2E** — real `tea.Program` via `teatest`, behind `//go:build e2e` → [model_e2e_test.go](model_e2e_test.go).

See [docs/testing.md](docs/testing.md) for when each tier applies.

## When modifying X, update Y

- Touching [model.go](model.go) `Update()` / message handling → add or adjust a test in [model_component_test.go](model_component_test.go); if behavior-visible, also update [model_e2e_test.go](model_e2e_test.go) snapshots.
- Touching [model.go](model.go) `View()` / `renderStatusBar()` / layout → regenerate affected goldens under [testdata/golden/](testdata/golden/) (`go test -tags=e2e -update ./...`), **diff them by hand in the PR**, do not blanket-accept.
- Touching [palette.go](palette.go) (filtering, layout) → [palette_test.go](palette_test.go) unit coverage + visual regression via `testdata/golden/palette_*`.
- Touching [autosave.go](autosave.go) → extend [autosave_test.go](autosave_test.go); if `flushNow` shape changes, adjust the `nowFunc` seam or the component-test clock stub accordingly.
- Touching [editor.go](editor.go) (Ctrl-shortcut mappings, mode synthesis) → [editor_test.go](editor_test.go); beware the `synthVimMotion` regression fix from commit `0ee96ea` (Ctrl shortcuts must transit Normal mode).
- Touching [plugin/protocol.go](plugin/protocol.go) → extend `FuzzCodec` corpus in [plugin/fuzz_test.go](plugin/fuzz_test.go); add a round-trip test in [plugin/protocol_test.go](plugin/protocol_test.go) (see `TestCodec_NullResultResponseRoundTrip` as the regression pattern).
- Touching [plugin/manifest.go](plugin/manifest.go) → extend `FuzzManifest` corpus and [plugin/manifest_test.go](plugin/manifest_test.go).
- Touching [plugin/activation.go](plugin/activation.go) → extend `FuzzActivationGlob` corpus and [plugin/activation_test.go](plugin/activation_test.go).
- Touching [plugin/host.go](plugin/host.go) / [plugin/extproc.go](plugin/extproc.go) (goroutines, shutdown, coalescing) → run `make race` + `-count=3`; extend [plugin/host_test.go](plugin/host_test.go) or [plugin/gaps_test.go](plugin/gaps_test.go) for the new behavior.
- Touching [sdk/plugsdk/plugsdk.go](sdk/plugsdk/plugsdk.go) → update [sdk/plugsdk/plugsdk_test.go](sdk/plugsdk/plugsdk_test.go); if a new public harness API is needed, add it to [sdk/plugsdk/plugintest/](sdk/plugsdk/plugintest/).
- Adding a new **plugin event or effect** → extend tests on both sides: host side in [plugin/host_test.go](plugin/host_test.go), SDK side in [sdk/plugsdk/plugsdk_test.go](sdk/plugsdk/plugsdk_test.go), and the harness in [sdk/plugsdk/plugintest/plugintest.go](sdk/plugsdk/plugintest/plugintest.go).
- Adding a new **palette command or keybinding** → [palette_test.go](palette_test.go) unit + either [model_component_test.go](model_component_test.go) or [model_e2e_test.go](model_e2e_test.go) flow.

## Goldens

- Location: [testdata/golden/](testdata/golden/) (`*.txt`).
- Regenerate: `go test -tags=e2e -count=1 -update ./...`.
- **Always diff the regenerated files in your PR.** Minor layout drift is the main way subtle rendering regressions slip in.
- If a golden fails, don't just `-update`. Read the diff. Understand what changed. If intentional, accept; if not, fix the code.
- Goldens are only asserted under the `e2e` build tag — default `go test ./...` never hits them.

## Plugin testing

For writing tests for plugins (yours or the examples), see [docs/plugins.md](docs/plugins.md) — specifically the `Testing your plugin` section. The [sdk/plugsdk/plugintest/](sdk/plugsdk/plugintest/) package is the public harness.

## House rules

- Stdlib `testing` only — no testify, no gomock, no ginkgo.
- No bare `time.Sleep` in tests. Use channel signals, `waitUntil`, or `teatest.WaitFor`.
- `//go:build e2e` tag for anything that boots a `tea.Program`. Default `go test ./...` must stay fast.
- Keep the test pyramid honest: don't reach for e2e when a component test would do.
- Never bypass tests with `--no-verify` or `t.Skip` without an inline explanation.
