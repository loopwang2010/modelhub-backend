// Wallet — credit ledger business logic.
//
// Per S6-WALLET-DESIGN.md and ADR-005 (double-entry ledger, hold-then-settle):
//
//   Hold:   user_wallet -cost  +  task_escrow +cost
//   Settle: task_escrow -held  +  modelhub_revenue +held
//           [if held > actual:]
//             modelhub_revenue -(held-actual) + user_wallet +(held-actual)
//   Refund: task_escrow -held  +  user_wallet +held
//   Partial: task_escrow -compute + modelhub_revenue +compute
//            task_escrow -asset   + user_wallet +asset
//   Topup:  external_topup -amt  +  user_wallet +amt
//
// Every operation runs through runTx (AP-6 SERIALIZABLE + retry). Each
// operation_id maps to exactly two ledger rows summing to zero (I1).
// Idempotency is enforced at SQL level via the unique index on
// (ref_idempotency, reason_code) and at app level by checking for an
// existing row with the same idempotency key before inserting.
//
// Wallet does NOT import internal/task — coupling is via internal/events
// (per ADR-011). subscriber.go wires the EventBus → Wallet method calls.

package wallet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/QuantumNous/new-api/internal/adapter"
)

// ─────────────────────────────────────────────────────────────────────────
// Sentinel errors
// ─────────────────────────────────────────────────────────────────────────

var (
	// ErrInsufficientBalance is returned by Hold when the user's available
	// balance is below the requested cost. Callers surface a 402-class error.
	ErrInsufficientBalance = errors.New("wallet: insufficient balance")

	// ErrEscrowNotFound is returned when Settle/Refund/PartialRefund is called
	// on an escrow that has no Hold row (programmer error or a stale call after
	// the escrow was already settled by a parallel path).
	ErrEscrowNotFound = errors.New("wallet: escrow not found")

	// ErrEscrowAlreadySettled is returned when a terminal op (Settle/Refund/
	// PartialRefund) is replayed against an already-terminal escrow.
	ErrEscrowAlreadySettled = errors.New("wallet: escrow already settled or refunded")

	// ErrInvalidTopupAmount is returned by Topup for amount<=0 or > MaxTopupUSD.
	ErrInvalidTopupAmount = errors.New("wallet: invalid topup amount")

	// ErrAccountNotFound is returned when an op targets an account_id that has
	// no wallet_account row yet. Caller should run EnsureAccount first.
	ErrAccountNotFound = errors.New("wallet: account not found")

	// ErrCostCeilingExceeded re-exports adapter.ErrCostCeilingExceeded so
	// wallet callers don't need to import the adapter package just to switch
	// on this error.
	ErrCostCeilingExceeded = adapter.ErrCostCeilingExceeded

	// ErrConcurrencyExhausted is returned by runTx when the SERIALIZABLE
	// retry budget is exhausted. Indicates systemic contention; alert ops.
	ErrConcurrencyExhausted = errors.New("wallet: serialization conflict, retries exhausted")
)

// MaxTopupUSD is the largest single admin top-up allowed (in micro-USD).
// Mirrors MaxCostUSD's $1000 ceiling × 10 — admins legitimately top up more
// than a single request can spend.
const MaxTopupUSD adapter.CostUSD = 10_000_000_000

// ─────────────────────────────────────────────────────────────────────────
// Wallet interface
// ─────────────────────────────────────────────────────────────────────────

