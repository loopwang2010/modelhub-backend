-- 0002_task_runtime_columns.sql
--
-- S5: async task runtime columns + task_event audit table.
--
-- Per S5-WORKER-DESIGN.md §2 — adds the runtime fields the worker pool,
-- webhook receiver, and reconciler need on top of the S2.5 baseline. The
-- S2.5 baseline never created the task table itself (it was a no-op
-- baseline), so this migration creates `task` AND adds runtime columns in
-- the same step.
--
-- Why this combined shape: the BLUEPRINT.md S5 task list calls out the
-- task row's columns in §S5 Tasks #1, and S5-WORKER-DESIGN.md §2 calls
-- out the runtime additions. There's no intermediate migration between
-- baseline and S5 that would have created `task`, so we land everything
-- here.
--
-- Indexes per S5-WORKER-DESIGN.md §2:
--   - task_worker_claim_idx — covers the worker's SKIP-LOCKED claim query
--   - task_reconciler_idx   — covers the reconciler's stuck/timeout sweep
--   - task_webhook_idx      — O(1) webhook lookup by token (per AP-18)
--   - task_idempotency_idx  — unique partial index for INSERT-ON-CONFLICT
--                             dedup (per S5-WORKER-DESIGN.md §8 / ADR-006)
--
-- task_event is append-only audit. Every state transition writes one row.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS task (
    id                TEXT PRIMARY KEY,
    account_id        TEXT NOT NULL,
    model_key         TEXT NOT NULL,
    provider_key      TEXT NOT NULL,
    state             TEXT NOT NULL,
    params_json       JSONB NOT NULL,
    idempotency_key   TEXT,
    upstream_ref      TEXT,
    polling_url       TEXT,
    poll_attempt      INT NOT NULL DEFAULT 0,
    next_poll_after   TIMESTAMPTZ,
    webhook_token     TEXT NOT NULL,
    sla_deadline      TIMESTAMPTZ NOT NULL,
    last_error_class  TEXT,
    raw_error         BYTEA,
    held_amount       BIGINT NOT NULL DEFAULT 0,
    actual_cost       BIGINT NOT NULL DEFAULT 0,
    submitted_at      TIMESTAMPTZ,
    terminal_at       TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS task_worker_claim_idx
    ON task (provider_key, state, next_poll_after)
    WHERE state IN ('submitted', 'running');
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS task_reconciler_idx
    ON task (state, sla_deadline)
    WHERE state IN ('held', 'submitted', 'running');
-- +goose StatementEnd

-- +goose StatementBegin
CREATE UNIQUE INDEX IF NOT EXISTS task_webhook_idx
    ON task (webhook_token);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE UNIQUE INDEX IF NOT EXISTS task_idempotency_idx
    ON task (idempotency_key)
    WHERE idempotency_key IS NOT NULL;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS task_event (
    id          BIGSERIAL PRIMARY KEY,
    task_id     TEXT NOT NULL REFERENCES task(id),
    from_state  TEXT NOT NULL,
    to_state    TEXT NOT NULL,
    reason      TEXT,
    metadata    JSONB,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS task_event_task_idx
    ON task_event (task_id, created_at);
-- +goose StatementEnd


-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS task_event;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS task;
-- +goose StatementEnd
