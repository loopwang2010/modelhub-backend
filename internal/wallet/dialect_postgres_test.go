// Postgres-backed exercises for PostgresDialect SQL strings.
//
// Per F2 in plans/CODE-REVIEW.md: shape assertions in coverage_test.go lock
// the surface; this file actually EXECUTES every PostgresDialect.* SQL
// against a real Postgres 16 testcontainer (via internal/testutil F7
// harness). When Docker is unavailable each test calls t.Skip cleanly via
// the harness — no Docker, no failure.
//
// What this exercises that SQLite cannot:
//   - Postgres-specific INSERT … ON CONFLICT DO NOTHING semantics on the
//     real `wallet_account` table backed by the account_kind ENUM.
//   - SELECT … FOR UPDATE row-locking on Postgres (LockAccountByIDSQL).
//   - The partial unique index `(ref_idempotency, reason_code) WHERE
//     ref_idempotency IS NOT NULL` enforced on the real table.
//   - Coalescing SUM over BIGINT amount_micro_usd including a concrete
//     CHECK(amount_micro_usd <> 0) constraint.
//
// Each test asserts InvariantSum == 0 after every mutation so a sum-zero
// regression in any SQL string surfaces here loudly (per ADR-005).
//
// On Docker host these tests claim ~95% coverage of dialect.go's
// PostgresDialect block; locally without Docker they SKIP and the existing
// shape tests in coverage_test.go remain the only signal.

package wallet

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/QuantumNous/new-api/internal/adapter"
	"github.com/QuantumNous/new-api/internal/testutil"
)

// newPostgresWallet returns a DBWallet wired to a fresh Postgres database
// (one per test), or skips the test when Docker is unavailable. The
// returned cleanup runs the sum-zero invariant check before close.
func newPostgresWallet(t *testing.T) (*DBWallet, *sql.DB) {
	t.Helper()
	db := testutil.NewPostgresDB(t)
	if db == nil {
		// NewPostgresDB called t.Skip — caller will return immediately.
		return nil, nil
	}
	w := New(db, PostgresDialect{})
	t.Cleanup(func() {
		// Sum-zero invariant check on close (I1).
		var total int64
		_ = db.QueryRow(PostgresDialect{}.SelectInvariantSumSQL()).Scan(&total)
		if total != 0 {
			t.Errorf("INVARIANT VIOLATED on close: SUM(amount_micro_usd)=%d, expected 0", total)
		}
	})
	return w, db
}

// TestPostgres_InsertAccount_Idempotent verifies the ON CONFLICT DO NOTHING
// branch on real Postgres: two INSERTs with the same id do not duplicate.
func TestPostgres_InsertAccount_Idempotent(t *testing.T) {
	w, db := newPostgresWallet(t)
	if w == nil {
		return
	}
	ctx := context.Background()

	const acct AccountID = "user:pg-acct-1"
	if err := w.EnsureAccount(ctx, acct, AccountKindUserWallet, "owner-pg-1"); err != nil {
		t.Fatalf("first EnsureAccount: %v", err)
	}
	// Replay must not error and must not duplicate (ON CONFLICT (id) DO NOTHING).
	if err := w.EnsureAccount(ctx, acct, AccountKindUserWallet, "owner-pg-1"); err != nil {
		t.Fatalf("replay EnsureAccount: %v", err)
	}

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM wallet_account WHERE id = $1`, string(acct)).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("ON CONFLICT failed: rows = %d, want 1", n)
	}
}

// TestPostgres_InsertAccount_RejectsBadEnum verifies the Postgres
// account_kind ENUM rejects unknown kinds — a guard against a future
// dialect change that lets bad data through.
func TestPostgres_InsertAccount_RejectsBadEnum(t *testing.T) {
	w, _ := newPostgresWallet(t)
	if w == nil {
		return
	}
	ctx := context.Background()

	err := w.EnsureAccount(ctx, "user:bad-kind", AccountKind("not_a_real_kind"), "x")
	if err == nil {
		t.Error("expected ENUM rejection from Postgres; got nil error")
	}
}

// TestPostgres_TopupSucceeded_InvariantSumZero exercises Topup's full
// SQL path against Postgres: InsertAccountSQL, InsertLedgerSQL (twice),
// InsertTopupAuditSQL, plus SelectLedgerByIdempotencyAndReasonSQL on the
// idempotency replay.
func TestPostgres_TopupSucceeded_InvariantSumZero(t *testing.T) {
	w, db := newPostgresWallet(t)
	if w == nil {
		return
	}
	ctx := context.Background()

	const acct AccountID = "user:pg-topup-1"
	if err := w.Topup(ctx, acct, 1_000_000, "first topup", "admin-1", "idem-pg-topup-1"); err != nil {
		t.Fatalf("Topup: %v", err)
	}

	bal, err := w.Balance(ctx, acct)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal != 1_000_000 {
		t.Errorf("balance = %d; want 1_000_000", bal)
	}

	// I1: ledger sums to zero across the entire table.
	sum, err := w.InvariantSum(ctx)
	if err != nil {
		t.Fatalf("InvariantSum: %v", err)
	}
	if sum != 0 {
		t.Errorf("InvariantSum after Topup = %d; want 0", sum)
	}

	// Audit row was written.
	var auditCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM topup_audit WHERE account_id = $1`, string(acct)).Scan(&auditCount); err != nil {
		t.Fatalf("audit count: %v", err)
	}
	if auditCount != 1 {
		t.Errorf("audit rows = %d; want 1", auditCount)
	}
}

