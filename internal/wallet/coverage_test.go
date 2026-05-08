// Coverage-targeted tests for branches that the smoke suite (wallet_test.go)
// doesn't naturally exercise. Kept in a separate file so the smoke suite
// stays readable.
//
// Coverage focus:
//   - dialect.go     : PostgresDialect SQL strings + SQLite SQL string shape
//   - subscriber.go  : every event handler through a real SQLite-backed wallet
//   - wallet.go      : EnsureAccount, InvariantSum, UserAccountID, SetClock,
//                      input-validation early returns
//   - tx.go          : isRetryableTxError, containsAny, txOptionsFor
//
// Per F2 in plans/CODE-REVIEW.md the production Postgres path is still only
// verified via shape assertions; a real Postgres testcontainer suite is
// deferred. The SERIALIZABLE retry path is verified by exercising the pure
// retry-classifier with synthetic SQLSTATE strings.

package wallet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/QuantumNous/new-api/internal/adapter"
	"github.com/QuantumNous/new-api/internal/events"
)

// ─────────────────────────────────────────────────────────────────────────
// Pure helpers
// ─────────────────────────────────────────────────────────────────────────

func TestEscrowID_FormatsCanonically(t *testing.T) {
	if got := EscrowID("task-42"); got != "escrow:task-42" {
		t.Errorf("EscrowID = %q; want escrow:task-42", got)
	}
	if got := EscrowID(""); got != "escrow:" {
		t.Errorf("EscrowID(empty) = %q; want escrow:", got)
	}
}

func TestUserAccountID_FormatsCanonically(t *testing.T) {
	if got := UserAccountID("123"); got != AccountID("user:123") {
		t.Errorf("UserAccountID = %q; want user:123", got)
	}
	if got := UserAccountID(""); got != AccountID("user:") {
		t.Errorf("UserAccountID(empty) = %q; want user:", got)
	}
}

