// Wallet transaction helper.
//
// Per S6-WALLET-DESIGN.md §4 (and AP-6) every wallet write must run
// inside a SERIALIZABLE-or-equivalent transaction with retry on
// serialization-failure.
//
// On Postgres SERIALIZABLE returns SQLSTATE 40001 (also 40P01 for
// deadlock_detected) when the system detects a conflict. The CALLER
// must retry. Without retry, occasional concurrent operations would
// surface as user-visible 5xx errors.
//
// On SQLite the database serializes writers natively — there is
// effectively only one writer at a time. We still wrap fn in BeginTx so
// the rest of the wallet code path looks identical. SQLite returns
// "database is locked" on contention; we retry that too.
//
// 5 retries with exponential backoff + jitter is empirically enough for
// normal contention. Beyond that signals a systemic problem (alert ops);
// callers receive ErrConcurrencyExhausted.

package wallet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	mathrand "math/rand"
	"strings"
	"time"
)

const maxTxRetries = 5

// runTx executes fn within a transaction with retry on serialization
// failure. On Postgres BeginTx is invoked with sql.LevelSerializable.
// On SQLite the fallback default isolation is used (SQLite serializes
// writers natively).
//
// The dialect parameter selects between SERIALIZABLE (Postgres) and
// default isolation (SQLite). Type-asserting on PostgresDialect is the
// minimal coupling — there is no broader Dialect.IsolationLevel()
// abstraction because the retry logic is the only place that cares.
func runTx(ctx context.Context, db *sql.DB, dialect Dialect, fn func(*sql.Tx) error) error {
	var lastErr error
	for attempt := 0; attempt < maxTxRetries; attempt++ {
		if attempt > 0 {
			// Backoff + jitter: 10ms, 30ms, 70ms, 150ms typical.
			delay := time.Duration(10*float64(attempt*attempt+attempt)) * time.Millisecond
			jitter := time.Duration(0)
			if delay > 0 {
				jitter = time.Duration(mathrand.Int63n(int64(delay)/2 + 1))
			}
			select {
			case <-time.After(delay + jitter):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		opts := txOptionsFor(dialect)
		tx, err := db.BeginTx(ctx, opts)
		if err != nil {
			if isRetryableTxError(err) {
				lastErr = err
				continue
			}
			return fmt.Errorf("wallet: begin tx: %w", err)
		}
		err = fn(tx)
		if err != nil {
			_ = tx.Rollback()
			if errors.Is(err, ErrEscrowAlreadySettled) ||
				errors.Is(err, ErrInsufficientBalance) ||
				errors.Is(err, ErrInvalidTopupAmount) ||
				errors.Is(err, ErrAccountNotFound) {
				return err
			}
			if isRetryableTxError(err) {
				lastErr = err
				continue
			}
			return err
		}
		if err := tx.Commit(); err != nil {
			if isRetryableTxError(err) {
				lastErr = err
				continue
			}
			return fmt.Errorf("wallet: commit: %w", err)
		}
		return nil
	}
	return fmt.Errorf("%w (%d attempts, last err: %v)",
		ErrConcurrencyExhausted, maxTxRetries, lastErr)
}

// txOptionsFor returns SERIALIZABLE on Postgres, default on SQLite.
//
// SQLite's modernc driver rejects sql.LevelSerializable in BeginTx —
// "isolation level not supported". We pass nil opts there, which gives
// SQLite's natural single-writer semantics (sufficient for tests).
func txOptionsFor(dialect Dialect) *sql.TxOptions {
	if _, ok := dialect.(PostgresDialect); ok {
		return &sql.TxOptions{Isolation: sql.LevelSerializable}
	}
	return nil
}

// isRetryableTxError reports whether err is a transient lock /
// serialization-conflict that warrants retrying the entire transaction.
//
// Match by error-message substring because the underlying typed errors
// live in driver-specific packages we deliberately don't import here:
//   - Postgres: SQLSTATE 40001 (serialization_failure), 40P01 (deadlock_detected),
//     plus the canonical "could not serialize access" string.
//   - SQLite: "database is locked", "database table is locked".
func isRetryableTxError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, sql.ErrConnDone) || errors.Is(err, sql.ErrTxDone) {
		return false
	}
	msg := err.Error()
	return containsAny(msg,
		"40001",
		"40P01",
		"could not serialize access",
		"deadlock detected",
		"database is locked",
		"database table is locked",
	)
}

func containsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}
