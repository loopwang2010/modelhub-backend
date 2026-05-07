// Dialect — per-database SQL strings.
//
// The Repo owns business logic; this file owns SQL syntax that varies
// between Postgres (production) and SQLite (tests). The two dialects
// implement the same set of statement-shaped queries — column order,
// parameter count, and return shape match across implementations so
// scanTask can be used interchangeably.
//
// Why two dialects:
//   - Postgres has SKIP LOCKED, partial-index BTREE, JSONB, TIMESTAMPTZ,
//     BIGSERIAL — production schema lives in migrations/0002.
//   - SQLite (modernc.org/sqlite, no CGO) is the test backbone — pure-Go,
//     no Docker dependency, runs on Windows out of the box.
//
// Dialect-specific divergences:
//   - ClaimNextTask: Postgres uses SELECT FOR UPDATE SKIP LOCKED; SQLite
//     uses a single-statement UPDATE...WHERE id=(SELECT...LIMIT 1)
//     which is safe because SQLite serializes writers.

package task

// Dialect bundles all SQL strings that may differ between database backends.
type Dialect interface {
	InsertTaskSQL() string
	InsertTaskEventSQL() string
	SelectTaskByIDSQL() string
	SelectTaskByWebhookTokenSQL() string
	SelectTaskByIdempotencyKeySQL() string
	LockTaskByIDSQL() string
	UpdateStateSQL() string
	UpdateHeldAmountSQL() string
	UpdateSubmittedSQL() string
	UpdateScheduleSQL() string
	UpdateActualCostAndTerminalSQL() string
	UpdateErrorAndTerminalSQL() string
	UpdateTerminalAtSQL() string
	ClaimNextTaskSQL() string
	FindStuckSQL() string
	FindTimedOutSQL() string
	ListEventsSQL() string
	CreateSchemaSQL() []string
}

// fullColumnList is the canonical ordering used by every SELECT.
// scanTask depends on this order; do NOT reorder without updating both.
const fullColumnList = `
	id, account_id, model_key, provider_key, state,
	params_json, idempotency_key, upstream_ref, polling_url,
	poll_attempt, next_poll_after, webhook_token, sla_deadline,
	last_error_class, raw_error,
	held_amount, actual_cost,
	submitted_at, terminal_at,
	created_at, updated_at
`

// ─────────────────────────────────────────────────────────────────────────
// Postgres
// ─────────────────────────────────────────────────────────────────────────

// PostgresDialect is the production target.
type PostgresDialect struct{}

func (PostgresDialect) InsertTaskSQL() string {
	return `
		INSERT INTO task (
			id, account_id, model_key, provider_key, state,
			params_json, idempotency_key, webhook_token,
			sla_deadline, held_amount,
			created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
	`
}

func (PostgresDialect) InsertTaskEventSQL() string {
	return `
		INSERT INTO task_event (task_id, from_state, to_state, reason, metadata, created_at)
		VALUES ($1,$2,$3,$4,$5,$6)
	`
}

func (PostgresDialect) SelectTaskByIDSQL() string {
	return `SELECT ` + fullColumnList + ` FROM task WHERE id = $1`
}

func (PostgresDialect) SelectTaskByWebhookTokenSQL() string {
	return `SELECT ` + fullColumnList + ` FROM task WHERE webhook_token = $1`
}

func (PostgresDialect) SelectTaskByIdempotencyKeySQL() string {
	return `SELECT ` + fullColumnList + ` FROM task WHERE idempotency_key = $1`
}

func (PostgresDialect) LockTaskByIDSQL() string {
	return `SELECT ` + fullColumnList + ` FROM task WHERE id = $1 FOR UPDATE`
}

func (PostgresDialect) UpdateStateSQL() string {
	return `UPDATE task SET state = $1, updated_at = $2 WHERE id = $3`
}

func (PostgresDialect) UpdateHeldAmountSQL() string {
	return `UPDATE task SET held_amount = $1, updated_at = NOW() WHERE id = $2`
}

func (PostgresDialect) UpdateSubmittedSQL() string {
	return `UPDATE task SET upstream_ref = $1, polling_url = $2, submitted_at = $3, updated_at = NOW() WHERE id = $4`
}

func (PostgresDialect) UpdateScheduleSQL() string {
	return `UPDATE task SET next_poll_after = $1, poll_attempt = $2, updated_at = NOW() WHERE id = $3`
}

func (PostgresDialect) UpdateActualCostAndTerminalSQL() string {
	return `UPDATE task SET actual_cost = $1, terminal_at = $2, updated_at = NOW() WHERE id = $3`
}

func (PostgresDialect) UpdateErrorAndTerminalSQL() string {
	return `UPDATE task SET last_error_class = $1, raw_error = $2, terminal_at = $3, updated_at = NOW() WHERE id = $4`
}

func (PostgresDialect) UpdateTerminalAtSQL() string {
	return `UPDATE task SET terminal_at = $1, updated_at = NOW() WHERE id = $2`
}

