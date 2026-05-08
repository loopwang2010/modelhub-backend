// Smoke tests for the Postgres testcontainer harness.
//
// These tests verify NewPostgresDB:
//   1. Spins up a Postgres 16 testcontainer (or skips cleanly when Docker is
//      unavailable so dev/CI machines without Docker stay green).
//   2. Applies all 5 goose migrations from ./migrations/ in version order.
//   3. Creates the expected wallet + task tables.
//   4. Cleans up automatically via t.Cleanup.
//
// Per F7 in plans/CODE-REVIEW.md, this harness is consumed by T4/T5 to lift
// internal/wallet and internal/task to ~95% coverage. Smoke coverage here
// is intentionally narrow — exercising the harness contract, not the SQL
// strings under test.

package testutil_test

import (
	"context"
	"database/sql"
	"sort"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/internal/testutil"
)

// TestNewPostgresDB_AppliesAllMigrations is the headline smoke test:
// spin up a container, run the harness, verify the goose version table
// shows every shipped migration applied in order.
func TestNewPostgresDB_AppliesAllMigrations(t *testing.T) {
	db := testutil.NewPostgresDB(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rows, err := db.QueryContext(ctx, `
		SELECT version_id
		  FROM goose_db_version
		 WHERE version_id > 0
		 ORDER BY version_id ASC
	`)
	if err != nil {
		t.Fatalf("query goose_db_version: %v", err)
	}
	defer rows.Close()

	var got []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan version: %v", err)
		}
		got = append(got, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	want := []int64{1, 2, 3, 4, 5}
	if !sort.SliceIsSorted(got, func(i, j int) bool { return got[i] < got[j] }) {
		t.Errorf("goose_db_version not sorted: %v", got)
	}
	if !equalInt64Slices(got, want) {
		t.Errorf("applied migrations = %v, want %v", got, want)
	}
}

// TestNewPostgresDB_TablesExist verifies that the schema produced by
// applying the migrations exposes the tables wallet/ and task/ packages
// will exercise. F7 calls this out specifically: tasks, wallet_account,
// wallet_ledger, topup_audit, plus the channels stub the harness creates
// so 0003_channel_modality.sql can ALTER it.
func TestNewPostgresDB_TablesExist(t *testing.T) {
	db := testutil.NewPostgresDB(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	want := []string{
		"task",
		"task_event",
		"wallet_account",
		"wallet_ledger",
		"topup_audit",
		"channels",
	}
	for _, name := range want {
		if !tableExists(ctx, t, db, name) {
			t.Errorf("expected table %q to exist after migrations", name)
		}
	}
}

// TestNewPostgresDB_IsolatedPerTest verifies that two concurrent calls to
// NewPostgresDB get logically isolated databases — a row written via one
// handle must not be visible via another. This is the contract that lets
// callers (T4/T5) skip per-test truncation.
func TestNewPostgresDB_IsolatedPerTest(t *testing.T) {
	dbA := testutil.NewPostgresDB(t)
	dbB := testutil.NewPostgresDB(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := dbA.ExecContext(ctx, `
		INSERT INTO wallet_account (id, kind, owner_subject)
		VALUES ('user:isolation-test', 'user_wallet', 'isolation-test')
	`); err != nil {
		t.Fatalf("insert into dbA: %v", err)
	}

	var count int
	if err := dbB.QueryRowContext(ctx, `
		SELECT COUNT(*)
		  FROM wallet_account
		 WHERE id = 'user:isolation-test'
	`).Scan(&count); err != nil {
		t.Fatalf("query dbB: %v", err)
	}
	if count != 0 {
		t.Errorf("isolation broken: dbB sees %d rows from dbA, want 0", count)
	}
}

// TestTruncateAllTables verifies the helper resets user data without
// dropping the schema, so later cases in the same test can start clean.
func TestTruncateAllTables(t *testing.T) {
	db := testutil.NewPostgresDB(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := db.ExecContext(ctx, `
		INSERT INTO wallet_account (id, kind, owner_subject)
		VALUES ('user:truncate-test', 'user_wallet', 'truncate-test')
	`); err != nil {
		t.Fatalf("seed wallet_account: %v", err)
	}

	testutil.TruncateAllTables(t, db)

	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM wallet_account`).Scan(&count); err != nil {
		t.Fatalf("count after truncate: %v", err)
	}
	if count != 0 {
		t.Errorf("after TruncateAllTables wallet_account has %d rows, want 0", count)
	}

	// Schema must still be intact: goose history survives, table still exists.
	if !tableExists(ctx, t, db, "wallet_account") {
		t.Errorf("schema was dropped by TruncateAllTables")
	}
	var goose int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM goose_db_version WHERE version_id > 0`).Scan(&goose); err != nil {
		t.Fatalf("count goose history: %v", err)
	}
	if goose != 5 {
		t.Errorf("goose history rows = %d, want 5 (Truncate must skip goose_db_version)", goose)
	}
}

// ─── helpers ─────────────────────────────────────────────────────────────

func tableExists(ctx context.Context, t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var exists bool
	err := db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			  FROM information_schema.tables
			 WHERE table_schema = 'public'
			   AND table_name = $1
		)
	`, name).Scan(&exists)
	if err != nil {
		t.Fatalf("check table %q: %v", name, err)
	}
	return exists
}

func equalInt64Slices(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
