// Wallet dialect — per-database SQL strings.
//
// Mirrors the pattern in internal/task/dialect.go:
//   - Postgres (production): UUID, BIGSERIAL, TIMESTAMPTZ, partial indexes,
//     SERIALIZABLE transactions.
//   - SQLite (modernc.org/sqlite, no CGO) for tests: TEXT for UUIDs,
//     INTEGER PRIMARY KEY AUTOINCREMENT, DATETIME columns, default
//     isolation (SQLite serializes writers).
//
// The SQL surface area is intentionally narrow — every wallet write goes
// through one of:
//   - InsertAccount: idempotent CREATE for an account row.
//   - InsertLedger: append a single ledger row inside an op.
//   - SelectBalance: SUM(amount_micro_usd) WHERE account_id = ?.
//   - SelectInvariantSum: SUM(amount_micro_usd) over the entire ledger.
//   - SelectLedgerByIdempotency: idempotency lookup for replay short-circuit.
//   - InsertTopupAudit: compliance row written alongside a topup op.
//   - LockAccountByID: SELECT FOR UPDATE on the account before reading
//     balance, so a Hold's "balance >= cost" check is linearizable.
//
// Both dialects use the same parameter placeholder syntax ($N) since
// modernc.org/sqlite supports it.

package wallet

// Dialect bundles all SQL strings that may differ between database backends.
type Dialect interface {
	InsertAccountSQL() string
	SelectAccountByIDSQL() string
	LockAccountByIDSQL() string
	InsertLedgerSQL() string
	SelectBalanceSQL() string
	SelectInvariantSumSQL() string
	SelectLedgerByIdempotencyAndReasonSQL() string
	SelectLedgerForAccountSQL() string
	InsertTopupAuditSQL() string
	CreateSchemaSQL() []string
}

// ─────────────────────────────────────────────────────────────────────────
// Postgres
// ─────────────────────────────────────────────────────────────────────────

// PostgresDialect is the production target.
type PostgresDialect struct{}

func (PostgresDialect) InsertAccountSQL() string {
	// ON CONFLICT DO NOTHING makes the helper idempotent — first writer
	// creates the row, later callers no-op. Required for Hold-creating-
	// escrow paths that must tolerate replay.
	return `
		INSERT INTO wallet_account (id, kind, owner_subject, created_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (id) DO NOTHING
	`
}

func (PostgresDialect) SelectAccountByIDSQL() string {
	return `SELECT id, kind, owner_subject, created_at FROM wallet_account WHERE id = $1`
}

func (PostgresDialect) LockAccountByIDSQL() string {
	return `SELECT id, kind, owner_subject, created_at FROM wallet_account WHERE id = $1 FOR UPDATE`
}

func (PostgresDialect) InsertLedgerSQL() string {
	return `
		INSERT INTO wallet_ledger (
			operation_id, account_id, amount_micro_usd, reason_code,
			ref_task_id, ref_idempotency, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
	`
}

func (PostgresDialect) SelectBalanceSQL() string {
	return `
		SELECT COALESCE(SUM(amount_micro_usd), 0)
		  FROM wallet_ledger
		 WHERE account_id = $1
	`
}

func (PostgresDialect) SelectInvariantSumSQL() string {
	return `SELECT COALESCE(SUM(amount_micro_usd), 0) FROM wallet_ledger`
}

func (PostgresDialect) SelectLedgerByIdempotencyAndReasonSQL() string {
	return `
		SELECT operation_id, account_id, amount_micro_usd, reason_code,
		       ref_task_id, ref_idempotency, created_at
		  FROM wallet_ledger
		 WHERE ref_idempotency = $1 AND reason_code = $2
		 LIMIT 1
	`
}

func (PostgresDialect) SelectLedgerForAccountSQL() string {
	return `
		SELECT operation_id, account_id, amount_micro_usd, reason_code,
		       ref_task_id, ref_idempotency, created_at
		  FROM wallet_ledger
		 WHERE account_id = $1
		 ORDER BY created_at ASC, id ASC
	`
}

func (PostgresDialect) InsertTopupAuditSQL() string {
	return `
		INSERT INTO topup_audit (op_id, account_id, amount, note, admin_user_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
	`
}

// CreateSchemaSQL is unused in production (goose owns the real schema).
// Returned as one-shot for tests that don't go through goose.
func (PostgresDialect) CreateSchemaSQL() []string {
	return []string{
		// Postgres-only: TYPE and full schema would mirror the migration.
		// Intentionally not maintained here — production uses goose, and
		// the SQLite path is what tests in this package use. If a
		// Postgres-backed integration test is ever added, it should run
		// the goose migrations rather than relying on this method.
	}
}

// ─────────────────────────────────────────────────────────────────────────
// SQLite (test backbone)
// ─────────────────────────────────────────────────────────────────────────

