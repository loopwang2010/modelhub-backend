// SERIALIZABLE retry path tests.
//
// runTx in tx.go opens transactions with sql.LevelSerializable on Postgres
// and retries (up to maxTxRetries=5) on SQLSTATE 40001 / 40P01 / "could
// not serialize access". Per F2 in plans/CODE-REVIEW.md the retry path is
// "partial" coverage — the synthetic isRetryableTxError table tests don't
// confirm that a real Postgres serialization conflict actually triggers
// a retry.
//
// What this exercises:
//   - sql.LevelSerializable BeginTx on real Postgres (unique to PG).
//   - Concurrent Hold operations on the same account → SERIALIZABLE
//     conflict → 40001 error returned by the SECOND-to-commit goroutine
//     → runTx classifies as retryable → retry loop drives both to success.
//   - The double-spend safety: even with concurrency only one Hold gets
//     to charge while the other either retries successfully (account had
//     enough funds) or fails with ErrInsufficientBalance (didn't).
//   - InvariantSum == 0 after every concurrent op so any retry that
//     accidentally doubled a write would be caught.
//
// All tests skip cleanly when Docker is unavailable.

package wallet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/internal/adapter"
)

// TestPostgresTx_RunsAtSerializable verifies that runTx on PostgresDialect
// actually opens the transaction at SERIALIZABLE. Reads
// `SHOW transaction_isolation` from inside a tx that uses txOptionsFor
// (the same options runTx would pass to BeginTx).
func TestPostgresTx_RunsAtSerializable(t *testing.T) {
	w, _ := newPostgresWallet(t)
	if w == nil {
		return
	}
	ctx := context.Background()

	var observed string
	err := runTx(ctx, w.db, w.dialect, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SHOW transaction_isolation`).Scan(&observed)
	})
	if err != nil {
		t.Fatalf("runTx: %v", err)
	}
	if observed != "serializable" {
		t.Errorf("isolation level = %q; want serializable", observed)
	}
}

// TestPostgresTx_ConcurrentHolds_BothEventuallySucceed spawns 2 goroutines
// that each Hold against the same well-funded account. With SERIALIZABLE
// isolation Postgres will throw 40001 on at least one of them; runTx's
// retry loop must observe that and re-run, so both calls eventually
// succeed (the account has enough balance to cover both holds).
//
// If the retry path were broken, one of the goroutines would receive a
// raw Postgres "could not serialize access" error instead of a clean
// success, and InvariantSum would still hit zero only if both writes
// actually committed.
func TestPostgresTx_ConcurrentHolds_BothEventuallySucceed(t *testing.T) {
	w, _ := newPostgresWallet(t)
	if w == nil {
		return
	}
	ctx := context.Background()

	const acct AccountID = "user:pg-concurrent"
	if err := w.Topup(ctx, acct, 5_000_000, "seed", "admin", "idem-pg-conc-seed"); err != nil {
		t.Fatalf("Topup: %v", err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			taskID := fmt.Sprintf("task-pg-conc-%d", idx)
			idem := adapter.IdempotencyKey(fmt.Sprintf("idem-pg-conc-%d", idx))
			_, errs[idx] = w.Hold(ctx, acct, taskID, 1_000_000, idem)
		}(i)
	}
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Errorf("goroutine %d: %v (retry path failed to recover from serialization conflict)", i, e)
		}
	}

	// Balance: 5 - 1 - 1 = 3 → 3_000_000.
	bal, _ := w.Balance(ctx, acct)
	if bal != 3_000_000 {
		t.Errorf("balance = %d; want 3_000_000 (one of the holds was lost or doubled)", bal)
	}
	if sum, _ := w.InvariantSum(ctx); sum != 0 {
		t.Errorf("InvariantSum after concurrent holds = %d; want 0", sum)
	}
}

// TestPostgresTx_ConcurrentHoldsExceedBalance_OneSucceedsOneFails
// drives the contention case where the two holds collectively can't fit:
// account has $1.50, both goroutines try to hold $1.00. Exactly one
// must succeed; the other must observe ErrInsufficientBalance after the
// retry settles. NEVER both succeed (would oversubscribe the wallet);
// NEVER both fail with retry-exhausted (would mean the retry path
// drops legitimate work).
func TestPostgresTx_ConcurrentHoldsExceedBalance_OneSucceedsOneFails(t *testing.T) {
	w, _ := newPostgresWallet(t)
	if w == nil {
		return
	}
	ctx := context.Background()

	const acct AccountID = "user:pg-tight"
	if err := w.Topup(ctx, acct, 1_500_000, "tight", "admin", "idem-pg-tight"); err != nil {
		t.Fatalf("Topup: %v", err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			taskID := fmt.Sprintf("task-pg-tight-%d", idx)
			idem := adapter.IdempotencyKey(fmt.Sprintf("idem-pg-tight-%d", idx))
			_, errs[idx] = w.Hold(ctx, acct, taskID, 1_000_000, idem)
		}(i)
	}
	wg.Wait()

	successes, insufficient, other := 0, 0, 0
	for _, e := range errs {
		switch {
		case e == nil:
			successes++
		case errors.Is(e, ErrInsufficientBalance):
			insufficient++
		default:
			other++
		}
	}
	if successes != 1 || insufficient != 1 || other != 0 {
		t.Errorf("got successes=%d insufficient=%d other=%d (errs=%v); want 1 success + 1 ErrInsufficientBalance",
			successes, insufficient, other, errs)
	}

	bal, _ := w.Balance(ctx, acct)
	if bal != 500_000 {
		t.Errorf("balance = %d; want 500_000 (1.5 - 1.0 = 0.5)", bal)
	}
	if sum, _ := w.InvariantSum(ctx); sum != 0 {
		t.Errorf("InvariantSum = %d; want 0", sum)
	}
}

// TestPostgresTx_ConcurrentSettleHoldRefund spawns a Hold + Refund flow
// concurrent with another Hold against the same account. Stresses the
// SERIALIZABLE retry path with mixed operations.
func TestPostgresTx_ConcurrentSettleHoldRefund(t *testing.T) {
	w, _ := newPostgresWallet(t)
	if w == nil {
		return
	}
	ctx := context.Background()

	const acct AccountID = "user:pg-mixed"
	if err := w.Topup(ctx, acct, 5_000_000, "seed", "admin", "idem-pg-mixed-seed"); err != nil {
		t.Fatalf("Topup: %v", err)
	}
	// Pre-stage one held escrow we'll Refund concurrently.
	preEscrow, err := w.Hold(ctx, acct, "task-pg-pre", 500_000, "idem-pg-pre-hold")
	if err != nil {
		t.Fatalf("pre Hold: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(3)
	holdErrs := make([]error, 2)

	go func() {
		defer wg.Done()
		_, holdErrs[0] = w.Hold(ctx, acct, "task-pg-mix-A", 200_000, "idem-pg-mix-A")
	}()
	go func() {
		defer wg.Done()
		_, holdErrs[1] = w.Hold(ctx, acct, "task-pg-mix-B", 200_000, "idem-pg-mix-B")
	}()
	refundErr := errors.New("not run")
	go func() {
		defer wg.Done()
		refundErr = w.Refund(ctx, preEscrow)
	}()
	wg.Wait()

	for i, e := range holdErrs {
		if e != nil {
			t.Errorf("Hold %d: %v (retry path didn't recover)", i, e)
		}
	}
	if refundErr != nil {
		t.Errorf("Refund: %v (retry path didn't recover)", refundErr)
	}

	// Account: 5.0 -.5 (preHold) -.2 -.2 +.5 (refund) = 4.6 → 4_600_000.
	bal, _ := w.Balance(ctx, acct)
	if bal != 4_600_000 {
		t.Errorf("balance = %d; want 4_600_000", bal)
	}
	if sum, _ := w.InvariantSum(ctx); sum != 0 {
		t.Errorf("InvariantSum after mixed concurrency = %d; want 0", sum)
	}
}

// TestPostgresTx_RetryBudgetReachable confirms that the retry budget
// (maxTxRetries=5) is plumbed end-to-end on Postgres. We can't
// realistically force 5 successive 40001s in a deterministic test
// without driver injection — so this test asserts only that under heavy
// contention all goroutines either succeed or fail with a CLEAN
// well-typed error (ErrInsufficientBalance / ErrConcurrencyExhausted),
// never a raw Postgres serialization-failure leak.
func TestPostgresTx_HighContention_NoRawSerializationLeaks(t *testing.T) {
	w, _ := newPostgresWallet(t)
	if w == nil {
		return
	}
	ctx := context.Background()

	const acct AccountID = "user:pg-storm"
	if err := w.Topup(ctx, acct, 50_000_000, "storm", "admin", "idem-pg-storm"); err != nil {
		t.Fatalf("Topup: %v", err)
	}

	const N = 8
	var wg sync.WaitGroup
	errs := make([]error, N)
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(idx int) {
			defer wg.Done()
			taskID := fmt.Sprintf("task-pg-storm-%d", idx)
			idem := adapter.IdempotencyKey(fmt.Sprintf("idem-pg-storm-%d", idx))
			_, errs[idx] = w.Hold(ctx, acct, taskID, 1_000_000, idem)
		}(i)
	}
	wg.Wait()

	for i, e := range errs {
		if e == nil {
			continue
		}
		// Acceptable: ErrInsufficientBalance, ErrConcurrencyExhausted (rare).
		// Unacceptable: a raw "could not serialize access" leaking through.
		if errors.Is(e, ErrInsufficientBalance) ||
			errors.Is(e, ErrConcurrencyExhausted) {
			continue
		}
		t.Errorf("goroutine %d returned unwrapped error %v (retry path leaked a raw PG error?)", i, e)
	}

	if sum, _ := w.InvariantSum(ctx); sum != 0 {
		t.Errorf("InvariantSum after storm = %d; want 0", sum)
	}
}
