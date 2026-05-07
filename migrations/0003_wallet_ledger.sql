-- 0003_wallet_ledger.sql
--
-- S6: credit wallet — double-entry ledger.
--
-- Per S6-WALLET-DESIGN.md (and ADR-005, ADR-013):
--   - The ledger is the only source of truth for balance — no mutable
--     `balance` column lives anywhere. `wallet_balance` view derives.
--   - Every wallet operation writes exactly two ledger rows in a single
--     transaction. They share an `operation_id` and sum to zero.
--   - `account_id` is opaque text typed in Go as adapter.AccountID
--     (ADR-013). Today every user_wallet has id "user:<numeric>" but
--     adding orgs / promotions / refunds later is just a new id-prefix.
--   - Sum-zero invariant (I1) checked continuously by tests + the
--     `wallet_invariant_check` view in production monitoring.
--
-- topup_audit holds compliance metadata (who issued the top-up, what
-- note) — kept separate from wallet_ledger so the ledger stays a clean
-- canonical state-of-the-world and doesn't grow PII columns.

-- +goose Up
-- +goose StatementBegin
CREATE TYPE account_kind AS ENUM (
    'user_wallet',
    'task_escrow',
    'modelhub_revenue',
    'external_topup'
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS wallet_account (
    id             TEXT PRIMARY KEY,
    kind           account_kind NOT NULL,
    owner_subject  TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS wallet_account_kind_idx
    ON wallet_account (kind);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS wallet_account_owner_idx
    ON wallet_account (owner_subject);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS wallet_ledger (
    id                BIGSERIAL PRIMARY KEY,
    operation_id      UUID NOT NULL,
    account_id        TEXT NOT NULL REFERENCES wallet_account(id),
    amount_micro_usd  BIGINT NOT NULL,
    reason_code       TEXT NOT NULL,
    ref_task_id       TEXT,
    ref_idempotency   TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT amount_nonzero CHECK (amount_micro_usd <> 0)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS wallet_ledger_op_idx
    ON wallet_ledger (operation_id);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS wallet_ledger_account_idx
    ON wallet_ledger (account_id, created_at DESC);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS wallet_ledger_task_idx
    ON wallet_ledger (ref_task_id)
    WHERE ref_task_id IS NOT NULL;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE UNIQUE INDEX IF NOT EXISTS wallet_ledger_idempotency_idx
    ON wallet_ledger (ref_idempotency, reason_code)
    WHERE ref_idempotency IS NOT NULL;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE IF NOT EXISTS topup_audit (
    id              BIGSERIAL PRIMARY KEY,
    op_id           UUID NOT NULL,
    account_id      TEXT NOT NULL REFERENCES wallet_account(id),
    amount          BIGINT NOT NULL,
    note            TEXT,
    admin_user_id   TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS topup_audit_account_idx
    ON topup_audit (account_id, created_at DESC);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX IF NOT EXISTS topup_audit_admin_idx
    ON topup_audit (admin_user_id, created_at DESC);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE VIEW wallet_balance AS
    SELECT account_id, SUM(amount_micro_usd) AS balance_micro_usd
      FROM wallet_ledger
     GROUP BY account_id;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE VIEW wallet_invariant_check AS
    SELECT SUM(amount_micro_usd) AS total_must_be_zero,
           COUNT(*) AS row_count
      FROM wallet_ledger;
-- +goose StatementEnd


-- +goose Down
-- +goose StatementBegin
DROP VIEW IF EXISTS wallet_invariant_check;
-- +goose StatementEnd

-- +goose StatementBegin
DROP VIEW IF EXISTS wallet_balance;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS topup_audit;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS wallet_ledger;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS wallet_account;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TYPE IF EXISTS account_kind;
-- +goose StatementEnd
