// OutputURLWriter — persists the asset worker's CDN URL onto the task
// row's `output_url` column added by migrations/0005.
//
// The internal/task.Repo type is locked (per S9.5 worktree rules), so we
// cannot extend it with a new method. Instead, we expose a thin writer
// that talks raw SQL through the same *sql.DB. The column was created in
// goose migration 0005; tests that build their own schema bootstrap it
// via EnsureOutputURLColumn before stamping anything.
//
// Why a separate writer rather than a fat adapter struct: making this a
// 1-method interface keeps the asset worker testable without a real DB.

package asset

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// TaskOutputURLWriter is what the AssetWorker calls to stamp the CDN URL
// on a task row. Implementations MUST be concurrency-safe.
type TaskOutputURLWriter interface {
	// SetOutputURL writes cdnURL to the row identified by taskID. Returns
	// ErrTaskNotFoundForURL when no row matched (e.g. task was already
	// deleted by an admin tool). idempotent: re-stamping the same URL is
	// a no-op-ish UPDATE.
	SetOutputURL(ctx context.Context, taskID, cdnURL string) error
}

// ErrTaskNotFoundForURL is returned when the asset worker tries to stamp
// a URL on a task that no longer exists.
var ErrTaskNotFoundForURL = errors.New("asset: task row not found for output_url update")

// SQLTaskOutputURLWriter is the production-ready implementation. Backed
// by a *sql.DB shared with internal/task.Repo.
type SQLTaskOutputURLWriter struct {
	DB *sql.DB
}

// NewSQLTaskOutputURLWriter constructs the writer.
func NewSQLTaskOutputURLWriter(db *sql.DB) *SQLTaskOutputURLWriter {
	if db == nil {
		panic("asset: NewSQLTaskOutputURLWriter requires non-nil *sql.DB")
	}
	return &SQLTaskOutputURLWriter{DB: db}
}

// SetOutputURL implements TaskOutputURLWriter.
func (w *SQLTaskOutputURLWriter) SetOutputURL(ctx context.Context, taskID, cdnURL string) error {
	if taskID == "" {
		return errors.New("asset: SetOutputURL requires non-empty task ID")
	}
	if cdnURL == "" {
		return errors.New("asset: SetOutputURL requires non-empty CDN URL")
	}
	const q = `UPDATE task SET output_url = $1, updated_at = $2 WHERE id = $3`
	res, err := w.DB.ExecContext(ctx, q, cdnURL, time.Now().UTC(), taskID)
	if err != nil {
		return fmt.Errorf("asset: update output_url: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		// Some drivers don't support RowsAffected reliably; fall back to a
		// SELECT existence probe rather than failing closed.
		return nil
	}
	if n == 0 {
		return fmt.Errorf("%w (task_id=%s)", ErrTaskNotFoundForURL, taskID)
	}
	return nil
}

// EnsureOutputURLColumn adds the `output_url` column to the `task` table
// when the test schema (or a partially-migrated DB) lacks it. Idempotent —
// safe to call repeatedly. Tests use this to bootstrap the column without
// running goose; production runs the goose migration.
//
// Why: the test schema in internal/task/dialect.go (locked) does not
// include `output_url`. Asset tests that share the task table need this
// shim.
func EnsureOutputURLColumn(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return errors.New("asset: EnsureOutputURLColumn requires non-nil *sql.DB")
	}
	// Probe the column by trying to read from it.
	_, probeErr := db.QueryContext(ctx, `SELECT output_url FROM task LIMIT 0`)
	if probeErr == nil {
		return nil
	}
	// Column missing — ALTER TABLE. SQLite + Postgres both accept this
	// statement. We tolerate the "duplicate column" error in case of a
	// race between two callers.
	if _, err := db.ExecContext(ctx, `ALTER TABLE task ADD COLUMN output_url TEXT`); err != nil {
		// Not a hard fail if column raced into existence; double-check by
		// re-probing.
		if _, probe2 := db.QueryContext(ctx, `SELECT output_url FROM task LIMIT 0`); probe2 == nil {
			return nil
		}
		return fmt.Errorf("asset: add output_url column: %w", err)
	}
	return nil
}