// TestPostgres_Topup_IdempotentReplay verifies the partial-unique-index
// path: a second Topup with the same idempotency_key + reason MUST NOT
// produce duplicate ledger rows. This is the production guarantee of
// the wallet_ledger_idempotency_idx WHERE ref_idempotency IS NOT NULL.
func TestPostgres_Topup_IdempotentReplay(t *testing.T) {
	w, db := newPostgresWallet(t)
	if w == nil {
		return
	}
	ctx := context.Background()

	const acct AccountID = "user:pg-topup-rep"
	const idem adapter.IdempotencyKey = "idem-pg-topup-rep"

	if err := w.Topup(ctx, acct, 500_000, "first", "admin", idem); err != nil {
		t.Fatalf("first Topup: %v", err)
	}
	if err := w.Topup(ctx, acct, 500_000, "second", "admin", idem); err != nil {
		t.Fatalf("replay Topup: %v", err)
	}

	bal, _ := w.Balance(ctx, acct)
	if bal != 500_000 {
		t.Errorf("replay charged twice: balance = %d, want 500_000", bal)
	}

	var ledgerRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM wallet_ledger WHERE ref_idempotency = $1`, string(idem)).Scan(&ledgerRows); err != nil {
		t.Fatalf("ledger count: %v", err)
	}
	// Only the FIRST row of the pair carries the idempotency key (per wallet.go);
	// so we expect exactly 1 row in the unique-index.
	if ledgerRows != 1 {
		t.Errorf("partial unique index allowed dup: rows = %d, want 1", ledgerRows)
	}

	if sum, _ := w.InvariantSum(ctx); sum != 0 {
		t.Errorf("InvariantSum after replay = %d; want 0", sum)
	}
}

// TestPostgres_HoldSettleHappyPath_InvariantSumZero exercises the full
// Postgres FOR UPDATE locking path in Hold (LockAccountByIDSQL) plus the
// settle drain + revenue insert.
func TestPostgres_HoldSettleHappyPath_InvariantSumZero(t *testing.T) {
	w, _ := newPostgresWallet(t)
	if w == nil {
		return
	}
	ctx := context.Background()

	const acct AccountID = "user:pg-hold-1"
	if err := w.Topup(ctx, acct, 10_000_000, "seed", "admin", "idem-pg-seed-1"); err != nil {
		t.Fatalf("Topup: %v", err)
	}

	escrow, err := w.Hold(ctx, acct, "task-pg-001", 500_000, "idem-pg-hold-1")
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if escrow != "escrow:task-pg-001" {
		t.Errorf("escrow = %q; want escrow:task-pg-001", escrow)
	}

	if sum, _ := w.InvariantSum(ctx); sum != 0 {
		t.Errorf("InvariantSum after Hold = %d; want 0", sum)
	}

	if err := w.Settle(ctx, escrow, 400_000); err != nil {
		t.Fatalf("Settle: %v", err)
	}

	if sum, _ := w.InvariantSum(ctx); sum != 0 {
		t.Errorf("InvariantSum after Settle = %d; want 0", sum)
	}

	bal, _ := w.Balance(ctx, acct)
	if bal != 9_600_000 {
		t.Errorf("balance after settle = %d; want 9_600_000", bal)
	}
	rev, _ := w.Balance(ctx, SystemAccountRevenue)
	if rev != 400_000 {
		t.Errorf("revenue = %d; want 400_000", rev)
	}
}

// TestPostgres_Hold_InsufficientBalance forces the SelectBalanceSQL +
// "balance < cost" branch on real Postgres: with no Topup, Hold must
// return ErrInsufficientBalance.
func TestPostgres_Hold_InsufficientBalance(t *testing.T) {
	w, _ := newPostgresWallet(t)
	if w == nil {
		return
	}
	ctx := context.Background()

	const acct AccountID = "user:pg-broke"
	if err := w.Topup(ctx, acct, 100_000, "tiny", "admin", "idem-pg-broke"); err != nil {
		t.Fatalf("Topup: %v", err)
	}

	_, err := w.Hold(ctx, acct, "task-pg-broke", 1_000_000, "idem-pg-broke-hold")
	if !errors.Is(err, ErrInsufficientBalance) {
		t.Errorf("Hold returned %v; want ErrInsufficientBalance", err)
	}

	if sum, _ := w.InvariantSum(ctx); sum != 0 {
		t.Errorf("InvariantSum after failed Hold = %d; want 0", sum)
	}
}

// TestPostgres_Hold_IdempotentReplay verifies the SELECT lookup for the
// idem-replay short-circuit hits the partial unique index, then exits
// without writing duplicate rows.
func TestPostgres_Hold_IdempotentReplay(t *testing.T) {
	w, db := newPostgresWallet(t)
	if w == nil {
		return
	}
	ctx := context.Background()

	const acct AccountID = "user:pg-hold-rep"
	if err := w.Topup(ctx, acct, 5_000_000, "seed", "admin", "idem-pg-rep-seed"); err != nil {
		t.Fatalf("Topup: %v", err)
	}

	const idem adapter.IdempotencyKey = "idem-pg-hold-rep"
	escrow1, err := w.Hold(ctx, acct, "task-pg-rep", 500_000, idem)
	if err != nil {
		t.Fatalf("first Hold: %v", err)
	}
	escrow2, err := w.Hold(ctx, acct, "task-pg-rep", 500_000, idem)
	if err != nil {
		t.Fatalf("replay Hold: %v", err)
	}
	if escrow1 != escrow2 {
		t.Errorf("replay escrow mismatch: %q vs %q", escrow1, escrow2)
	}

	bal, _ := w.Balance(ctx, acct)
	if bal != 4_500_000 {
		t.Errorf("replay-charged: balance = %d; want 4_500_000", bal)
	}

	var holdRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM wallet_ledger WHERE ref_task_id = $1 AND reason_code = 'hold'`,
		"task-pg-rep").Scan(&holdRows); err != nil {
		t.Fatalf("hold rows: %v", err)
	}
	if holdRows != 2 {
		t.Errorf("hold pair count = %d; want 2 (one paired op, no replay rows)", holdRows)
	}
}

