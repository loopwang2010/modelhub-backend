-- 0001_baseline.sql
--
-- A no-op baseline migration whose only purpose is to confirm the goose
-- toolchain is wired correctly: `migrate up` applies it and writes one row
-- to goose_db_version; `migrate down` removes it.
--
-- New product tables (provider, task, wallet ledger) ship in S3, S5, and S6
-- respectively, each in its own NNNN_<name>.sql file. Existing inherited
-- new-api tables continue to be created via GORM AutoMigrate; see
-- docs/migrations.md for the split policy.

-- +goose Up
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
SELECT 1;
-- +goose StatementEnd