func (PostgresDialect) ClaimNextTaskSQL() string {
	return `
		UPDATE task
		   SET poll_attempt = poll_attempt + 1,
		       next_poll_after = NULL,
		       updated_at = NOW()
		 WHERE id = (
		     SELECT id FROM task
		      WHERE provider_key = $1
		        AND state IN ('submitted', 'running')
		        AND (next_poll_after IS NULL OR next_poll_after <= $2)
		      ORDER BY next_poll_after ASC NULLS FIRST
		      LIMIT 1
		      FOR UPDATE SKIP LOCKED
		 )
		RETURNING ` + fullColumnList
}

func (PostgresDialect) FindStuckSQL() string {
	return `
		SELECT ` + fullColumnList + `
		  FROM task
		 WHERE state IN ('submitted', 'running')
		   AND next_poll_after IS NOT NULL
		   AND next_poll_after < $1
		 LIMIT $2
	`
}

func (PostgresDialect) FindTimedOutSQL() string {
	return `
		SELECT ` + fullColumnList + `
		  FROM task
		 WHERE state IN ('held', 'submitted', 'running')
		   AND sla_deadline < $1
		 LIMIT $2
	`
}

func (PostgresDialect) ListEventsSQL() string {
	return `
		SELECT id, task_id, from_state, to_state, reason, metadata, created_at
		  FROM task_event
		 WHERE task_id = $1
		 ORDER BY created_at ASC, id ASC
	`
}

// CreateSchemaSQL is unused for Postgres in normal operation — production
// uses goose migrations. Returned as a one-shot for tests that want to
// stand up an in-memory Postgres without goose.
func (PostgresDialect) CreateSchemaSQL() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS task (
			id TEXT PRIMARY KEY,
			account_id TEXT NOT NULL,
			model_key TEXT NOT NULL,
			provider_key TEXT NOT NULL,
			state TEXT NOT NULL,
			params_json JSONB NOT NULL,
			idempotency_key TEXT,
			upstream_ref TEXT,
			polling_url TEXT,
			poll_attempt INT NOT NULL DEFAULT 0,
			next_poll_after TIMESTAMPTZ,
			webhook_token TEXT NOT NULL,
			sla_deadline TIMESTAMPTZ NOT NULL,
			last_error_class TEXT,
			raw_error BYTEA,
			held_amount BIGINT NOT NULL DEFAULT 0,
			actual_cost BIGINT NOT NULL DEFAULT 0,
			submitted_at TIMESTAMPTZ,
			terminal_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS task_webhook_idx ON task (webhook_token)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS task_idempotency_idx ON task (idempotency_key) WHERE idempotency_key IS NOT NULL`,
		`CREATE TABLE IF NOT EXISTS task_event (
			id BIGSERIAL PRIMARY KEY,
			task_id TEXT NOT NULL REFERENCES task(id),
			from_state TEXT NOT NULL,
			to_state TEXT NOT NULL,
			reason TEXT,
			metadata JSONB,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
	}
}

// ─────────────────────────────────────────────────────────────────────────
// SQLite (test backbone)
// ─────────────────────────────────────────────────────────────────────────

// SQLiteDialect targets the modernc.org/sqlite pure-Go driver.
//
// Differences from Postgres:
//   - Parameter placeholders are still $N (modernc supports them).
//   - SKIP LOCKED is dropped — sqlite serializes writers.
//   - TIMESTAMPTZ → TEXT (RFC3339 strings); JSONB → BLOB.
//   - BIGSERIAL → INTEGER PRIMARY KEY AUTOINCREMENT.
//
// All non-claim queries are identical syntax — only the schema and the
// claim path differ. No Repo logic depends on either.
type SQLiteDialect struct{}

func (SQLiteDialect) InsertTaskSQL() string {
	return `
		INSERT INTO task (
			id, account_id, model_key, provider_key, state,
			params_json, idempotency_key, webhook_token,
			sla_deadline, held_amount,
			created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
	`
}

func (SQLiteDialect) InsertTaskEventSQL() string {
	return `
		INSERT INTO task_event (task_id, from_state, to_state, reason, metadata, created_at)
		VALUES ($1,$2,$3,$4,$5,$6)
	`
}

func (SQLiteDialect) SelectTaskByIDSQL() string {
	return `SELECT ` + fullColumnList + ` FROM task WHERE id = $1`
}

func (SQLiteDialect) SelectTaskByWebhookTokenSQL() string {
	return `SELECT ` + fullColumnList + ` FROM task WHERE webhook_token = $1`
}

func (SQLiteDialect) SelectTaskByIdempotencyKeySQL() string {
	return `SELECT ` + fullColumnList + ` FROM task WHERE idempotency_key = $1`
}

