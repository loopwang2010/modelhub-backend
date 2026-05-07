// Task repository — persistence layer over database/sql.
//
// Owns the row<->struct mapping for the `task` and `task_event` tables
// defined in migrations/0002_task_runtime_columns.sql. Worker, webhook
// handler, reconciler, and dedup wrapper all funnel state changes through
// here so every transition is audited in `task_event`.
//
// Dialect portability:
//   - Production target is Postgres (SKIP LOCKED, partial indexes, JSONB).
//   - Tests use sqlite (modernc.org/sqlite, no CGO) via a parallel
//     CreateTestSchema helper. SQL that differs by dialect (notably the
//     SKIP LOCKED claim) is dispatched through the Dialect interface.
//
// Concurrency:
//   - Every method takes a context; callers MUST set deadlines.
//   - ClaimNextTask is the SKIP-LOCKED path used by Worker.Run. Multiple
//     workers calling concurrently on Postgres each pick a different row.
//     On sqlite (single-writer), they serialize naturally.
//
// State transitions:
//   - All write methods that change `state` first AssertTransition() and
//     then write a row to task_event. This guarantees the audit log is
//     consistent with the live state.

package task

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	mathrand "math/rand"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/internal/adapter"
)

// Task is the in-memory shape of a row in the `task` table.
//
// Mutation rule: callers receive Task by *pointer* but MUST treat it as
// immutable inside their goroutine. State changes go through Repo methods
// that re-fetch — this prevents stale-state writes from racing.
type Task struct {
	ID              string
	AccountID       string
	ModelKey        adapter.ModelKey
	ProviderKey     adapter.ProviderKey
	State           TaskState
	ParamsJSON      json.RawMessage
	IdempotencyKey  string
	UpstreamRef     string
	PollingURL      string
	PollAttempt     int
	NextPollAfter   *time.Time
	WebhookToken    string
	SLADeadline     time.Time
	LastErrorClass  string
	RawError        []byte
	HeldAmount      adapter.CostUSD
	ActualCost      adapter.CostUSD
	SubmittedAt     *time.Time
	TerminalAt      *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// NewTaskParams is the input to Repo.Create.
type NewTaskParams struct {
	AccountID      string
	ModelKey       adapter.ModelKey
	ProviderKey    adapter.ProviderKey
	ParamsJSON     json.RawMessage
	IdempotencyKey string
	HeldAmount     adapter.CostUSD
	SLATimeout     time.Duration
}

// Repo is the persistence boundary for tasks.
//
// All methods take context.Context. Callers SHOULD wrap calls in a
// timeout (5–30 s typical) — there is no in-Repo retry loop.
type Repo struct {
	db      *sql.DB
	dialect Dialect
}

// NewRepo constructs a Repo for the given DB + dialect.
func NewRepo(db *sql.DB, dialect Dialect) *Repo {
	if db == nil {
		panic("task: NewRepo requires non-nil *sql.DB")
	}
	if dialect == nil {
		dialect = PostgresDialect{}
	}
	return &Repo{db: db, dialect: dialect}
}

// DB returns the underlying *sql.DB. Useful for tests that want to
// run raw queries; production code should not depend on this.
func (r *Repo) DB() *sql.DB { return r.db }

// Dialect returns the configured dialect.
func (r *Repo) Dialect() Dialect { return r.dialect }

// Create inserts a new task in StateCreated. Caller is expected to walk
// the FSM forward (Hold → Submit) by calling subsequent Repo methods.
//
// The task ID is auto-minted (gen_<32-hex>). The webhook_token is a
// 256-bit random per AP-18 — embedded in the webhook URL handed to the
// upstream provider.
//
// Idempotency: when params.IdempotencyKey is non-empty and a task with
// the same key already exists, returns the existing task and ErrIdempotentReplay.
// The caller MUST inspect this error and short-circuit. See dedup.go.
func (r *Repo) Create(ctx context.Context, p NewTaskParams) (*Task, error) {
	if err := validateCreateParams(p); err != nil {
		return nil, err
	}
	id, err := mintTaskID()
	if err != nil {
		return nil, fmt.Errorf("task: mint id: %w", err)
	}
	tok, err := mintWebhookToken()
	if err != nil {
		return nil, fmt.Errorf("task: mint webhook token: %w", err)
	}
	now := time.Now().UTC()
	deadline := now.Add(p.SLATimeout)
	t := &Task{
		ID:             id,
		AccountID:      p.AccountID,
		ModelKey:       p.ModelKey,
		ProviderKey:    p.ProviderKey,
		State:          StateCreated,
		ParamsJSON:     p.ParamsJSON,
		IdempotencyKey: p.IdempotencyKey,
		WebhookToken:   tok,
		SLADeadline:    deadline,
		HeldAmount:     p.HeldAmount,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	q := r.dialect.InsertTaskSQL()
	args := []any{
		t.ID, t.AccountID, string(t.ModelKey), string(t.ProviderKey),
		string(t.State), []byte(t.ParamsJSON),
		nullableString(t.IdempotencyKey), t.WebhookToken,
		t.SLADeadline.UTC(), int64(t.HeldAmount),
		t.CreatedAt.UTC(), t.UpdatedAt.UTC(),
	}
	_, err = r.db.ExecContext(ctx, q, args...)
	if err != nil {
		// Detect idempotency conflict by re-querying. This is portable
		// across Postgres / sqlite without sniffing driver-specific error
		// types.
		if p.IdempotencyKey != "" {
			if existing, ferr := r.FindByIdempotencyKey(ctx, p.IdempotencyKey); ferr == nil && existing != nil {
				return existing, ErrIdempotentReplay
			}
		}
		return nil, fmt.Errorf("task: insert: %w", err)
	}
	if err := r.appendEvent(ctx, nil, t.ID, "", string(StateCreated), "created", nil); err != nil {
		return nil, fmt.Errorf("task: append created event: %w", err)
	}
	return t, nil
}

// FindByID returns the task by primary key, or (nil, nil) if not found.
func (r *Repo) FindByID(ctx context.Context, id string) (*Task, error) {
	q := r.dialect.SelectTaskByIDSQL()
	row := r.db.QueryRowContext(ctx, q, id)
	t, err := scanTask(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return t, nil
}

// FindByWebhookToken returns the task whose webhook_token equals tok.
// Used by the webhook receiver per AP-18 (look up by token, not task_id).
func (r *Repo) FindByWebhookToken(ctx context.Context, tok string) (*Task, error) {
	if tok == "" {
		return nil, nil
	}
	q := r.dialect.SelectTaskByWebhookTokenSQL()
	row := r.db.QueryRowContext(ctx, q, tok)
	t, err := scanTask(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return t, nil
}

// FindByIdempotencyKey returns the task whose idempotency_key equals key.
func (r *Repo) FindByIdempotencyKey(ctx context.Context, key string) (*Task, error) {
	if key == "" {
		return nil, nil
	}
	q := r.dialect.SelectTaskByIdempotencyKeySQL()
	row := r.db.QueryRowContext(ctx, q, key)
	t, err := scanTask(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return t, nil
}

// MarkHeld transitions Created → Held with the given held amount.
func (r *Repo) MarkHeld(ctx context.Context, id string, held adapter.CostUSD) error {
	return r.transition(ctx, id, StateHeld, "held", func(tx *sql.Tx, t *Task, now time.Time) error {
		_, err := tx.ExecContext(ctx, r.dialect.UpdateHeldAmountSQL(), int64(held), now, id)
		return err
	})
}

// MarkSubmittedAsync transitions Held → Submitted and persists the
// upstream ref + polling URL returned by adapter.Submit.
func (r *Repo) MarkSubmittedAsync(ctx context.Context, id string, ref adapter.UpstreamRef, pollingURL string, submittedAt time.Time) error {
	return r.transition(ctx, id, StateSubmitted, "submitted", func(tx *sql.Tx, t *Task, now time.Time) error {
		_, err := tx.ExecContext(ctx, r.dialect.UpdateSubmittedSQL(),
			string(ref), nullableString(pollingURL), submittedAt.UTC(), now, id)
		return err
	})
}

// MarkRunning transitions Submitted → Running.
func (r *Repo) MarkRunning(ctx context.Context, id string) error {
	return r.transition(ctx, id, StateRunning, "running", nil)
}

// SchedulePoll advances poll_attempt and sets next_poll_after.
// Does NOT change state (used between Submitted/Running while task
// remains in flight).
func (r *Repo) SchedulePoll(ctx context.Context, id string, nextPoll time.Time, attempt int) error {
	q := r.dialect.UpdateScheduleSQL()
	now := time.Now().UTC()
	_, err := r.db.ExecContext(ctx, q, nextPoll.UTC(), attempt, now, id)
	if err != nil {
		return fmt.Errorf("task: schedule poll: %w", err)
	}
	return nil
}

// MarkSucceeded transitions to Succeeded with the actual cost.
func (r *Repo) MarkSucceeded(ctx context.Context, id string, actual adapter.CostUSD) error {
	return r.transition(ctx, id, StateSucceeded, "succeeded", func(tx *sql.Tx, t *Task, now time.Time) error {
		_, err := tx.ExecContext(ctx, r.dialect.UpdateActualCostAndTerminalSQL(),
			int64(actual), now, now, id)
		return err
	})
}

// MarkFailed transitions to Failed with the supplied error class + raw body.
func (r *Repo) MarkFailed(ctx context.Context, id string, class adapter.ErrorClass, msg string, raw []byte) error {
	return r.transition(ctx, id, StateFailed, msg, func(tx *sql.Tx, t *Task, now time.Time) error {
		_, err := tx.ExecContext(ctx, r.dialect.UpdateErrorAndTerminalSQL(),
			string(class), capRaw(raw), now, now, id)
		return err
	})
}

// MarkTimedOut transitions to TimedOut. Used by the reconciler.
func (r *Repo) MarkTimedOut(ctx context.Context, id string, reason string) error {
	return r.transition(ctx, id, StateTimedOut, reason, func(tx *sql.Tx, t *Task, now time.Time) error {
		_, err := tx.ExecContext(ctx, r.dialect.UpdateTerminalAtSQL(), now, now, id)
		return err
	})
}

// MarkCancelled transitions to Cancelled.
func (r *Repo) MarkCancelled(ctx context.Context, id string, reason string) error {
	return r.transition(ctx, id, StateCancelled, reason, func(tx *sql.Tx, t *Task, now time.Time) error {
		_, err := tx.ExecContext(ctx, r.dialect.UpdateTerminalAtSQL(), now, now, id)
		return err
	})
}

// ClaimNextTask atomically picks the next ready task for providerKey
// and bumps poll_attempt. Returns (nil, nil) when no task is ready.
//
// On Postgres uses SELECT ... FOR UPDATE SKIP LOCKED (the canonical
// queue pattern). On sqlite degrades to a non-blocking single-row
// update — single-writer semantics make it safe.
func (r *Repo) ClaimNextTask(ctx context.Context, providerKey adapter.ProviderKey) (*Task, error) {
	q := r.dialect.ClaimNextTaskSQL()
	row := r.db.QueryRowContext(ctx, q, string(providerKey), time.Now().UTC())
	t, err := scanTask(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("task: claim: %w", err)
	}
	return t, nil
}

// FindStuck returns up to limit tasks whose next_poll_after is past
// olderThan ago and which are still in non-terminal state.
func (r *Repo) FindStuck(ctx context.Context, olderThan time.Duration, limit int) ([]*Task, error) {
	q := r.dialect.FindStuckSQL()
	cutoff := time.Now().UTC().Add(-olderThan)
	rows, err := r.db.QueryContext(ctx, q, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Task, 0)
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// FindTimedOut returns up to limit tasks whose sla_deadline has passed
// while still in non-terminal state.
func (r *Repo) FindTimedOut(ctx context.Context, limit int) ([]*Task, error) {
	q := r.dialect.FindTimedOutSQL()
	rows, err := r.db.QueryContext(ctx, q, time.Now().UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]*Task, 0)
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListEvents returns all task_event rows for the given task ID, ordered
// by created_at ASC. Used by tests + admin tooling.
func (r *Repo) ListEvents(ctx context.Context, taskID string) ([]TaskEvent, error) {
	q := r.dialect.ListEventsSQL()
	rows, err := r.db.QueryContext(ctx, q, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]TaskEvent, 0)
	for rows.Next() {
		var ev TaskEvent
		var meta []byte
		var reason sql.NullString
		if err := rows.Scan(&ev.ID, &ev.TaskID, &ev.FromState, &ev.ToState,
			&reason, &meta, &ev.CreatedAt); err != nil {
			return nil, err
		}
		if reason.Valid {
			ev.Reason = reason.String
		}
		if len(meta) > 0 {
			ev.Metadata = meta
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// transition runs the assert + update + audit-event in a single tx,
// retrying on transient lock contention (SQLite shared-cache deadlocks
// on concurrent writers; Postgres serialization failures under high
// contention). AssertTransition + IsTerminal failures and other
// programmer-error returns DO NOT retry.
//
// The tx serializes a state read with the state write, so concurrent
// transitions on the same task are linearizable. AssertTransition is
// called against the current DB state, not the caller's snapshot — this
// is what makes webhook + worker race correctness work.
//
// `mutate` receives the same `now` used for the state-update statement
// so all timestamps inside the transition share a single value (clean
// ordering for the audit log).
func (r *Repo) transition(ctx context.Context, id string, to TaskState, reason string, mutate func(tx *sql.Tx, t *Task, now time.Time) error) error {
	const maxRetries = 5
	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			// Exponential backoff with jitter: 5ms, 12ms, 28ms, 65ms.
			delay := time.Duration(5<<uint(attempt-1)) * time.Millisecond
			jitter := time.Duration(mathrand.Int63n(int64(delay) / 2))
			select {
			case <-time.After(delay + jitter):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		err := r.transitionOnce(ctx, id, to, reason, mutate)
		if err == nil {
			return nil
		}
		// Non-retryable conditions return immediately.
		if errors.Is(err, ErrTaskNotFound) || errors.Is(err, ErrTerminalState) {
			return err
		}
		var illegal *ErrIllegalTransition
		if errors.As(err, &illegal) {
			return err
		}
		// SQLite shared-cache and Postgres serialization conflicts are
		// retryable. We match by error message substring because the
		// driver-specific wrapping types are not stably exposed.
		if isRetryableLockError(err) {
			lastErr = err
			continue
		}
		return err
	}
	return fmt.Errorf("task: transition retries exhausted (%d attempts): %w", maxRetries, lastErr)
}

// transitionOnce is one attempt of transition. Pulled out so the caller
// can retry the entire tx (BeginTx → … → Commit) cleanly on lock errors.
func (r *Repo) transitionOnce(ctx context.Context, id string, to TaskState, reason string, mutate func(tx *sql.Tx, t *Task, now time.Time) error) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("task: begin tx: %w", err)
	}
	defer tx.Rollback()
	row := tx.QueryRowContext(ctx, r.dialect.LockTaskByIDSQL(), id)
	t, err := scanTask(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrTaskNotFound
		}
		return fmt.Errorf("task: select for transition: %w", err)
	}
	if t.State.IsTerminal() {
		return ErrTerminalState
	}
	if err := AssertTransition(t.State, to); err != nil {
		return err
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, r.dialect.UpdateStateSQL(), string(to), now, id); err != nil {
		return fmt.Errorf("task: update state: %w", err)
	}
	if mutate != nil {
		if err := mutate(tx, t, now); err != nil {
			return fmt.Errorf("task: mutate during transition: %w", err)
		}
	}
	if err := r.appendEvent(ctx, tx, id, string(t.State), string(to), reason, nil); err != nil {
		return fmt.Errorf("task: append event: %w", err)
	}
	return tx.Commit()
}

// isRetryableLockError reports whether err is the kind of transient lock
// contention that should trigger a transaction retry. Covers:
//   - SQLite SQLITE_BUSY / SQLITE_LOCKED / shared-cache deadlock
//   - Postgres SQLSTATE 40001 (serialization_failure) and 40P01 (deadlock_detected)
//
// We match on substring because the underlying typed errors live in
// driver-specific packages that this layer deliberately doesn't import.
func isRetryableLockError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return containsAny(msg,
		"database is locked",
		"database table is locked",
		"deadlocked",
		"40001",
		"40P01",
		"could not serialize access",
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

// appendEvent inserts a row in task_event. When tx is nil, runs against
// the bare DB (used by Create where there is no enclosing tx).
func (r *Repo) appendEvent(ctx context.Context, tx *sql.Tx, taskID, from, to, reason string, metadata []byte) error {
	q := r.dialect.InsertTaskEventSQL()
	args := []any{taskID, from, to, nullableString(reason), nullableBytes(metadata), time.Now().UTC()}
	if tx != nil {
		_, err := tx.ExecContext(ctx, q, args...)
		return err
	}
	_, err := r.db.ExecContext(ctx, q, args...)
	return err
}

// TaskEvent mirrors a row in task_event.
type TaskEvent struct {
	ID        int64
	TaskID    string
	FromState string
	ToState   string
	Reason    string
	Metadata  json.RawMessage
	CreatedAt time.Time
}

// ─────────────────────────────────────────────────────────────────────────
// Sentinel errors
// ─────────────────────────────────────────────────────────────────────────

// ErrIdempotentReplay is returned by Create when an existing task with
// the same idempotency_key already exists. Callers MUST inspect this
// error and short-circuit (see dedup.go).
var ErrIdempotentReplay = errors.New("task: idempotent replay (existing task returned)")

// ErrTaskNotFound is returned by transition methods when the task ID is
// missing.
var ErrTaskNotFound = errors.New("task: not found")

// ─────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────

func validateCreateParams(p NewTaskParams) error {
	if p.AccountID == "" {
		return errors.New("task: AccountID is required")
	}
	if p.ModelKey == "" {
		return errors.New("task: ModelKey is required")
	}
	if p.ProviderKey == "" {
		return errors.New("task: ProviderKey is required")
	}
	if len(p.ParamsJSON) == 0 {
		return errors.New("task: ParamsJSON is required")
	}
	if p.SLATimeout <= 0 {
		return errors.New("task: SLATimeout must be positive")
	}
	return nil
}

func mintTaskID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return "gen_" + hex.EncodeToString(buf[:]), nil
}

// mintWebhookToken returns a 32-byte (256-bit) hex-encoded random token
// per AP-18.
func mintWebhookToken() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

func capRaw(raw []byte) []byte {
	if len(raw) > adapter.MaxRawErrorBytes {
		return raw[:adapter.MaxRawErrorBytes]
	}
	return raw
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// rowScanner is the common surface of *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanTask reads a fully-populated task row.
func scanTask(s rowScanner) (*Task, error) {
	var t Task
	var (
		modelKey, providerKey, state string
		paramsJSON                   []byte
		idemKey, ref, pollingURL     sql.NullString
		nextPoll                     sql.NullTime
		lastErrClass                 sql.NullString
		raw                          []byte
		submittedAt, terminalAt      sql.NullTime
	)
	if err := s.Scan(
		&t.ID, &t.AccountID, &modelKey, &providerKey, &state,
		&paramsJSON, &idemKey, &ref, &pollingURL,
		&t.PollAttempt, &nextPoll, &t.WebhookToken, &t.SLADeadline,
		&lastErrClass, &raw,
		(*int64)(&t.HeldAmount), (*int64)(&t.ActualCost),
		&submittedAt, &terminalAt,
		&t.CreatedAt, &t.UpdatedAt,
	); err != nil {
		return nil, err
	}
	t.ModelKey = adapter.ModelKey(modelKey)
	t.ProviderKey = adapter.ProviderKey(providerKey)
	t.State = TaskState(state)
	t.ParamsJSON = paramsJSON
	if idemKey.Valid {
		t.IdempotencyKey = idemKey.String
	}
	if ref.Valid {
		t.UpstreamRef = ref.String
	}
	if pollingURL.Valid {
		t.PollingURL = pollingURL.String
	}
	if nextPoll.Valid {
		v := nextPoll.Time
		t.NextPollAfter = &v
	}
	if lastErrClass.Valid {
		t.LastErrorClass = lastErrClass.String
	}
	if len(raw) > 0 {
		t.RawError = raw
	}
	if submittedAt.Valid {
		v := submittedAt.Time
		t.SubmittedAt = &v
	}
	if terminalAt.Valid {
		v := terminalAt.Time
		t.TerminalAt = &v
	}
	return &t, nil
}
