# Decision: migration tool

Status: accepted (stage 01, 2026-10-20)

## Decision

Use [goose](https://github.com/pressly/goose) v3 (v3.29.0) as a library. Migrations are plain SQL files in `migrations/`, embedded into the binary with `embed.FS` and applied by `team-memory migrate`, `team-memory serve --migrate`, or `team-memory init`.

## Reasons

- Plain SQL with `-- +goose Up/Down` annotations is read natively by sqlc, so the schema has one source for both migrations and code generation.
- Library use means no separate migration binary in the container or Helm job.
- Supports `StatementBegin/End` for the PL/pgSQL trigger functions this schema relies on (tree derivation, append-only enforcement).

## Conventions

- Migrations are forward-only. Destructive down migrations deliberately raise an error, because dropping revision, event, or audit tables would destroy history. Recovery uses backups (stage 06).
- Later stages add new numbered files. They never edit an applied migration.

## Alternatives considered

- **golang-migrate**: equally capable. Rejected because separate up/down files and a CLI-first workflow add little here.
- **Atlas** (declarative): attractive for diffing. Rejected because it brings a larger toolchain and its declarative model fits poorly with triggers and functions written by hand.