// Wallet is the public surface of the credit ledger. All methods are
// idempotent against their canonical key (taskID + reason for task-bound
// ops; ref_idempotency for top-ups).
type Wallet interface {
	// EnsureAccount creates a wallet_account row if missing. Idempotent.
	// Called by the auth layer on registration (per ADR-013) and by Hold's
	// internal escrow creation. account.id format: "user:{user_id}" for
	// user wallets; "escrow:{task_id}" for task escrows.
	EnsureAccount(ctx context.Context, account AccountID, kind AccountKind, ownerSubject string) error

	// Hold reserves cost from the user's wallet into a per-task escrow.
	// Returns the escrow account id ("escrow:{taskID}").
	// Idempotent on (taskID, "hold"): replay returns the same escrow id
	// and writes no new ledger rows.
	Hold(ctx context.Context, account AccountID, taskID string, cost adapter.CostUSD, idem adapter.IdempotencyKey) (escrowID string, err error)

	// Settle drains the escrow into modelhub revenue at actualCost. If
	// actualCost < held amount, the difference is refunded to the user
	// in a SECOND operation (paired again).
	// Idempotent on (taskID, "settle_drain"): replay is a no-op.
	Settle(ctx context.Context, escrowID string, actualCost adapter.CostUSD) error

	// Refund returns the full held amount to the user. For TaskFailed /
	// TaskTimedOut / TaskCancelled.
	// Idempotent on (taskID, "refund_full").
	Refund(ctx context.Context, escrowID string) error

	// PartialRefund settles the compute portion to revenue, refunds the
	// asset portion to the user. For StateAssetLost.
	// assetCostFraction in [0,1] — typically from ModelManifest.AssetCostFraction.
	// Idempotent on (taskID, "asset_partial_refund").
	PartialRefund(ctx context.Context, escrowID string, assetCostFraction float64) error

	// Topup credits the user's wallet from the external_topup source.
	// Admin-only path; the audit row is written alongside.
	// Idempotent on (idem, "topup") if idem is non-empty.
	Topup(ctx context.Context, account AccountID, amount adapter.CostUSD, note string, adminUserID string, idem adapter.IdempotencyKey) error

	// Balance returns the current spendable balance for an account.
	// Pure derived view — no balance column anywhere.
	Balance(ctx context.Context, account AccountID) (adapter.CostUSD, error)
}

// AccountID is the typed identifier for billable subjects (per ADR-013).
// Today every value is "user:<numeric>" or "escrow:<task_id>" or one of the
// system:* constants below. Future multi-tenancy may introduce "org:<uuid>".
//
// This type is defined in the wallet package (not adapter) because the wallet
// is the canonical owner of accounts; the adapter package's ProviderAdapter
// interface deliberately does not couple to billing identity.
type AccountID string

// AccountKind enumerates the wallet_account.kind ENUM values.
// Mirrors the SQL enum in 0003_wallet_ledger.sql.
type AccountKind string

const (
	AccountKindUserWallet      AccountKind = "user_wallet"
	AccountKindTaskEscrow      AccountKind = "task_escrow"
	AccountKindModelhubRevenue AccountKind = "modelhub_revenue"
	AccountKindExternalTopup   AccountKind = "external_topup"
)

// Reason codes for ledger rows. Constants centralize the strings so the
// idempotency lookup uses the same value the writer used.
const (
	ReasonHold              = "hold"
	ReasonSettleDrain       = "settle_drain"
	ReasonSettleOverRefund  = "settle_overheld_refund"
	ReasonRefundFull        = "refund_full"
	ReasonAssetPartialDrain = "asset_partial_compute"
	ReasonAssetPartialBack  = "asset_partial_refund"
	ReasonTopup             = "topup"
)

// SystemAccountRevenue is the global revenue sink. Created lazily on first
// Settle. Negative balance is fine — it grows as we book revenue.
const SystemAccountRevenue AccountID = "system:modelhub_revenue"

// SystemAccountTopupSource is the implicit "outside world" account that
// funds top-ups. Negative balance is expected.
const SystemAccountTopupSource AccountID = "system:external_topup"

// ─────────────────────────────────────────────────────────────────────────
// DBWallet — primary implementation backed by database/sql
// ─────────────────────────────────────────────────────────────────────────

// DBWallet is the production Wallet implementation backed by
// database/sql + the wallet_ledger schema.
type DBWallet struct {
	db      *sql.DB
	dialect Dialect
	now     func() time.Time // injectable for tests
}

