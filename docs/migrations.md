# Migrations (ADR-008)

modelhub uses two coexisting migration systems. This document explains the
split, when each applies, and how to add a migration.

## TL;DR

- **New modelhub tables** (provider catalog, task lifecycle, wallet ledger,
  asset registry, ...): use `goose` + raw SQL files in `migrations/`.
- **Inherited new-api tables** (users, tokens, redemptions, channels, ...):
  continue to be created and evolved via GORM `AutoMigrate` in `model/`.
- The two systems do not collide because they touch different tables.

## Why the split?

Inherited new-api code uses GORM's `AutoMigrate` to add columns and indexes
to its own tables. AutoMigrate is convenient but cannot:

- emit DOWN scripts (no rollback story)
- pin a schema to an exact version
- be reviewed by a DBA before applying in production
- safely apply multi-step migrations that include data backfills

Sprint 1 introduces wallet ledger semantics where rollbackable, audit-grade
schema changes matter. We chose `pressly/goose` for those tables because:

- declarative SQL files in `migrations/NNNN_<name>.sql`
- explicit `-- +goose Up` and `-- +goose Down` markers
- linear version numbering with collision detection
- standard `up`, `down`, `redo`, `status`, `version` subcommands
- well-supported across postgres, mysql, sqlite

We do **not** rip out GORM AutoMigrate for the inherited tables — touching
that code is out of scope for Sprint 1, and the new-api upstream still
expects it.

## Policy

| Table | System | Notes |
|---|---|---|
| `users`, `tokens`, `redemptions`, `channels`, `logs`, ... | GORM AutoMigrate | Owned by upstream new-api; keep AutoMigrate |
| `provider` (renamed from `channel` in S3) | goose | New schema |
| `task`, `task_event` (S5) | goose | Wallet-coupled |
| `wallet_ledger`, `wallet_account` (S6) | goose | Audit-grade; needs DOWN |
| `asset` (S9.5) | goose | New schema |

If you find yourself adding a column to an inherited table, prefer GORM
AutoMigrate to stay consistent with the rest of that file. If you're adding
a *new* table, use goose.

## Filename convention

```
migrations/
  0001_baseline.sql
  0002_provider_table.sql                 (S3)
  0003_task_lifecycle.sql                 (S5)
  0004_wallet_ledger.sql                  (S6)
  ...
```

- 4-digit zero-padded version, monotonically increasing
- snake_case description after the version
- `.sql` extension (Go-based migrations are not used here)

`migrate fix` will renumber if you accidentally collide.

## Local development

Set `DATABASE_URL` to your local Postgres (the `.env.example` ships a default
that points at the docker-compose service):

```bash
export DATABASE_URL='postgres://newapi:newapi@localhost:5432/newapi?sslmode=disable'

make migrate-status   # show what's applied
make migrate-up       # apply pending migrations
make migrate-down     # roll back the latest
```

`make migrate-create NAME=add_foo_index` scaffolds a new file with both
`Up` and `Down` blocks pre-filled.

## Driver selection

The default is `pgx` (which talks postgres). Override via
`MIGRATE_DRIVER=mysql` or `MIGRATE_DRIVER=sqlite3` when running tests
against a non-postgres database.

## Verifying a roundtrip

A migration is not done until `up → down → up` produces the same schema
as `up` alone. The Sprint-1 CI gate runs:

```bash
make migrate-up
make migrate-down
make migrate-up
psql "$DATABASE_URL" -c '\dt'   # smoke check
```

## What goose does NOT do

- Generate Go code (use sqlc/sqlboiler/etc. separately if you want that)
- Manage data migrations longer than a single transaction (split into
  multiple sequential migrations)
- Coordinate with multiple replicas mid-deploy (run migrations from one
  shell on one node before rolling out)

## Failure modes to watch

- A migration that depends on data already present in production but absent
  in dev fixtures will pass locally and fail in production. Always seed dev
  with realistic shapes before merging.
- `down` scripts that aren't tested rot silently. Pair every `Up` with a
  `Down` that's exercised at least once in CI.
- Long-running `ALTER TABLE` on Postgres with the default lock will block
  reads/writes. For prod, prefer `ALTER ... NOT VALID` + `VALIDATE
  CONSTRAINT` or pgrollup-style two-phase migrations.