// LockTaskByIDSQL: SQLite doesn't support FOR UPDATE; the BeginTx already
// holds an exclusive write lock for the duration.
func (SQLiteDialect) LockTaskByIDSQL() string {
	return `SELECT ` + fullColumnList + ` FROM task WHERE id = $1`
}

func (SQLiteDialect) UpdateStateSQL() string {
	return `UPDATE task SET state = $1, updated_at = $2 WHERE id = $3`
}

func (SQLiteDialect) UpdateHeldAmountSQL() string {
	return `UPDATE task SET held_amount = $1, updated_at = $2 WHERE id = $3`
}

func (SQLiteDialect) UpdateSubmittedSQL() string {
	return `UPDATE task SET upstream_ref = $1, polling_url = $2, submitted_at = $3, updated_at = $4 WHERE id = $5`
}

func (SQLiteDialect) UpdateScheduleSQL() string {
	return `UPDATE task SET next_poll_after = $1, poll_attempt = $2, updated_at = $3 WHERE id = $4`
}

func (SQLiteDialect) UpdateActualCostAndTerminalSQL() string {
	return `UPDATE task SET actual_cost = $1, terminal_at = $2, updated_at = $3 WHERE id = $4`
}

func (SQLiteDialect) UpdateErrorAndTerminalSQL() string {
	return `UPDATE task SET last_error_class = $1, raw_error = $2, terminal_at = $3, updated_at = $4 WHERE id = $5`
}

func (SQLiteDialect) UpdateTerminalAtSQL() string {
	return `UPDATE task SET terminal_at = $1, updated_at = $2 WHERE id = $3`
}

// ClaimNextTaskSQL: SQLite RETURNING is supported in 3.35+ which is what
// modernc embeds. Drops SKIP LOCKED — the BeginTx pattern + sqlite's
// single-writer model gives effectively the same guarantees in test.
func (SQLiteDialect) ClaimNextTaskSQL() string {
	return `
		UPDATE task
		   SET poll_attempt = poll_attempt + 1,
		       next_poll_after = NULL,
		       updated_at = $2
		 WHERE id = (
		     SELECT id FROM task
		      WHERE provider_key = $1
		        AND state IN ('submitted', 'running')
		        AND (next_poll_after IS NULL OR next_poll_after <= $2)
		      ORDER BY next_poll_after ASC
		      LIMIT 1
		 )
		RETURNING ` + fullColumnList
}

func (SQLiteDialect) FindStuckSQL() string {
	return `
		SELECT ` + fullColumnList + `
		  FROM task
		 WHERE state IN ('submitted', 'running')
		   AND next_poll_after IS NOT NULL
		   AND next_poll_after < $1
		 LIMIT $2
	`
}

func (SQLiteDialect) FindTimedOutSQL() string {
	return `
		SELECT ` + fullColumnList + `
		  FROM task
		 WHERE state IN ('held', 'submitted', 'running')
		   AND sla_deadline < $1
		 LIMIT $2
	`
}

func (SQLiteDialect) ListEventsSQL() string {
	return `
		SELECT id, task_id, from_state, to_state, reason, metadata, created_at
		  FROM task_event
		 WHERE task_id = $1
		 ORDER BY created_at ASC, id ASC
	`
}

// CreateSchemaSQL emits the schema as one statement per line so tests can
// stand up a fresh sqlite DB without parsing the goose migration file.
func (SQLiteDialect) CreateSchemaSQL() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS task (
			id TEXT PRIMARY KEY,
			account_id TEXT NOT NULL,
			model_key TEXT NOT NULL,
			provider_key TEXT NOT NULL,
			state TEXT NOT NULL,
			params_json BLOB NOT NULL,
			idempotency_key TEXT,
			upstream_ref TEXT,
			polling_url TEXT,
			poll_attempt INTEGER NOT NULL DEFAULT 0,
			next_poll_after DATETIME,
			webhook_token TEXT NOT NULL,
			sla_deadline DATETIME NOT NULL,
			last_error_class TEXT,
			raw_error BLOB,
			held_amount INTEGER NOT NULL DEFAULT 0,
			actual_cost INTEGER NOT NULL DEFAULT 0,
			submitted_at DATETIME,
			terminal_at DATETIME,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS task_webhook_idx ON task (webhook_token)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS task_idempotency_idx ON task (idempotency_key) WHERE idempotency_key IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS task_worker_claim_idx ON task (provider_key, state, next_poll_after)`,
		`CREATE INDEX IF NOT EXISTS task_reconciler_idx ON task (state, sla_deadline)`,
		`CREATE TABLE IF NOT EXISTS task_event (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			task_id TEXT NOT NULL,
			from_state TEXT NOT NULL,
			to_state TEXT NOT NULL,
			reason TEXT,
			metadata BLOB,
			created_at DATETIME NOT NULL,
			FOREIGN KEY (task_id) REFERENCES task(id)
		)`,
		`CREATE INDEX IF NOT EXISTS task_event_task_idx ON task_event (task_id, created_at)`,
	}
}
