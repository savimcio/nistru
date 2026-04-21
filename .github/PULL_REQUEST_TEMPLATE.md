## What

<!-- One-line summary of the change. -->

## Why

<!-- One line of motivation. Link the issue if there is one: Fixes #123. -->

## Testing

<!-- What you ran and what it showed. Paste the tail of `make ci` if relevant. -->

## Checklist

- [ ] `make ci` green locally
- [ ] `make e2e` run if `Model.View` / `Model.Update` / plugin host lifecycle changed
- [ ] Goldens regenerated (`go test -tags=e2e -update ./...`) and diffed by hand if View / layout changed
- [ ] Docs updated if user-visible (README, `docs/`, `CLAUDE.md`)
- [ ] Commit subject follows conventional commits (`feat:`, `fix:`, `docs:`, `test:`, `chore:`, `refactor:`)

## Out of scope / follow-ups

<!-- Optional. Things you noticed but deliberately didn't address here. -->