// SQLiteDialect targets modernc.org/sqlite (pure-Go, no CGO).
//
// Differences from Postgres:
//   - UUID → TEXT (we generate uuid.NewString()).
//   - BIGSERIAL → INTEGER PRIMARY KEY AUTOINCREMENT.
//   - TIMESTAMPTZ → DATETIME (RFC3339 strings).
//   - account_kind ENUM → TEXT with a CHECK constraint.
//   - FOR UPDATE has no effect — SQLite serializes writers, BeginTx
//     already holds the write lock.
//   - ON CONFLICT works the same way (since SQLite 3.24+ which modernc embeds).
type SQLiteDialect struct{}

func (SQLiteDialect) InsertAccountSQL() string {
	return `
		INSERT INTO wallet_account (id, kind, owner_subject, created_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (id) DO NOTHING
	`
}

func (SQLiteDialect) SelectAccountByIDSQL() string {
	return `SELECT id, kind, owner_subject, created_at FROM wallet_account WHERE id = $1`
}

// LockAccountByIDSQL: SQLite has no FOR UPDATE; the BeginTx already
// holds the write lock for the connection.
func (SQLiteDialect) LockAccountByIDSQL() string {
	return `SELECT id, kind, owner_subject, created_at FROM wallet_account WHERE id = $1`
}

func (SQLiteDialect) InsertLedgerSQL() string {
	return `
		INSERT INTO wallet_ledger (
			operation_id, account_id, amount_micro_usd, reason_code,
			ref_task_id, ref_idempotency, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
	`
}

func (SQLiteDialect) SelectBalanceSQL() string {
	return `
		SELECT COALESCE(SUM(amount_micro_usd), 0)
		  FROM wallet_ledger
		 WHERE account_id = $1
	`
}

func (SQLiteDialect) SelectInvariantSumSQL() string {
	return `SELECT COALESCE(SUM(amount_micro_usd), 0) FROM wallet_ledger`
}

func (SQLiteDialect) SelectLedgerByIdempotencyAndReasonSQL() string {
	return `
		SELECT operation_id, account_id, amount_micro_usd, reason_code,
		       ref_task_id, ref_idempotency, created_at
		  FROM wallet_ledger
		 WHERE ref_idempotency = $1 AND reason_code = $2
		 LIMIT 1
	`
}

func (SQLiteDialect) SelectLedgerForAccountSQL() string {
	return `
		SELECT operation_id, account_id, amount_micro_usd, reason_code,
		       ref_task_id, ref_idempotency, created_at
		  FROM wallet_ledger
		 WHERE account_id = $1
		 ORDER BY created_at ASC, id ASC
	`
}

func (SQLiteDialect) InsertTopupAuditSQL() string {
	return `
		INSERT INTO topup_audit (op_id, account_id, amount, note, admin_user_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
	`
}

// CreateSchemaSQL emits the SQLite-flavoured schema for tests.
func (SQLiteDialect) CreateSchemaSQL() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS wallet_account (
			id            TEXT PRIMARY KEY,
			kind          TEXT NOT NULL CHECK (kind IN (
				'user_wallet', 'task_escrow', 'modelhub_revenue', 'external_topup'
			)),
			owner_subject TEXT,
			created_at    DATETIME NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS wallet_account_kind_idx
			ON wallet_account (kind)`,
		`CREATE INDEX IF NOT EXISTS wallet_account_owner_idx
			ON wallet_account (owner_subject)`,
		`CREATE TABLE IF NOT EXISTS wallet_ledger (
			id                INTEGER PRIMARY KEY AUTOINCREMENT,
			operation_id      TEXT NOT NULL,
			account_id        TEXT NOT NULL REFERENCES wallet_account(id),
			amount_micro_usd  INTEGER NOT NULL,
			reason_code       TEXT NOT NULL,
			ref_task_id       TEXT,
			ref_idempotency   TEXT,
			created_at        DATETIME NOT NULL,
			CHECK (amount_micro_usd <> 0)
		)`,
		`CREATE INDEX IF NOT EXISTS wallet_ledger_op_idx
			ON wallet_ledger (operation_id)`,
		`CREATE INDEX IF NOT EXISTS wallet_ledger_account_idx
			ON wallet_ledger (account_id, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS wallet_ledger_task_idx
			ON wallet_ledger (ref_task_id) WHERE ref_task_id IS NOT NULL`,
		`CREATE UNIQUE INDEX IF NOT EXISTS wallet_ledger_idempotency_idx
			ON wallet_ledger (ref_idempotency, reason_code)
			WHERE ref_idempotency IS NOT NULL`,
		`CREATE TABLE IF NOT EXISTS topup_audit (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			op_id           TEXT NOT NULL,
			account_id      TEXT NOT NULL REFERENCES wallet_account(id),
			amount          INTEGER NOT NULL,
			note            TEXT,
			admin_user_id   TEXT NOT NULL,
			created_at      DATETIME NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS topup_audit_account_idx
			ON topup_audit (account_id, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS topup_audit_admin_idx
			ON topup_audit (admin_user_id, created_at DESC)`,
	}
}
