# Testing

## Overview

Nistru's test suite is a **trophy-tilted pyramid**: a thin slice of pure-unit tests at the base, a heavy middle of component tests that drive `Model.Update` and `plugin.Host` directly, a capped layer of TUI e2e flows on top, and a band of non-functional sweeps (race, vet, staticcheck, fuzz) running alongside everything else. The tilt is deliberate. Nistru is a TUI plus an out-of-process plugin host, so the bugs that actually ship are state-machine transitions (dirty-flag loss on file switch, Ctrl shortcuts leaking into Insert mode) and concurrency contract violations (coalescing, crash isolation, shutdown ordering). Pure functions are cheap to test but rarely break; the component and e2e tiers earn their weight.

The gate is `make ci`. It runs in under thirty seconds with `-race` enabled, and it is what you run before you push. Everything else in this document is plumbing around that gate — how the tiers are laid out, how to add to them, and how to regenerate the golden snapshots when `View()` legitimately changes.

## Pyramid

```
    +-- TUI e2e (teatest, goldens) --+   model_e2e_test.go (//go:build e2e)
    |     ~20%                       |   testdata/golden/
    +-- Component (Model.Update,   --+   model_component_test.go
    |   Host direct)   ~40%         |   plugin/host_test.go, gaps_test.go
    +-- Unit (pure helpers)   ~30% -+   palette_test.go, autosave_test.go,
    |                              |   model_test.go, editor_test.go,
    |                              |   plugin/{activation,manifest,protocol}_test.go
    +-- Non-functional sweeps ~10%-+   fuzz_test.go, -race, staticcheck, vet
```

## Tier rubric

Three lines decide where a new test goes:

- **Pure function, no `tea.Msg`, no I/O** -> **Unit**. Lives next to the code. See [`palette_test.go`](../palette_test.go), [`autosave_test.go`](../autosave_test.go), [`plugin/activation_test.go`](../plugin/activation_test.go), [`plugin/manifest_test.go`](../plugin/manifest_test.go), [`plugin/protocol_test.go`](../plugin/protocol_test.go).
- **Calls `Model.Update(msg)` or `plugin.Host.Emit`/`ExecuteCommand` but never starts a `tea.Program`** -> **Component**. See [`model_component_test.go`](../model_component_test.go), [`plugin/host_test.go`](../plugin/host_test.go), [`plugin/gaps_test.go`](../plugin/gaps_test.go).
- **Starts a real program via `teatest.NewTestModel`** -> **E2E**. Lives under the `//go:build e2e` tag so the default `go test` stays fast. See [`model_e2e_test.go`](../model_e2e_test.go).

When in doubt, drop one tier: a component test that exercises a state transition is almost always more useful than the e2e flow around it. The trade-off: component tests run in microseconds and catch state-machine regressions directly; e2e tests run in tens of milliseconds each and catch only the bugs that survive `Update` but manifest at `View`.

### Coverage targets

The numbers to beat are recorded in the plan and enforced by eyeball, not by CI gate:

- `plugin/` >= 64% (T2 baseline).
- `plugins/treepane/` >= 77%.
- Root package >= 65% (currently 68.6%).
- `sdk/plugsdk` + `sdk/plugsdk/plugintest` combined >= 80%.

If your PR drops a number, raise a new test rather than lower the floor.

## Running tests

All targets are declared in the [Makefile](../Makefile). Authoritative list lives there; this table is a cheat sheet.

| Target | What it does | When to use it |
|---|---|---|
| `make test` | `go test ./...` | Rarely -- prefer `test-short` during dev, `ci` before push. |
| `make test-short` | `go test -short ./...` | Fast inner loop. Skips the long-debounce autosave cases. |
| `make race` | `go test -race -count=1 ./...` | Before touching `plugin/extproc.go` or anything with goroutines. |
| `make cover` | Writes `coverage.out`, prints total. | When you want to see where you stand. |
| `make lint` | `go vet` + `staticcheck` via [`tools.go`](../tools.go). | Before push; also part of `ci`. |
| `make fuzz` | Runs every `Fuzz*` target for 10 s each. | Before touching the codec or manifest parser. |
| `make e2e` | `go test -tags=e2e -count=1 ./...` | Before touching `Model.View`, `Model.Update`, or plugin host lifecycle. |
| `make ci` | vet + staticcheck + `-race -short -count=1` + coverage. | **The done-gate. Run before you push.** |
| `make fmt` | `gofmt -s -w .` | After a large refactor. |
| `make clean` | Removes coverage artifacts. | When they get in the way. |

## Golden files

E2E snapshots live under [`testdata/golden/`](../testdata/golden/). The runner in [`model_e2e_test.go`](../model_e2e_test.go) normalises output (strips ANSI, trims trailing whitespace per line, collapses `\r\n` to `\n`) before diffing, so environment colour differences do not break the suite.

Regenerate a golden after a legitimate `View()` change:

```sh
go test -tags=e2e -count=1 -update ./...
```

The `-update` flag is declared at the package level (piggy-backing on a transitive dep that already registers it). When set, every failing or passing snapshot test writes its current normalised output back to disk.