// New constructs a DBWallet. Pass PostgresDialect{} in production,
// SQLiteDialect{} in tests.
func New(db *sql.DB, dialect Dialect) *DBWallet {
	if db == nil {
		panic("wallet: New requires non-nil db")
	}
	if dialect == nil {
		panic("wallet: New requires non-nil dialect")
	}
	return &DBWallet{
		db:      db,
		dialect: dialect,
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// SetClock injects a fake clock for tests. Safe to call between operations
// but not concurrently.
func (w *DBWallet) SetClock(clock func() time.Time) {
	w.now = clock
}

// ─────────────────────────────────────────────────────────────────────────
// EnsureAccount
// ─────────────────────────────────────────────────────────────────────────

func (w *DBWallet) EnsureAccount(ctx context.Context, account AccountID, kind AccountKind, ownerSubject string) error {
	if account == "" {
		return errors.New("wallet: EnsureAccount requires non-empty account")
	}
	if kind == "" {
		return errors.New("wallet: EnsureAccount requires non-empty kind")
	}
	now := w.now()
	return runTx(ctx, w.db, w.dialect, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, w.dialect.InsertAccountSQL(),
			string(account), string(kind), nullableString(ownerSubject), now)
		return err
	})
}

// ─────────────────────────────────────────────────────────────────────────
// Hold
// ─────────────────────────────────────────────────────────────────────────