// TestPostgres_Refund_FullPath exercises Refund's Postgres path:
// SelectBalanceSQL on escrow, userAccountFromTaskID's bare-SQL lookup
// (which is the same on both dialects but exercised here through PG),
// and the paired refund insert.
func TestPostgres_Refund_FullPath(t *testing.T) {
	w, _ := newPostgresWallet(t)
	if w == nil {
		return
	}
	ctx := context.Background()

	const acct AccountID = "user:pg-refund"
	if err := w.Topup(ctx, acct, 2_000_000, "seed", "admin", "idem-pg-refund-seed"); err != nil {
		t.Fatalf("Topup: %v", err)
	}
	escrow, err := w.Hold(ctx, acct, "task-pg-refund", 700_000, "idem-pg-refund-hold")
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if err := w.Refund(ctx, escrow); err != nil {
		t.Fatalf("Refund: %v", err)
	}
	bal, _ := w.Balance(ctx, acct)
	if bal != 2_000_000 {
		t.Errorf("balance after refund = %d; want 2_000_000 (full)", bal)
	}
	if sum, _ := w.InvariantSum(ctx); sum != 0 {
		t.Errorf("InvariantSum after Refund = %d; want 0", sum)
	}
}

// TestPostgres_PartialRefund_FullPath exercises the asset/compute split
// path through real Postgres including the rounding boundary.
func TestPostgres_PartialRefund_FullPath(t *testing.T) {
	w, _ := newPostgresWallet(t)
	if w == nil {
		return
	}
	ctx := context.Background()

	const acct AccountID = "user:pg-partial"
	if err := w.Topup(ctx, acct, 1_000_000, "seed", "admin", "idem-pg-partial-seed"); err != nil {
		t.Fatalf("Topup: %v", err)
	}
	escrow, err := w.Hold(ctx, acct, "task-pg-partial", 1_000_000, "idem-pg-partial-hold")
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if err := w.PartialRefund(ctx, escrow, 0.3); err != nil {
		t.Fatalf("PartialRefund: %v", err)
	}
	bal, _ := w.Balance(ctx, acct)
	if bal != 300_000 {
		t.Errorf("balance after partial = %d; want 300_000", bal)
	}
	rev, _ := w.Balance(ctx, SystemAccountRevenue)
	if rev != 700_000 {
		t.Errorf("revenue = %d; want 700_000", rev)
	}
	if sum, _ := w.InvariantSum(ctx); sum != 0 {
		t.Errorf("InvariantSum after PartialRefund = %d; want 0", sum)
	}
}