func TestNullableString_BlankReturnsNil(t *testing.T) {
	if got := nullableString(""); got != nil {
		t.Errorf("nullableString(\"\") = %v; want nil", got)
	}
	if got := nullableString("x"); got != "x" {
		t.Errorf("nullableString(\"x\") = %v; want \"x\"", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// tx.go — retry classifier + tx options
// ─────────────────────────────────────────────────────────────────────────

func TestContainsAny_PositiveAndNegative(t *testing.T) {
	if !containsAny("foo bar baz", "qux", "bar") {
		t.Error("expected match on 'bar'")
	}
	if containsAny("foo bar baz", "qux", "fred") {
		t.Error("unexpected match")
	}
	if containsAny("anything", /* no needles */ ) {
		t.Error("zero needles must not match")
	}
}

func TestIsRetryableTxError_KnownStrings(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"sql.ErrConnDone", sql.ErrConnDone, false},
		{"sql.ErrTxDone", sql.ErrTxDone, false},
		{"pg 40001", errors.New("ERROR: 40001 serialization failure"), true},
		{"pg 40P01", errors.New("ERROR: 40P01 deadlock"), true},
		{"could not serialize", errors.New("could not serialize access due to read/write dependencies"), true},
		{"deadlock detected", errors.New("deadlock detected"), true},
		{"sqlite locked", errors.New("database is locked"), true},
		{"sqlite table locked", errors.New("database table is locked"), true},
		{"unrelated", errors.New("connection refused"), false},
	}
	for _, tc := range cases {
		if got := isRetryableTxError(tc.err); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestTxOptionsFor_PostgresVsSQLite(t *testing.T) {
	pgOpts := txOptionsFor(PostgresDialect{})
	if pgOpts == nil || pgOpts.Isolation != sql.LevelSerializable {
		t.Errorf("PostgresDialect → %+v; want LevelSerializable", pgOpts)
	}
	if got := txOptionsFor(SQLiteDialect{}); got != nil {
		t.Errorf("SQLiteDialect → %+v; want nil", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// dialect.go — PostgresDialect SQL string shape contracts
//
// Production Postgres isn't reachable from unit tests; instead we lock the
// SQL surface so accidental breakage (e.g., dropping FOR UPDATE) shows up
// here. Mirrors the design intent in dialect.go's header comment.
// ─────────────────────────────────────────────────────────────────────────

func TestPostgresDialect_SQLShape(t *testing.T) {
	d := PostgresDialect{}

	cases := []struct {
		name string
		sql  string
		want []string
	}{
		{"InsertAccount", d.InsertAccountSQL(), []string{
			"INSERT INTO wallet_account", "ON CONFLICT", "DO NOTHING", "$1", "$4",
		}},
		{"SelectAccountByID", d.SelectAccountByIDSQL(), []string{
			"SELECT", "wallet_account", "WHERE id = $1",
		}},
		{"LockAccountByID", d.LockAccountByIDSQL(), []string{
			"FOR UPDATE", "WHERE id = $1",
		}},
		{"InsertLedger", d.InsertLedgerSQL(), []string{
			"INSERT INTO wallet_ledger", "operation_id", "amount_micro_usd",
			"reason_code", "ref_idempotency", "$7",
		}},
		{"SelectBalance", d.SelectBalanceSQL(), []string{
			"COALESCE(SUM(amount_micro_usd)", "wallet_ledger", "WHERE account_id = $1",
		}},
		{"SelectInvariantSum", d.SelectInvariantSumSQL(), []string{
			"COALESCE(SUM(amount_micro_usd)", "FROM wallet_ledger",
		}},
		{"SelectLedgerByIdempotencyAndReason", d.SelectLedgerByIdempotencyAndReasonSQL(), []string{
			"WHERE ref_idempotency = $1", "AND reason_code = $2", "LIMIT 1",
		}},
		{"SelectLedgerForAccount", d.SelectLedgerForAccountSQL(), []string{
			"WHERE account_id = $1", "ORDER BY created_at",
		}},
		{"InsertTopupAudit", d.InsertTopupAuditSQL(), []string{
			"INSERT INTO topup_audit", "admin_user_id", "$6",
		}},
	}
	for _, tc := range cases {
		for _, want := range tc.want {
			if !strings.Contains(tc.sql, want) {
				t.Errorf("%s missing %q\nfull SQL:\n%s", tc.name, want, tc.sql)
			}
		}
	}

	// CreateSchemaSQL is intentionally empty for Postgres (goose owns prod).
	if got := d.CreateSchemaSQL(); len(got) != 0 {
		t.Errorf("PostgresDialect.CreateSchemaSQL = %v; want empty (goose owns prod)", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// New() panic guards
// ─────────────────────────────────────────────────────────────────────────

func TestNew_PanicsOnNilDB(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic on nil db")
		}
	}()
	_ = New(nil, SQLiteDialect{})
}

func TestNew_PanicsOnNilDialect(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic on nil dialect")
		}
	}()
	db, _ := sql.Open("sqlite", "file::memory:?cache=shared")
	defer db.Close()
	_ = New(db, nil)
}

// ─────────────────────────────────────────────────────────────────────────
// EnsureAccount
// ─────────────────────────────────────────────────────────────────────────

func TestEnsureAccount_HappyAndIdempotent(t *testing.T) {
	w, cleanup := newTestWallet(t)
	defer cleanup()
	ctx := context.Background()

	const acct AccountID = "user:7777"
	if err := w.EnsureAccount(ctx, acct, AccountKindUserWallet, "user-7777"); err != nil {
		t.Fatalf("first EnsureAccount: %v", err)
	}
	// Replay must be a no-op (ON CONFLICT DO NOTHING).
	if err := w.EnsureAccount(ctx, acct, AccountKindUserWallet, "user-7777"); err != nil {
		t.Fatalf("replay EnsureAccount: %v", err)
	}
	// Verify exactly one row exists.
	var n int
	if err := w.db.QueryRow(`SELECT COUNT(*) FROM wallet_account WHERE id = $1`, string(acct)).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("account row count = %d; want 1", n)
	}
}

func TestEnsureAccount_RejectsEmptyArgs(t *testing.T) {
	w, cleanup := newTestWallet(t)
	defer cleanup()
	ctx := context.Background()

	if err := w.EnsureAccount(ctx, "", AccountKindUserWallet, "x"); err == nil {
		t.Error("EnsureAccount(empty account) should error")
	}
	if err := w.EnsureAccount(ctx, "user:1", "", "x"); err == nil {
		t.Error("EnsureAccount(empty kind) should error")
	}
}

// ─────────────────────────────────────────────────────────────────────────
// InvariantSum
// ─────────────────────────────────────────────────────────────────────────

func TestInvariantSum_AlwaysZeroAfterOperations(t *testing.T) {
	w, cleanup := newTestWallet(t)
	defer cleanup()
	ctx := context.Background()

	// Empty ledger — sum is 0.
	if got, err := w.InvariantSum(ctx); err != nil || got != 0 {
		t.Errorf("InvariantSum on empty ledger = (%d, %v); want (0, nil)", got, err)
	}

	mustTopup(t, w, testUser, 1.0)
	if got, _ := w.InvariantSum(ctx); got != 0 {
		t.Errorf("InvariantSum after Topup = %d; want 0 (paired ledger)", got)
	}

	escrow, _ := w.Hold(ctx, testUser, "task-inv", 250_000, "idem-inv")
	if got, _ := w.InvariantSum(ctx); got != 0 {
		t.Errorf("InvariantSum after Hold = %d; want 0", got)
	}
	if err := w.Settle(ctx, escrow, 200_000); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if got, _ := w.InvariantSum(ctx); got != 0 {
		t.Errorf("InvariantSum after Settle = %d; want 0", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// SetClock
// ─────────────────────────────────────────────────────────────────────────

func TestSetClock_StampsLedgerWithFakeNow(t *testing.T) {
	w, cleanup := newTestWallet(t)
	defer cleanup()

	fixed := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	w.SetClock(func() time.Time { return fixed })

	mustTopup(t, w, testUser, 1.0)
	var got time.Time
	if err := w.db.QueryRow(`SELECT created_at FROM wallet_ledger LIMIT 1`).Scan(&got); err != nil {
		t.Fatalf("read created_at: %v", err)
	}
	if !got.Equal(fixed) {
		t.Errorf("created_at = %v; want %v", got, fixed)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Input validation early-returns
// ─────────────────────────────────────────────────────────────────────────

func TestHold_InputValidation(t *testing.T) {
	w, cleanup := newTestWallet(t)
	defer cleanup()
	ctx := context.Background()

	cases := []struct {
		name string
		acct AccountID
		task string
		cost adapter.CostUSD
		idem adapter.IdempotencyKey
	}{
		{"empty account", "", "t", 100, "k"},
		{"empty taskID", "user:1", "", 100, "k"},
		{"zero cost", "user:1", "t", 0, "k"},
		{"negative cost", "user:1", "t", -1, "k"},
		{"empty idem", "user:1", "t", 100, ""},
	}
	for _, tc := range cases {
		if _, err := w.Hold(ctx, tc.acct, tc.task, tc.cost, tc.idem); err == nil {
			t.Errorf("%s: Hold should error", tc.name)
		}
	}
}

func TestSettle_InputValidation(t *testing.T) {
	w, cleanup := newTestWallet(t)
	defer cleanup()
	ctx := context.Background()

	if err := w.Settle(ctx, "", 100); err == nil {
		t.Error("Settle(empty escrow) should error")
	}
	if err := w.Settle(ctx, "escrow:x", -1); err == nil {
		t.Error("Settle(negative actual) should error")
	}
}

func TestRefund_InputValidation(t *testing.T) {
	w, cleanup := newTestWallet(t)
	defer cleanup()
	ctx := context.Background()
	if err := w.Refund(ctx, ""); err == nil {
		t.Error("Refund(empty escrow) should error")
	}
}

func TestPartialRefund_InputValidation(t *testing.T) {
	w, cleanup := newTestWallet(t)
	defer cleanup()
	ctx := context.Background()
	if err := w.PartialRefund(ctx, "", 0.5); err == nil {
		t.Error("PartialRefund(empty escrow) should error")
	}
	if err := w.PartialRefund(ctx, "escrow:x", -0.1); err == nil {
		t.Error("PartialRefund(negative fraction) should error")
	}
	if err := w.PartialRefund(ctx, "escrow:x", 1.1); err == nil {
		t.Error("PartialRefund(fraction>1) should error")
	}
}

func TestTopup_InputValidation(t *testing.T) {
	w, cleanup := newTestWallet(t)
	defer cleanup()
	ctx := context.Background()
	if err := w.Topup(ctx, "", 100, "", "admin", ""); err == nil {
		t.Error("Topup(empty account) should error")
	}
	if err := w.Topup(ctx, testUser, 100, "", "", ""); err == nil {
		t.Error("Topup(empty admin) should error")
	}
}

func TestBalance_InputValidation(t *testing.T) {
	w, cleanup := newTestWallet(t)
	defer cleanup()
	ctx := context.Background()
	if _, err := w.Balance(ctx, ""); err == nil {
		t.Error("Balance(empty account) should error")
	}
}

// Settle on never-held escrow returns ErrEscrowAlreadySettled (held = 0).
// Exercises the "held == 0" branch in Settle.
func TestSettle_OnUnknownEscrowReturnsAlreadySettled(t *testing.T) {
	w, cleanup := newTestWallet(t)
	defer cleanup()
	ctx := context.Background()

	if err := w.Settle(ctx, "escrow:never-held", 100); !errors.Is(err, ErrEscrowAlreadySettled) {
		t.Errorf("Settle on unknown escrow = %v; want ErrEscrowAlreadySettled", err)
	}
}

// Refund on never-held escrow → same behaviour.
func TestRefund_OnUnknownEscrowReturnsAlreadySettled(t *testing.T) {
	w, cleanup := newTestWallet(t)
	defer cleanup()
	ctx := context.Background()

	if err := w.Refund(ctx, "escrow:never-held"); !errors.Is(err, ErrEscrowAlreadySettled) {
		t.Errorf("Refund on unknown escrow = %v; want ErrEscrowAlreadySettled", err)
	}
}

func TestPartialRefund_OnUnknownEscrowReturnsAlreadySettled(t *testing.T) {
	w, cleanup := newTestWallet(t)
	defer cleanup()
	ctx := context.Background()

	if err := w.PartialRefund(ctx, "escrow:never-held", 0.5); !errors.Is(err, ErrEscrowAlreadySettled) {
		t.Errorf("PartialRefund on unknown escrow = %v; want ErrEscrowAlreadySettled", err)
	}
}

// Topup idempotent replay — second call with same idem must be a no-op.
func TestTopup_IdempotentReplay(t *testing.T) {
	w, cleanup := newTestWallet(t)
	defer cleanup()
	ctx := context.Background()

	if err := w.Topup(ctx, testUser, 1_000_000, "first", "admin", "idem-topup"); err != nil {
		t.Fatalf("first Topup: %v", err)
	}
	if err := w.Topup(ctx, testUser, 1_000_000, "second", "admin", "idem-topup"); err != nil {
		t.Fatalf("replay Topup: %v", err)
	}
	bal, _ := w.Balance(ctx, testUser)
	if bal != 1_000_000 {
		t.Errorf("balance after replay = %d; want 1_000_000 (single charge)", bal)
	}
}

// Settle that under-charges by exactly the held amount (held == actual) does
// NOT emit the over-refund branch. Locks the equality boundary so a
// future "remaining > 0" → "remaining >= 0" regression would surface here.
func TestSettle_ExactCostNoOverRefund(t *testing.T) {
	w, cleanup := newTestWallet(t)
	defer cleanup()
	ctx := context.Background()
	mustTopup(t, w, testUser, 1.0)

	escrow, _ := w.Hold(ctx, testUser, "task-eq", 500_000, "idem-eq")
	if err := w.Settle(ctx, escrow, 500_000); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	bal, _ := w.Balance(ctx, testUser)
	if bal != 500_000 {
		t.Errorf("user balance = %d; want 500_000 (no over-refund)", bal)
	}
	rev, _ := w.Balance(ctx, SystemAccountRevenue)
	if rev != 500_000 {
		t.Errorf("revenue = %d; want 500_000", rev)
	}
}

// Settle clamps over-charge to held instead of going negative on the escrow.
func TestSettle_OverChargeClampsToHeld(t *testing.T) {
	w, cleanup := newTestWallet(t)
	defer cleanup()
	ctx := context.Background()
	mustTopup(t, w, testUser, 1.0)

	escrow, _ := w.Hold(ctx, testUser, "task-clamp", 200_000, "idem-clamp")
	// Pass actual = 999_999 — wallet must clamp to 200_000 held.
	if err := w.Settle(ctx, escrow, 999_999); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	rev, _ := w.Balance(ctx, SystemAccountRevenue)
	if rev != 200_000 {
		t.Errorf("revenue = %d; want 200_000 (clamped to held)", rev)
	}
	escrowBal, _ := w.Balance(ctx, AccountID(escrow))
	if escrowBal != 0 {
		t.Errorf("escrow balance = %d; want 0", escrowBal)
	}
}

// PartialRefund with assetCostFraction = 0 → full revenue, no user refund.
func TestPartialRefund_AllCompute(t *testing.T) {
	w, cleanup := newTestWallet(t)
	defer cleanup()
	ctx := context.Background()
	mustTopup(t, w, testUser, 1.0)

	escrow, _ := w.Hold(ctx, testUser, "task-allc", 400_000, "idem-allc")
	if err := w.PartialRefund(ctx, escrow, 0.0); err != nil {
		t.Fatalf("PartialRefund: %v", err)
	}
	bal, _ := w.Balance(ctx, testUser)
	if bal != 600_000 {
		t.Errorf("user balance = %d; want 600_000 (no refund)", bal)
	}
	rev, _ := w.Balance(ctx, SystemAccountRevenue)
	if rev != 400_000 {
		t.Errorf("revenue = %d; want 400_000", rev)
	}
}

// PartialRefund with assetCostFraction = 1 → full user refund, no revenue.
func TestPartialRefund_AllAsset(t *testing.T) {
	w, cleanup := newTestWallet(t)
	defer cleanup()
	ctx := context.Background()
	mustTopup(t, w, testUser, 1.0)

	escrow, _ := w.Hold(ctx, testUser, "task-alla", 400_000, "idem-alla")
	if err := w.PartialRefund(ctx, escrow, 1.0); err != nil {
		t.Fatalf("PartialRefund: %v", err)
	}
	bal, _ := w.Balance(ctx, testUser)
	if bal != 1_000_000 {
		t.Errorf("user balance = %d; want 1_000_000 (full refund)", bal)
	}
	rev, _ := w.Balance(ctx, SystemAccountRevenue)
	if rev != 0 {
		t.Errorf("revenue = %d; want 0 (no compute charge)", rev)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// Subscriber — wired to a real SQLite-backed wallet via MemoryBus.
//
// This is the only path that converts AssetHosted/AssetLost/TaskFailed/
// TaskTimedOut/TaskCancelled events into ledger operations. Per CODE-REVIEW.md
// F2, it had 0 % coverage prior to this file.
// ─────────────────────────────────────────────────────────────────────────

// recordingLogger captures errLogger output so tests can assert the
// "AssetHosted without prior TaskSucceeded" + Settle-failure paths fire.
type recordingLogger struct {
	mu   sync.Mutex
	logs []string
}

func (r *recordingLogger) capture() func(format string, args ...any) {
	return func(format string, args ...any) {
		r.mu.Lock()
		r.logs = append(r.logs, fmt.Sprintf(format, args...))
		r.mu.Unlock()
	}
}

func (r *recordingLogger) any(substr string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, line := range r.logs {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

// subscriberHarness wires real wallet + real bus + Subscriber. The
// subscriber Start() returns an Unsubscribe; cleanup calls it + the
// wallet's invariant check.
type subscriberHarness struct {
	w    *DBWallet
	bus  *events.MemoryBus
	sub  *Subscriber
	logs *recordingLogger
}

// pendingHarnessCounter so each harness gets a unique SQLite DSN even when
// run in parallel.
var pendingHarnessCounter atomic.Int64

func newSubscriberHarness(t *testing.T, fractionFn AssetCostFractionFunc) (*subscriberHarness, func()) {
	t.Helper()
	dsn := fmt.Sprintf("file:wallet_sub_test_%d?mode=memory&cache=shared", pendingHarnessCounter.Add(1))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	dialect := SQLiteDialect{}
	for _, stmt := range dialect.CreateSchemaSQL() {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	w := New(db, dialect)
	bus := events.NewMemoryBus()
	logs := &recordingLogger{}
	sub := NewSubscriber(w, bus, fractionFn)
	sub.SetErrLogger(logs.capture())
	unsub := sub.Start(context.Background())
	cleanup := func() {
		unsub()
		_ = bus.Close()
		// Sum-zero invariant check on close.
		var total int64
		_ = db.QueryRow(dialect.SelectInvariantSumSQL()).Scan(&total)
		if total != 0 {
			t.Errorf("INVARIANT VIOLATED: SUM=%d", total)
		}
		db.Close()
	}
	return &subscriberHarness{w: w, bus: bus, sub: sub, logs: logs}, cleanup
}

// publish + drain. MemoryBus is synchronous in Subscribe, so Publish
// already invokes the handler before returning — no additional wait is
// required. Kept as a helper so the tests read like a story.
func publish(t *testing.T, bus *events.MemoryBus, ev events.Event) {
	t.Helper()
	if err := bus.Publish(ev); err != nil {
		t.Fatalf("publish %s: %v", ev.Kind(), err)
	}
}

func TestSubscriber_HappyPath_TaskSucceededThenAssetHosted(t *testing.T) {
	h, cleanup := newSubscriberHarness(t, nil) // default 0.5 fraction unused here
	defer cleanup()

	mustTopup(t, h.w, testUser, 2.0)
	escrow, err := h.w.Hold(context.Background(), testUser, "t-happy", 500_000, "idem-happy")
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}

	// TaskSucceeded caches the actual cost the subscriber should Settle for.
	publish(t, h.bus, events.MakeTaskSucceeded(events.NewBaseEvent("t-happy"), 400_000))
	// AssetHosted triggers Settle.
	publish(t, h.bus, events.MakeAssetHosted(events.NewBaseEvent("t-happy"),
		"https://cdn.example.com/x.png", 1024))

	bal, _ := h.w.Balance(context.Background(), testUser)
	if bal != 1_600_000 {
		t.Errorf("balance after AssetHosted = %d; want 1_600_000 ($1.60 = $2 - $0.40 charged)", bal)
	}
	escrowBal, _ := h.w.Balance(context.Background(), AccountID(escrow))
	if escrowBal != 0 {
		t.Errorf("escrow not drained: %d", escrowBal)
	}
	rev, _ := h.w.Balance(context.Background(), SystemAccountRevenue)
	if rev != 400_000 {
		t.Errorf("revenue = %d; want 400_000", rev)
	}
}

func TestSubscriber_AssetHostedWithoutPriorSucceededLogsWarning(t *testing.T) {
	h, cleanup := newSubscriberHarness(t, nil)
	defer cleanup()

	// AssetHosted for a task that was never held: subscriber logs the
	// "without prior TaskSucceeded" warning, then calls Settle(escrow, 0).
	// Settle sees held=0 and returns ErrEscrowAlreadySettled, which the
	// subscriber swallows. No ledger rows are written.
	//
	// Latent S6 note (not exercised here): if a *funded* escrow received
	// AssetHosted without prior TaskSucceeded, the Settle(0) call would
	// hit the wallet_ledger CHECK(amount<>0) constraint at the drain row.
	// Tracked alongside TestSettleIdempotentReplay's S6 follow-up.
	publish(t, h.bus, events.MakeAssetHosted(events.NewBaseEvent("t-noprior"),
		"https://cdn.example.com/x.png", 1024))

	if !h.logs.any("AssetHosted without prior TaskSucceeded") {
		t.Errorf("expected warning log; got %v", h.logs.logs)
	}
	if h.logs.any("Settle failed") {
		t.Errorf("Settle should swallow ErrEscrowAlreadySettled silently; got %v", h.logs.logs)
	}
	// No ledger rows for the never-held escrow.
	var n int
	if err := h.w.db.QueryRow(`SELECT COUNT(*) FROM wallet_ledger WHERE ref_task_id = $1`, "t-noprior").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 ledger rows for never-held task; got %d", n)
	}
}

func TestSubscriber_TaskFailed_TimedOut_Cancelled_AllRefund(t *testing.T) {
	cases := []struct {
		name     string
		taskID   string
		makeEv   func(events.BaseEvent) events.Event
		topup    float64
		hold     adapter.CostUSD
		wantBal  adapter.CostUSD
	}{
		{"failed", "t-failed", func(b events.BaseEvent) events.Event {
			return events.MakeTaskFailed(b, "upstream", "boom")
		}, 1.0, 500_000, 1_000_000},
		{"timed_out", "t-timeout", func(b events.BaseEvent) events.Event {
			return events.MakeTaskTimedOut(b, 0)
		}, 1.0, 500_000, 1_000_000},
		{"cancelled", "t-cancel", func(b events.BaseEvent) events.Event {
			return events.MakeTaskCancelled(b, 0, "user")
		}, 1.0, 500_000, 1_000_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, cleanup := newSubscriberHarness(t, nil)
			defer cleanup()
			mustTopup(t, h.w, testUser, tc.topup)
			if _, err := h.w.Hold(context.Background(), testUser, tc.taskID, tc.hold,
				adapter.IdempotencyKey("idem-"+tc.taskID)); err != nil {
				t.Fatalf("Hold: %v", err)
			}
			publish(t, h.bus, tc.makeEv(events.NewBaseEvent(events.TaskID(tc.taskID))))
			bal, _ := h.w.Balance(context.Background(), testUser)
			if bal != tc.wantBal {
				t.Errorf("balance = %d; want %d (full refund)", bal, tc.wantBal)
			}
		})
	}
}

// onTaskTerminal silent-return: republishing a terminal event after the
// escrow has already been refunded must not log an error. Exercises the
// `errors.Is(err, ErrEscrowAlreadySettled) → return` branch.
func TestSubscriber_TaskTerminalReplayIsSilent(t *testing.T) {
	h, cleanup := newSubscriberHarness(t, nil)
	defer cleanup()
	mustTopup(t, h.w, testUser, 1.0)
	if _, err := h.w.Hold(context.Background(), testUser, "t-trep", 500_000, "idem-trep"); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	publish(t, h.bus, events.MakeTaskFailed(events.NewBaseEvent("t-trep"), "upstream", "1"))
	logsBefore := len(h.logs.logs)

	// Replay must be silent — escrow is now drained, Refund returns
	// ErrEscrowAlreadySettled, subscriber swallows it.
	publish(t, h.bus, events.MakeTaskFailed(events.NewBaseEvent("t-trep"), "upstream", "2"))
	publish(t, h.bus, events.MakeTaskCancelled(events.NewBaseEvent("t-trep"), 0, "user"))
	publish(t, h.bus, events.MakeTaskTimedOut(events.NewBaseEvent("t-trep"), 0))

	if len(h.logs.logs) != logsBefore {
		t.Errorf("terminal-event replay produced extra logs: %v", h.logs.logs[logsBefore:])
	}
}

func TestSubscriber_AssetLost_UsesFractionFn(t *testing.T) {
	called := false
	fn := func(_ context.Context, taskID string) (float64, error) {
		called = true
		if taskID != "t-lost" {
			t.Errorf("fractionFn taskID = %q; want t-lost", taskID)
		}
		return 0.25, nil
	}
	h, cleanup := newSubscriberHarness(t, fn)
	defer cleanup()
	mustTopup(t, h.w, testUser, 1.0)
	if _, err := h.w.Hold(context.Background(), testUser, "t-lost", 400_000, "idem-lost"); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	publish(t, h.bus, events.MakeAssetLost(events.NewBaseEvent("t-lost"),
		"https://upstream/x.png", "404 expired"))
	if !called {
		t.Error("fractionFn not invoked")
	}
	bal, _ := h.w.Balance(context.Background(), testUser)
	// 25% of $0.40 = $0.10 refunded → user has $0.60 + $0.10 = $0.70.
	if bal != 700_000 {
		t.Errorf("balance = %d; want 700_000 ($0.60 + $0.10 refund)", bal)
	}
}

func TestSubscriber_AssetLost_FractionFnError_DefaultsTo50Pct(t *testing.T) {
	fn := func(_ context.Context, _ string) (float64, error) {
		return 0, errors.New("catalog miss")
	}
	h, cleanup := newSubscriberHarness(t, fn)
	defer cleanup()
	mustTopup(t, h.w, testUser, 1.0)
	if _, err := h.w.Hold(context.Background(), testUser, "t-lost-err", 400_000, "idem-lost-err"); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	publish(t, h.bus, events.MakeAssetLost(events.NewBaseEvent("t-lost-err"),
		"https://upstream/x.png", "broken"))
	if !h.logs.any("fractionFn failed") {
		t.Errorf("expected fractionFn-failed log; got %v", h.logs.logs)
	}
	// 50% default of $0.40 = $0.20 refund → user has $0.60 + $0.20 = $0.80.
	bal, _ := h.w.Balance(context.Background(), testUser)
	if bal != 800_000 {
		t.Errorf("balance = %d; want 800_000 (default 0.5)", bal)
	}
}

func TestSubscriber_DefaultFractionFn_WhenNil(t *testing.T) {
	h, cleanup := newSubscriberHarness(t, nil) // nil → 0.5 default
	defer cleanup()
	mustTopup(t, h.w, testUser, 1.0)
	if _, err := h.w.Hold(context.Background(), testUser, "t-nilf", 400_000, "idem-nilf"); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	publish(t, h.bus, events.MakeAssetLost(events.NewBaseEvent("t-nilf"), "u", "r"))
	bal, _ := h.w.Balance(context.Background(), testUser)
	if bal != 800_000 {
		t.Errorf("balance = %d; want 800_000 (nil fractionFn → 0.5)", bal)
	}
}

func TestSubscriber_IgnoresUnrelatedEvents(t *testing.T) {
	h, cleanup := newSubscriberHarness(t, nil)
	defer cleanup()
	// Publish events the wallet does NOT consume — must not panic and must
	// not produce ledger rows. Invariant check in cleanup verifies sum=0.
	publish(t, h.bus, events.MakeTaskHeld(events.NewBaseEvent("t-x"), 0, "model", "k"))
	publish(t, h.bus, events.MakeTaskSubmitted(events.NewBaseEvent("t-x"), "p", "u"))
	publish(t, h.bus, events.MakeTaskRunning(events.NewBaseEvent("t-x"), nil))
	publish(t, h.bus, events.MakeOutputAvailable(events.NewBaseEvent("t-x"), "u", "image/png", 1))
}

func TestSubscriber_ReplayedAssetHostedIsBenign(t *testing.T) {
	h, cleanup := newSubscriberHarness(t, nil)
	defer cleanup()
	ctx := context.Background()

	mustTopup(t, h.w, testUser, 1.0)
	if _, err := h.w.Hold(ctx, testUser, "t-rep", 500_000, "idem-rep"); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	publish(t, h.bus, events.MakeTaskSucceeded(events.NewBaseEvent("t-rep"), 300_000))
	publish(t, h.bus, events.MakeAssetHosted(events.NewBaseEvent("t-rep"),
		"https://cdn.example.com/x.png", 1024))

	// Snapshot ledger row count after the first (successful) AssetHosted.
	var rowsBefore int
	if err := h.w.db.QueryRow(`SELECT COUNT(*) FROM wallet_ledger`).Scan(&rowsBefore); err != nil {
		t.Fatalf("count rows before: %v", err)
	}

	// Replay: pendingCost was consumed by call #1, so the warning fires;
	// Settle then sees held=0 (escrow drained) → ErrEscrowAlreadySettled →
	// subscriber's errors.Is branch returns silently. No new ledger rows,
	// no Settle-failed log.
	publish(t, h.bus, events.MakeAssetHosted(events.NewBaseEvent("t-rep"),
		"https://cdn.example.com/x.png", 1024))

	var rowsAfter int
	if err := h.w.db.QueryRow(`SELECT COUNT(*) FROM wallet_ledger`).Scan(&rowsAfter); err != nil {
		t.Fatalf("count rows after: %v", err)
	}
	if rowsAfter != rowsBefore {
		t.Errorf("replay added %d ledger rows; want 0", rowsAfter-rowsBefore)
	}
	if h.logs.any("Settle failed") {
		t.Errorf("Settle replay should be silent; got %v", h.logs.logs)
	}
}

func TestSubscriber_SetErrLogger_NilIgnored(t *testing.T) {
	h, cleanup := newSubscriberHarness(t, nil)
	defer cleanup()
	// Replace with nil — should be ignored, original logger preserved.
	h.sub.SetErrLogger(nil)
	publish(t, h.bus, events.MakeAssetHosted(events.NewBaseEvent("t-noprior"), "u", 1))
	// Logger from harness still captures.
	if !h.logs.any("AssetHosted without prior") {
		t.Errorf("nil SetErrLogger should not replace the existing logger")
	}
}

func TestSubscriber_Stop_IsIdempotent(t *testing.T) {
	h, cleanup := newSubscriberHarness(t, nil)
	defer cleanup()
	h.sub.Stop()
	h.sub.Stop() // second call must not panic
}

func TestNewSubscriber_PanicOnNilWallet(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic on nil wallet")
		}
	}()
	_ = NewSubscriber(nil, events.NewMemoryBus(), nil)
}

func TestNewSubscriber_PanicOnNilBus(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic on nil bus")
		}
	}()
	w, cleanup := newTestWallet(t)
	defer cleanup()
	_ = NewSubscriber(w, nil, nil)
}