func (w *DBWallet) Hold(ctx context.Context, account AccountID, taskID string, cost adapter.CostUSD, idem adapter.IdempotencyKey) (string, error) {
	if account == "" {
		return "", errors.New("wallet: Hold requires non-empty account")
	}
	if taskID == "" {
		return "", errors.New("wallet: Hold requires non-empty taskID")
	}
	if cost <= 0 {
		return "", errors.New("wallet: Hold requires positive cost")
	}
	if cost > adapter.MaxCostUSD {
		return "", ErrCostCeilingExceeded
	}
	if idem == "" {
		return "", errors.New("wallet: Hold requires non-empty idempotency key")
	}

	escrowID := EscrowID(taskID)
	now := w.now()

	err := runTx(ctx, w.db, w.dialect, func(tx *sql.Tx) error {
		// Idempotency: if this idem+hold already wrote ledger rows, no-op.
		if existing, err := w.findOpByIdempotency(ctx, tx, string(idem), ReasonHold); err != nil {
			return err
		} else if existing {
			return nil
		}

		// Ensure both accounts exist (idempotent).
		if _, err := tx.ExecContext(ctx, w.dialect.InsertAccountSQL(),
			string(account), string(AccountKindUserWallet), string(account), now); err != nil {
			return fmt.Errorf("wallet: ensure user account: %w", err)
		}
		if _, err := tx.ExecContext(ctx, w.dialect.InsertAccountSQL(),
			escrowID, string(AccountKindTaskEscrow), taskID, now); err != nil {
			return fmt.Errorf("wallet: ensure escrow account: %w", err)
		}

		// Lock the user account row — readers + writers serialize on this.
		if _, err := tx.ExecContext(ctx, w.dialect.LockAccountByIDSQL(), string(account)); err != nil {
			return fmt.Errorf("wallet: lock account: %w", err)
		}

		// Read current balance and check sufficiency.
		var balance int64
		if err := tx.QueryRowContext(ctx, w.dialect.SelectBalanceSQL(), string(account)).Scan(&balance); err != nil {
			return fmt.Errorf("wallet: read balance: %w", err)
		}
		if balance < int64(cost) {
			return ErrInsufficientBalance
		}

		// Write the paired ledger rows. Same operation_id so they're a unit.
		// IDEMPOTENCY: only the FIRST row carries ref_idempotency; the second
		// stores NULL. This satisfies the partial-unique-index on
		// (ref_idempotency, reason_code) WHERE ref_idempotency IS NOT NULL
		// while keeping findOpByIdempotency O(1) (it finds the first row).
		opID := uuid.NewString()
		if err := w.insertLedger(ctx, tx, opID, string(account), -int64(cost), ReasonHold, taskID, string(idem), now); err != nil {
			return err
		}
		if err := w.insertLedger(ctx, tx, opID, escrowID, +int64(cost), ReasonHold, taskID, "", now); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return escrowID, nil
}

// ─────────────────────────────────────────────────────────────────────────
// Settle / Refund / PartialRefund
// ─────────────────────────────────────────────────────────────────────────

func (w *DBWallet) Settle(ctx context.Context, escrowID string, actualCost adapter.CostUSD) error {
	if escrowID == "" {
		return errors.New("wallet: Settle requires non-empty escrowID")
	}
	if actualCost < 0 {
		return errors.New("wallet: Settle requires non-negative actualCost")
	}
	now := w.now()
	taskID := strings.TrimPrefix(escrowID, "escrow:")
	idem := taskID + ":settle"

	return runTx(ctx, w.db, w.dialect, func(tx *sql.Tx) error {
		if existing, err := w.findOpByIdempotency(ctx, tx, idem, ReasonSettleDrain); err != nil {
			return err
		} else if existing {
			return nil
		}

		// Read held amount from escrow balance (the +cost row from Hold).
		var held int64
		if err := tx.QueryRowContext(ctx, w.dialect.SelectBalanceSQL(), escrowID).Scan(&held); err != nil {
			return fmt.Errorf("wallet: read escrow balance: %w", err)
		}
		if held == 0 {
			return ErrEscrowAlreadySettled
		}
		if held < 0 {
			// escrow went negative — invariant violation. Surface as bug.
			return fmt.Errorf("wallet: escrow %s has negative balance %d (invariant violated)", escrowID, held)
		}

		actual := int64(actualCost)
		if actual > held {
			actual = held // cap at held; over-settle is a programmer error but safer to clamp
		}

		// Op 1: drain escrow → revenue at actual amount.
		opID := uuid.NewString()
		if err := w.insertLedger(ctx, tx, opID, escrowID, -actual, ReasonSettleDrain, taskID, "", now); err != nil {
			return err
		}
		if err := w.insertLedger(ctx, tx, opID, string(SystemAccountRevenue), +actual, ReasonSettleDrain, taskID, "", now); err != nil {
			return err
		}
		// Ensure revenue account exists (idempotent insert).
		if _, err := tx.ExecContext(ctx, w.dialect.InsertAccountSQL(),
			string(SystemAccountRevenue), string(AccountKindModelhubRevenue), nullableString(""), now); err != nil {
			return fmt.Errorf("wallet: ensure revenue account: %w", err)
		}

		// Op 2: if held > actual, refund difference to the user wallet.
		if remaining := held - actual; remaining > 0 {
			refundIdem := taskID + ":settle_overheld"
			refundOpID := uuid.NewString()
			user := userAccountFromTaskID(ctx, tx, w.dialect, taskID)
			if user == "" {
				return fmt.Errorf("wallet: cannot determine user account for task %s", taskID)
			}
			if err := w.insertLedger(ctx, tx, refundOpID, escrowID, -remaining, ReasonSettleOverRefund, taskID, refundIdem, now); err != nil {
				return err
			}
			if err := w.insertLedger(ctx, tx, refundOpID, string(user), +remaining, ReasonSettleOverRefund, taskID, "", now); err != nil {
				return err
			}
		}
		return nil
	})
}

func (w *DBWallet) Refund(ctx context.Context, escrowID string) error {
	if escrowID == "" {
		return errors.New("wallet: Refund requires non-empty escrowID")
	}
	now := w.now()
	taskID := strings.TrimPrefix(escrowID, "escrow:")
	idem := taskID + ":refund"

	return runTx(ctx, w.db, w.dialect, func(tx *sql.Tx) error {
		if existing, err := w.findOpByIdempotency(ctx, tx, idem, ReasonRefundFull); err != nil {
			return err
		} else if existing {
			return nil
		}

		var held int64
		if err := tx.QueryRowContext(ctx, w.dialect.SelectBalanceSQL(), escrowID).Scan(&held); err != nil {
			return fmt.Errorf("wallet: read escrow balance: %w", err)
		}
		if held == 0 {
			return ErrEscrowAlreadySettled
		}
		if held < 0 {
			return fmt.Errorf("wallet: escrow %s has negative balance %d (invariant violated)", escrowID, held)
		}

		user := userAccountFromTaskID(ctx, tx, w.dialect, taskID)
		if user == "" {
			return fmt.Errorf("wallet: cannot determine user account for task %s", taskID)
		}

		opID := uuid.NewString()
		if err := w.insertLedger(ctx, tx, opID, escrowID, -held, ReasonRefundFull, taskID, "", now); err != nil {
			return err
		}
		if err := w.insertLedger(ctx, tx, opID, string(user), +held, ReasonRefundFull, taskID, "", now); err != nil {
			return err
		}
		return nil
	})
}

func (w *DBWallet) PartialRefund(ctx context.Context, escrowID string, assetCostFraction float64) error {
	if escrowID == "" {
		return errors.New("wallet: PartialRefund requires non-empty escrowID")
	}
	if assetCostFraction < 0 || assetCostFraction > 1 {
		return errors.New("wallet: assetCostFraction must be in [0,1]")
	}
	now := w.now()
	taskID := strings.TrimPrefix(escrowID, "escrow:")
	idem := taskID + ":partial"

	return runTx(ctx, w.db, w.dialect, func(tx *sql.Tx) error {
		if existing, err := w.findOpByIdempotency(ctx, tx, idem, ReasonAssetPartialDrain); err != nil {
			return err
		} else if existing {
			return nil
		}

		var held int64
		if err := tx.QueryRowContext(ctx, w.dialect.SelectBalanceSQL(), escrowID).Scan(&held); err != nil {
			return fmt.Errorf("wallet: read escrow balance: %w", err)
		}
		if held == 0 {
			return ErrEscrowAlreadySettled
		}
		if held < 0 {
			return fmt.Errorf("wallet: escrow %s has negative balance %d", escrowID, held)
		}

		// Round assetPortion DOWN so we never over-refund. computePortion = held - assetPortion.
		assetPortion := int64(float64(held) * assetCostFraction)
		computePortion := held - assetPortion

		user := userAccountFromTaskID(ctx, tx, w.dialect, taskID)
		if user == "" {
			return fmt.Errorf("wallet: cannot determine user account for task %s", taskID)
		}

		// Op 1: compute portion → revenue.
		if computePortion > 0 {
			opID := uuid.NewString()
			if err := w.insertLedger(ctx, tx, opID, escrowID, -computePortion, ReasonAssetPartialDrain, taskID, "", now); err != nil {
				return err
			}
			if err := w.insertLedger(ctx, tx, opID, string(SystemAccountRevenue), +computePortion, ReasonAssetPartialDrain, taskID, "", now); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, w.dialect.InsertAccountSQL(),
				string(SystemAccountRevenue), string(AccountKindModelhubRevenue), nullableString(""), now); err != nil {
				return fmt.Errorf("wallet: ensure revenue account: %w", err)
			}
		}

		// Op 2: asset portion → user refund.
		if assetPortion > 0 {
			opID := uuid.NewString()
			refundIdem := taskID + ":partial_refund"
			if err := w.insertLedger(ctx, tx, opID, escrowID, -assetPortion, ReasonAssetPartialBack, taskID, refundIdem, now); err != nil {
				return err
			}
			if err := w.insertLedger(ctx, tx, opID, string(user), +assetPortion, ReasonAssetPartialBack, taskID, "", now); err != nil {
				return err
			}
		}
		return nil
	})
}

// ─────────────────────────────────────────────────────────────────────────
// Topup
// ─────────────────────────────────────────────────────────────────────────

func (w *DBWallet) Topup(ctx context.Context, account AccountID, amount adapter.CostUSD, note string, adminUserID string, idem adapter.IdempotencyKey) error {
	if account == "" {
		return errors.New("wallet: Topup requires non-empty account")
	}
	if amount <= 0 || amount > MaxTopupUSD {
		return ErrInvalidTopupAmount
	}
	if adminUserID == "" {
		return errors.New("wallet: Topup requires adminUserID")
	}
	now := w.now()
	idemStr := string(idem)

	return runTx(ctx, w.db, w.dialect, func(tx *sql.Tx) error {
		if idemStr != "" {
			if existing, err := w.findOpByIdempotency(ctx, tx, idemStr, ReasonTopup); err != nil {
				return err
			} else if existing {
				return nil
			}
		}

		// Ensure both accounts exist.
		if _, err := tx.ExecContext(ctx, w.dialect.InsertAccountSQL(),
			string(account), string(AccountKindUserWallet), string(account), now); err != nil {
			return fmt.Errorf("wallet: ensure user account: %w", err)
		}
		if _, err := tx.ExecContext(ctx, w.dialect.InsertAccountSQL(),
			string(SystemAccountTopupSource), string(AccountKindExternalTopup), nullableString(""), now); err != nil {
			return fmt.Errorf("wallet: ensure topup source account: %w", err)
		}

		opID := uuid.NewString()
		amt := int64(amount)
		if err := w.insertLedger(ctx, tx, opID, string(SystemAccountTopupSource), -amt, ReasonTopup, "", idemStr, now); err != nil {
			return err
		}
		if err := w.insertLedger(ctx, tx, opID, string(account), +amt, ReasonTopup, "", "", now); err != nil {
			return err
		}

		// Audit row (separate from the ledger; ledger stays clean of PII).
		if _, err := tx.ExecContext(ctx, w.dialect.InsertTopupAuditSQL(),
			opID, string(account), amt, nullableString(note), adminUserID, now); err != nil {
			return fmt.Errorf("wallet: insert topup audit: %w", err)
		}
		return nil
	})
}

// ─────────────────────────────────────────────────────────────────────────
// Balance
// ─────────────────────────────────────────────────────────────────────────

func (w *DBWallet) Balance(ctx context.Context, account AccountID) (adapter.CostUSD, error) {
	if account == "" {
		return 0, errors.New("wallet: Balance requires non-empty account")
	}
	var balance int64
	err := w.db.QueryRowContext(ctx, w.dialect.SelectBalanceSQL(), string(account)).Scan(&balance)
	if err != nil {
		return 0, fmt.Errorf("wallet: read balance: %w", err)
	}
	return adapter.CostUSD(balance), nil
}

// InvariantSum returns SUM(amount_micro_usd) over the entire ledger.
// MUST always be 0 (I1). Exposed for tests + production monitoring.
func (w *DBWallet) InvariantSum(ctx context.Context) (int64, error) {
	var total int64
	err := w.db.QueryRowContext(ctx, w.dialect.SelectInvariantSumSQL()).Scan(&total)
	return total, err
}

// ─────────────────────────────────────────────────────────────────────────
// Helpers (unexported)
// ─────────────────────────────────────────────────────────────────────────

// EscrowID returns the canonical escrow account id for a task.
func EscrowID(taskID string) string {
	return "escrow:" + taskID
}

// UserAccountID returns the canonical user wallet account id.
func UserAccountID(userID string) AccountID {
	return AccountID("user:" + userID)
}

// findOpByIdempotency reports whether a ledger row already exists for the
// given (idempotency, reason) pair. Used by every write method to short-circuit
// replays.
func (w *DBWallet) findOpByIdempotency(ctx context.Context, tx *sql.Tx, idem string, reason string) (bool, error) {
	if idem == "" {
		return false, nil
	}
	var (
		opID, accountID, reasonOut string
		amount                     int64
		refTaskID, refIdem         sql.NullString
		createdAt                  time.Time
	)
	err := tx.QueryRowContext(ctx, w.dialect.SelectLedgerByIdempotencyAndReasonSQL(),
		idem, reason).Scan(&opID, &accountID, &amount, &reasonOut, &refTaskID, &refIdem, &createdAt)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return false, fmt.Errorf("wallet: idempotency lookup: %w", err)
}

// insertLedger writes a single row to wallet_ledger. Caller is responsible for
// the paired second row that closes the operation.
func (w *DBWallet) insertLedger(ctx context.Context, tx *sql.Tx, opID, accountID string, amount int64, reason, refTaskID, refIdem string, now time.Time) error {
	_, err := tx.ExecContext(ctx, w.dialect.InsertLedgerSQL(),
		opID, accountID, amount, reason,
		nullableString(refTaskID), nullableString(refIdem), now)
	if err != nil {
		return fmt.Errorf("wallet: insert ledger: %w", err)
	}
	return nil
}

// userAccountFromTaskID infers the user wallet id from the most recent
// Hold operation for this task. Hold writes (user_wallet, -cost) +
// (escrow, +cost) sharing operation_id; we look up the negative-amount row.
//
// Returns "" if no Hold found (programmer error elsewhere).
func userAccountFromTaskID(ctx context.Context, tx *sql.Tx, dialect Dialect, taskID string) AccountID {
	const q = `
		SELECT account_id FROM wallet_ledger
		 WHERE ref_task_id = $1
		   AND reason_code = 'hold'
		   AND amount_micro_usd < 0
		 ORDER BY created_at ASC
		 LIMIT 1
	`
	var accountID string
	if err := tx.QueryRowContext(ctx, q, taskID).Scan(&accountID); err != nil {
		return ""
	}
	return AccountID(accountID)
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