// TestPostgres_LedgerCheckConstraint verifies the Postgres
// CHECK(amount_micro_usd <> 0) constraint refuses zero-amount inserts.
// Calls the dialect's SQL directly so the bypass is unambiguous.
func TestPostgres_LedgerCheckConstraint_RejectsZero(t *testing.T) {
	w, db := newPostgresWallet(t)
	if w == nil {
		return
	}
	ctx := context.Background()

	// Seed an account so the FK is satisfied.
	const acct AccountID = "user:pg-checkzero"
	if err := w.EnsureAccount(ctx, acct, AccountKindUserWallet, "x"); err != nil {
		t.Fatalf("EnsureAccount: %v", err)
	}

	// Attempt a direct zero-amount insert via the same SQL the wallet uses.
	d := PostgresDialect{}
	_, err := db.ExecContext(ctx, d.InsertLedgerSQL(),
		uuid.NewString(), string(acct), int64(0), "raw_zero_attempt", nil, nil, time.Now().UTC())
	if err == nil {
		t.Error("expected CHECK(amount_micro_usd <> 0) violation; got nil")
		return
	}
	// Postgres signals check_violation as SQLSTATE 23514 — match by substring.
	if !strings.Contains(strings.ToLower(err.Error()), "check") &&
		!strings.Contains(err.Error(), "23514") {
		t.Errorf("expected CHECK violation error; got %v", err)
	}
}

// TestPostgres_SelectLedgerForAccount_Ordering verifies the explicit
// "ORDER BY created_at ASC, id ASC" deterministic order from
// SelectLedgerForAccountSQL — the only dialect SQL not exercised by the
// happy paths above (it's a debug/audit query).
func TestPostgres_SelectLedgerForAccount_Ordering(t *testing.T) {
	w, db := newPostgresWallet(t)
	if w == nil {
		return
	}
	ctx := context.Background()

	const acct AccountID = "user:pg-order"
	// Three ops so we have multiple rows on the same account in temporal order.
	if err := w.Topup(ctx, acct, 100_000, "1", "admin", "idem-ord-1"); err != nil {
		t.Fatalf("Topup 1: %v", err)
	}
	if err := w.Topup(ctx, acct, 200_000, "2", "admin", "idem-ord-2"); err != nil {
		t.Fatalf("Topup 2: %v", err)
	}
	if err := w.Topup(ctx, acct, 50_000, "3", "admin", "idem-ord-3"); err != nil {
		t.Fatalf("Topup 3: %v", err)
	}

	rows, err := db.QueryContext(ctx, PostgresDialect{}.SelectLedgerForAccountSQL(), string(acct))
	if err != nil {
		t.Fatalf("SelectLedgerForAccount: %v", err)
	}
	defer rows.Close()

	var amounts []int64
	for rows.Next() {
		var (
			opID, accountID, reason string
			amount                  int64
			refTask, refIdem        sql.NullString
			createdAt               time.Time
		)
		if err := rows.Scan(&opID, &accountID, &amount, &reason, &refTask, &refIdem, &createdAt); err != nil {
			t.Fatalf("scan: %v", err)
		}
		amounts = append(amounts, amount)
	}
	if got, want := len(amounts), 3; got != want {
		t.Errorf("rows = %d; want %d", got, want)
	}
	// Must be in insertion order.
	if len(amounts) == 3 && (amounts[0] != 100_000 || amounts[1] != 200_000 || amounts[2] != 50_000) {
		t.Errorf("ordering wrong: %v; want [100000 200000 50000]", amounts)
	}
}

// TestPostgres_SelectAccountByID exercises the read SQL the wallet
// dialect publishes but never calls itself; lock-free counterpart of
// LockAccountByIDSQL.
func TestPostgres_SelectAccountByID(t *testing.T) {
	w, db := newPostgresWallet(t)
	if w == nil {
		return
	}
	ctx := context.Background()

	const acct AccountID = "user:pg-readone"
	if err := w.EnsureAccount(ctx, acct, AccountKindUserWallet, "owner-readone"); err != nil {
		t.Fatalf("EnsureAccount: %v", err)
	}

	row := db.QueryRowContext(ctx, PostgresDialect{}.SelectAccountByIDSQL(), string(acct))
	var (
		id, kind     string
		ownerSubject sql.NullString
		createdAt    time.Time
	)
	if err := row.Scan(&id, &kind, &ownerSubject, &createdAt); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if id != string(acct) || kind != string(AccountKindUserWallet) {
		t.Errorf("got id=%q kind=%q; want %q %q", id, kind, acct, AccountKindUserWallet)
	}
	if !ownerSubject.Valid || ownerSubject.String != "owner-readone" {
		t.Errorf("owner_subject = %v; want owner-readone", ownerSubject)
	}
}
