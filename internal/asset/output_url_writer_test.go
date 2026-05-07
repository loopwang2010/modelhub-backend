package asset

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	_ "modernc.org/sqlite"
)

var dbCounter atomic.Int64

func newTaskTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:asset_db_%d?mode=memory&cache=shared", dbCounter.Add(1))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = db.Close() })

	// Minimal task table — only the columns SetOutputURL touches. The
	// real production schema lives in migrations/0002 (locked) +
	// migrations/0005 (S9.5).
	if _, err := db.Exec(`
		CREATE TABLE task (
			id TEXT PRIMARY KEY,
			account_id TEXT NOT NULL DEFAULT '',
			updated_at DATETIME NOT NULL,
			output_url TEXT
		)
	`); err != nil {
		t.Fatalf("create task: %v", err)
	}
	return db
}

func TestSQLTaskOutputURLWriter_Roundtrip(t *testing.T) {
	t.Parallel()
	db := newTaskTestDB(t)
	if _, err := db.Exec(`INSERT INTO task (id, updated_at) VALUES ('gen_1', '2026-01-01')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	w := NewSQLTaskOutputURLWriter(db)
	if err := w.SetOutputURL(context.Background(), "gen_1", "https://cdn.modelhub.local/outputs/gen_1/abc.png"); err != nil {
		t.Fatalf("set: %v", err)
	}
	var got sql.NullString
	if err := db.QueryRow(`SELECT output_url FROM task WHERE id = 'gen_1'`).Scan(&got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !got.Valid || got.String != "https://cdn.modelhub.local/outputs/gen_1/abc.png" {
		t.Errorf("output_url: %v", got)
	}
}

func TestSQLTaskOutputURLWriter_RequiresArgs(t *testing.T) {
	t.Parallel()
	db := newTaskTestDB(t)
	w := NewSQLTaskOutputURLWriter(db)
	if err := w.SetOutputURL(context.Background(), "", "x"); err == nil {
		t.Error("empty taskID should error")
	}
	if err := w.SetOutputURL(context.Background(), "x", ""); err == nil {
		t.Error("empty url should error")
	}
}

func TestSQLTaskOutputURLWriter_NotFound(t *testing.T) {
	t.Parallel()
	db := newTaskTestDB(t)
	w := NewSQLTaskOutputURLWriter(db)
	err := w.SetOutputURL(context.Background(), "nope-id", "https://cdn.modelhub.local/x")
	if !errors.Is(err, ErrTaskNotFoundForURL) {
		t.Errorf("want ErrTaskNotFoundForURL, got %v", err)
	}
}

func TestSQLTaskOutputURLWriter_PanicsOnNilDB(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic")
		}
	}()
	_ = NewSQLTaskOutputURLWriter(nil)
}

func TestEnsureOutputURLColumn_AlreadyExists(t *testing.T) {
	t.Parallel()
	db := newTaskTestDB(t)
	// Column was already created by newTaskTestDB.
	if err := EnsureOutputURLColumn(context.Background(), db); err != nil {
		t.Errorf("ensure (existing): %v", err)
	}
}

func TestEnsureOutputURLColumn_AddsColumn(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("sqlite", fmt.Sprintf("file:ensure_db_%d?mode=memory&cache=shared", dbCounter.Add(1)))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Exec(`CREATE TABLE task (id TEXT PRIMARY KEY, updated_at DATETIME NOT NULL)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := EnsureOutputURLColumn(context.Background(), db); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	// Now the column exists — re-call must be a no-op.
	if err := EnsureOutputURLColumn(context.Background(), db); err != nil {
		t.Fatalf("ensure (again): %v", err)
	}
	// Confirm we can write to it.
	if _, err := db.Exec(`INSERT INTO task (id, updated_at, output_url) VALUES ('x', 'now', 'http://y')`); err != nil {
		t.Errorf("write: %v", err)
	}
}

func TestEnsureOutputURLColumn_RequiresDB(t *testing.T) {
	t.Parallel()
	if err := EnsureOutputURLColumn(context.Background(), nil); err == nil {
		t.Error("nil db: expected error")
	}
}
