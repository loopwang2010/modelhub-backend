// Test scaffolding shared by every internal/task test.
//
// Uses modernc.org/sqlite (pure-Go, no CGO required) — a single
// in-memory DB per test, with the schema bootstrapped from
// SQLiteDialect.CreateSchemaSQL.
//
// Why not Postgres + testcontainers: the Windows worktree has no Docker
// install, and the worker logic is testable end-to-end on SQLite as
// long as the SQL strings differ only where they have to. The test
// asserts FSM correctness via the worker + repo + reconciler, NOT the
// SKIP LOCKED machinery (which a one-process SQLite test could not
// stress anyway). CI is the right place for the SKIP-LOCKED-on-
// Postgres soak.

package task

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/QuantumNous/new-api/internal/adapter"
	"github.com/QuantumNous/new-api/internal/events"
)

// dbCounter ensures every newTestDB returns a unique in-memory DSN.
var dbCounter atomic.Int64

// newTestDB opens a fresh in-memory SQLite DB and bootstraps the task
// + task_event schema. Returns a (*sql.DB, *Repo, cleanup) triplet —
// caller must defer cleanup() to close the DB.
func newTestDB(t *testing.T) (*sql.DB, *Repo, func()) {
	t.Helper()
	// shared cache + named DSN allows multiple connections in the pool
	// to see the same in-memory database — required because the worker
	// race tests use concurrent goroutines.
	dsn := fmt.Sprintf("file:test_db_%d?mode=memory&cache=shared&_busy_timeout=5000", dbCounter.Add(1))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	// SQLite serializes writers — keep the pool tight to avoid lock
	// thrash from concurrent tests.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	if err := db.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	dialect := SQLiteDialect{}
	for _, stmt := range dialect.CreateSchemaSQL() {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("create schema: %v\nsql: %s", err, stmt)
		}
	}
	repo := NewRepo(db, dialect)
	cleanup := func() { db.Close() }
	return db, repo, cleanup
}

// newTestTaskParams returns a NewTaskParams with sensible defaults,
// suitable for tests that don't care about specific values.
func newTestTaskParams(seed string) NewTaskParams {
	return NewTaskParams{
		AccountID:      "acct-" + seed,
		ModelKey:       "test-model",
		ProviderKey:    "mock-async",
		ParamsJSON:     json.RawMessage(`{"prompt":"hello"}`),
		IdempotencyKey: "key-" + seed,
		HeldAmount:     adapter.CostUSD(60_000),
		SLATimeout:     5 * time.Minute,
	}
}

// mustCreateAndSubmit creates a task and walks it Created → Held →
// Submitted with a synthetic upstream_ref. The returned task is fully
// reflective of DB state.
func mustCreateAndSubmit(t *testing.T, repo *Repo, p NewTaskParams) *Task {
	t.Helper()
	ctx := context.Background()
	task, err := repo.Create(ctx, p)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.MarkHeld(ctx, task.ID, p.HeldAmount); err != nil {
		t.Fatalf("mark held: %v", err)
	}
	ref := adapter.UpstreamRef("ref-" + task.ID)
	if err := repo.MarkSubmittedAsync(ctx, task.ID, ref, "", time.Now().UTC()); err != nil {
		t.Fatalf("mark submitted: %v", err)
	}
	out, err := repo.FindByID(ctx, task.ID)
	if err != nil || out == nil {
		t.Fatalf("re-find: %v / %v", err, out)
	}
	return out
}

// recordedEvents collects every event published to a bus, in order.
type recordedEvents struct {
	mu     sync.Mutex
	events []events.Event
}

func newRecordedEvents(bus events.EventBus) *recordedEvents {
	r := &recordedEvents{}
	bus.Subscribe(func(ev events.Event) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.events = append(r.events, ev)
	})
	return r
}

func (r *recordedEvents) Snapshot() []events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]events.Event, len(r.events))
	copy(out, r.events)
	return out
}

func (r *recordedEvents) ByKind(kind string) []events.Event {
	out := make([]events.Event, 0)
	for _, e := range r.Snapshot() {
		if e.Kind() == kind {
			out = append(out, e)
		}
	}
	return out
}
