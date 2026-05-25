# AGENTS.md — Flanders (operational)

> Operational only: how to build/test/run. Status & progress live in
> `IMPLEMENTATION_PLAN.md`.

## Project

Single Go binary (Go 1.24+) that wraps the `claude` CLI and drives a Ralph loop.
Specs are the source of truth in `specs/*.md`.

## Layout

- Module path: `flanders` (`go.mod` at repo root).
- `src/cmd/flanders/` — `main` entry point.
- `src/lib/*` — shared standard library (paths, logging, …). Import as
  `flanders/src/lib/<pkg>`. Put shared primitives here; no ad-hoc copies.

## Commands (run from repo root)

```sh
go build ./...                # compile everything
go test ./...                 # run all tests (the ground-truth gate)
go vet ./...                  # static checks
go run ./src/cmd/flanders     # run the binary
```

## Runtime

- `.flanders/` (gitignored) holds runtime state: `journal/`, `state.json`,
  `flanders.log`. Created on demand at startup.
- Diagnostic logs go to `.flanders/flanders.log` (file-backed, never stdout, so
  they don't interleave with the TUI).
