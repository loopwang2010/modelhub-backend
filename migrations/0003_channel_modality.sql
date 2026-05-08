-- 0003_channel_modality.sql
--
-- S3 (BLUEPRINT.md §S3): add `modality` and `task_kind` columns to the
-- inherited `channels` table so modelhub can route image/video/edit
-- workloads alongside the inherited LLM /v1/chat/completions flow.
--
-- ─── Coordination with GORM AutoMigrate (per docs/migrations.md) ──────────
-- The `channels` table is OWNED by GORM AutoMigrate (it ships with the
-- inherited new-api codebase). docs/migrations.md §Policy says inherited
-- tables stay on AutoMigrate; we are bending that rule narrowly here so
-- that:
--
--   1. We get a reviewable, rollbackable schema change in goose's history,
--      matching the audit-grade requirement of the wallet ledger work
--      that lands in S6.
--   2. Production deploys can apply the schema change BEFORE the new
--      binary boots (goose is invoked from cmd/migrate as a pre-deploy
--      step), avoiding a window where a freshly-deployed binary writes
--      to a column GORM AutoMigrate has not yet added.
--
-- The migration is intentionally idempotent: `ADD COLUMN IF NOT EXISTS`
-- means it is safe to run after GORM AutoMigrate has already added the
-- columns (because Channel struct now declares them). The reverse is
-- also true — if goose runs first, GORM's column-shape inspection at
-- boot will detect a match and skip the ALTER.
--
-- ─── Defaults and back-compat ─────────────────────────────────────────────
-- BLUEPRINT.md §S3 calls for "sensible defaults so existing rows migrate
-- cleanly":
--
--   modality   = 'llm'        — the inherited new-api code only ever
--                                modeled LLM channels.
--   task_kind  = 'streaming'  — /v1/chat/completions is streaming-first.
--
-- Both columns are NOT NULL with literal defaults so any pre-existing
-- channel row gets a valid value automatically. This matches the GORM
-- tag declarations on model.Channel.{Modality,TaskKind}.
--
-- ─── Indexes ──────────────────────────────────────────────────────────────
-- The admin UI and the future `/v1/models` endpoint both filter by
-- modality (e.g., "show me only image-modality providers"); a btree
-- index on each column is cheap and covers the common cardinality
-- (5 modalities, 3 task kinds).

-- +goose Up
-- +goose StatementBegin
ALTER TABLE channels
    ADD COLUMN IF NOT EXISTS modality VARCHAR(32) NOT NULL DEFAULT 'llm';
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE channels
    ADD COLUMN IF NOT EXISTS task_kind VARCHAR(32) NOT NULL DEFAULT 'streaming';
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS channels_modality_idx
    ON channels (modality);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS channels_task_kind_idx
    ON channels (task_kind);
-- +goose StatementEnd


-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS channels_task_kind_idx;
-- +goose StatementEnd

-- +goose StatementBegin
DROP INDEX IF EXISTS channels_modality_idx;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE channels
    DROP COLUMN IF EXISTS task_kind;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE channels
    DROP COLUMN IF EXISTS modality;
-- +goose StatementEnd