**Review etiquette.** *Always* diff goldens in PRs, line by line. Never blanket-accept a `-update` regen. A bad accept looks like this: you change one status-bar colour, run `-update` without reading the diff, and accidentally commit a treepane snapshot that now has a stale path from last week's experiment still embedded in it. The regression slips because the diff was never examined. The rule: if a golden file changed, the PR description must explain why, and a reviewer must eyeball the diff.

## Fuzzing

Three targets live in [`plugin/fuzz_test.go`](../plugin/fuzz_test.go):

- `FuzzCodec` -- JSON-RPC 2.0 frames fed into `Codec.Read`.
- `FuzzManifest` -- `plugin.json` bytes fed into `LoadManifest`.
- `FuzzActivationGlob` -- `(activation, path)` pairs fed into `ParseActivation` + `Match`.

Seeds are embedded with `f.Add` rather than checked in under `plugin/testdata/fuzz/<FuzzName>/`, so the corpus is visible next to the target. A crash corpus, if one ever materialises, will appear under `plugin/testdata/fuzz/<Name>/NEW_CRASH` after a live run.

Run one target for 30 s:

```sh
go test -run=^$ -fuzz=^FuzzCodec$ -fuzztime=30s ./plugin/
```

`make fuzz` sweeps every `Fuzz*` target at 10 s each.

**If a fuzz run finds a crash:** the runner writes a reproducer under `plugin/testdata/fuzz/<Name>/<hash>`. Check it in, fix the underlying bug, and keep the seed as a permanent regression test. Do not delete the corpus entry after fixing; it is your proof the bug stays fixed.

## Writing component tests

Recipe:

1. Build a `*Model` via a test helper (`newTestModel(t, dir)` in [`model_component_test.go`](../model_component_test.go)).
2. Craft a `tea.Msg` that represents the event you want to test.
3. Call `m.Update(msg)`.
4. Assert returned model state; drain the returned `tea.Cmd` if the flow needs a follow-up message.

```go
newM, cmd := m.Update(openFileRequestMsg{path: path})
got := newM.(*Model)
if got.openPath != path {
    t.Errorf("openPath: got %q, want %q", got.openPath, path)
}
if got.dirty {
    t.Errorf("dirty should be false after successful open")
}
_ = cmd // drain if the test needs the follow-up msg
```

Canonical example: [`model_component_test.go`](../model_component_test.go). Use it as the template for any new flow.

For component tests that need a live out-of-process plugin without the cost of a full e2e build, `plugin/host_test.go` has a `PLUGIN_MODE` self-spawn pattern: the test binary re-executes itself with an env var set, and an `init()` branch runs a minimal plugin main. Reuse it; do not reinvent the subprocess dance.

## Writing e2e tests

Recipe:

1. `tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(80, 24))`.
2. Send input with `tm.Send(tea.KeyMsg{...})` or `tm.Type("hello")`.
3. Wait on a condition with `teatest.WaitFor(t, tm.Output(), func(b []byte) bool { ... })`.
4. Either snapshot via `assertGolden` or inspect the final model via `tm.FinalModel(t, ...)`.

```go
tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(80, 24))
t.Cleanup(func() { _ = tm.Quit() })

tm.Send(tea.KeyMsg{Type: tea.KeyCtrlP})
tm.Type("Ping")
tm.Send(tea.KeyMsg{Type: tea.KeyEnter})

teatest.WaitFor(t, tm.Output(), func(b []byte) bool {
    return bytes.Contains(b, []byte("unknown command"))
}, teatest.WithCheckInterval(5*time.Millisecond), teatest.WithDuration(time.Second))
```

Canonical example: [`model_e2e_test.go`](../model_e2e_test.go).

## Testing your plugin

Plugin authors get their own harness. See the **Testing your plugin** section in [docs/plugins.md](./plugins.md) for the full walkthrough -- it covers `plugintest.New(t, plugin)`, the event-feeder and inspection APIs, and the synchronous-round-trip footgun. The harness is part of the SDK at [`sdk/plugsdk/plugintest/`](../sdk/plugsdk/plugintest/plugintest.go); both bundled examples use it.

## CI

[`.github/workflows/ci.yml`](../.github/workflows/ci.yml) runs a single job on `ubuntu-latest` with a `go-version: ['1.26.x']` matrix. It executes `make ci`, uploads `coverage.out` as an artifact, and emits the coverage summary into the job summary. Both `push` and `pull_request` events trigger it. Read the summary in the GitHub Actions UI; download the artifact if you need per-file breakdowns.

## Flakiness policy

**No bare `time.Sleep` in tests.** Use a signal channel (`done <- struct{}{}`) or a bounded `waitUntil` helper with `t.Helper()` and an explicit attempt counter. The T2 refactor removed every `time.Sleep` from `plugin/host_test.go` precisely because they were the suite's dominant flake vector under CI load. The pattern to follow: block on a channel the code-under-test writes to, or spin on a cheap predicate with a generous timeout and a per-attempt interval that respects `testing.Short()`. If you find yourself reaching for `time.Sleep`, redesign.
