-- 0005_task_output_url.sql
--
-- S9.5: asset hosting — add `output_url` column to `task` table.
--
-- Per BLUEPRINT.md §S9.5 + ADR-010, every successful generation triggers a
-- background download of upstream URLs into our object storage. Once the
-- asset worker finishes hosting, it stamps `task.output_url` with our CDN
-- URL. The envelope builder (internal/api) reads this column to populate
-- the public Output.URL field — never the upstream-shaped URL.
--
-- Schema notes:
--   - Nullable: tasks in non-terminal state, sync tasks that haven't yet
--     been hosted, and base64-only outputs all leave this NULL.
--   - No index needed: lookups happen by task.id (PK); output_url is read
--     only after a task is found.
--
-- Backwards-compat: pre-existing rows are NULL. The envelope builder
-- treats a NULL output_url for a Succeeded task as "not yet hosted" and
-- surfaces Status=running until the worker stamps it.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE task ADD COLUMN output_url TEXT;
-- +goose StatementEnd


-- +goose Down
-- +goose StatementBegin
ALTER TABLE task DROP COLUMN output_url;
-- +goose StatementEnd
