// PendingCostStore — bridge between TaskSucceeded (carries ActualCost) and
// AssetHosted (does not). Settle on AssetHosted needs the cost from the
// earlier TaskSucceeded; this store holds it for the seconds-to-minutes
// gap between the two events.
//
// Two implementations:
//
//   - InMemoryPendingStore: process-local map with TTL. Used in dev and
//     tests; preserves the original single-instance semantics that
//     subscriber.go shipped with.
//
//   - RedisPendingStore: shared store keyed by `wallet:pending:{taskID}`
//     with caller-supplied TTL. Required for multi-instance deployments
//     where TaskSucceeded and AssetHosted may be handled on different
//     replicas.
//
// Idempotency contract (both impls):
//
//   - Set with same taskID overwrites the prior value (no error).
//   - Get on a missing/expired key returns (0, false, nil) — NOT an error.
//   - Get after Delete returns (0, false, nil).
//   - Delete on a missing key is a no-op (no error).
//
// Concurrency:
//
//   - All methods are safe for concurrent use.

package wallet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/QuantumNous/new-api/internal/adapter"
)

// DefaultPendingTTL is the recommended TTL for entries written between
// TaskSucceeded and AssetHosted. Long enough to cover slow asset hosters,
// short enough that orphaned entries (events dropped before AssetHosted)
// do not accumulate forever.
const DefaultPendingTTL = 24 * time.Hour

// pendingRedisKeyPrefix is the canonical Redis key prefix for pending-cost
// entries. Exported via PendingRedisKey() for tests; not part of the public
// wallet API surface.
const pendingRedisKeyPrefix = "wallet:pending:"

// PendingRedisKey returns the canonical Redis key for a taskID.
func PendingRedisKey(taskID string) string {
	return pendingRedisKeyPrefix + taskID
}

// PendingCostStore stages the ActualCost from a TaskSucceeded event so a
// later AssetHosted event (which carries no cost) can drive Settle with
// the right amount.
//
// The interface is deliberately small (3 methods) — see
// rules/golang/coding-style.md "Keep interfaces small".
type PendingCostStore interface {
	// Set stores cost for taskID with the given TTL. Re-Set with the same
	// taskID overwrites the prior entry. ttl<=0 is rejected.
	Set(ctx context.Context, taskID string, cost adapter.CostUSD, ttl time.Duration) error

	// Get returns the stored cost. (cost, true, nil) on hit;
	// (0, false, nil) on miss or expiry. Errors are reserved for transport
	// or decoding failures.
	Get(ctx context.Context, taskID string) (adapter.CostUSD, bool, error)

	// Delete removes the entry. Missing keys are NOT an error.
	Delete(ctx context.Context, taskID string) error
}

// ─────────────────────────────────────────────────────────────────────────
// InMemoryPendingStore
// ─────────────────────────────────────────────────────────────────────────

// inMemoryEntry is the value stored in the map. expiresAt zero means
// "never expires" (callers should always pass a TTL, but defensive).
type inMemoryEntry struct {
	cost      adapter.CostUSD
	expiresAt time.Time
}

// InMemoryPendingStore is a process-local PendingCostStore. Goroutine-safe
// via sync.RWMutex. Expiry is checked lazily on Get; entries are evicted
// when observed expired. There is no background sweeper — the map grows
// only as fast as in-flight tasks and shrinks via Delete on each Settle.
type InMemoryPendingStore struct {
	mu    sync.RWMutex
	items map[string]inMemoryEntry
	now   func() time.Time // injectable for tests
}

// NewInMemoryPendingStore returns a ready-to-use in-memory store.
func NewInMemoryPendingStore() *InMemoryPendingStore {
	return &InMemoryPendingStore{
		items: make(map[string]inMemoryEntry),
		now:   func() time.Time { return time.Now() },
	}
}

// SetClock overrides the time source for tests. Not goroutine-safe with
// concurrent Set/Get; call before traffic.
func (s *InMemoryPendingStore) SetClock(now func() time.Time) {
	if now != nil {
		s.now = now
	}
}

// Set stores cost for taskID with TTL.
func (s *InMemoryPendingStore) Set(_ context.Context, taskID string, cost adapter.CostUSD, ttl time.Duration) error {
	if taskID == "" {
		return errors.New("wallet: pending Set requires non-empty taskID")
	}
	if ttl <= 0 {
		return errors.New("wallet: pending Set requires positive ttl")
	}
	s.mu.Lock()
	s.items[taskID] = inMemoryEntry{
		cost:      cost,
		expiresAt: s.now().Add(ttl),
	}
	s.mu.Unlock()
	return nil
}

// Get reads and lazily evicts expired entries.
func (s *InMemoryPendingStore) Get(_ context.Context, taskID string) (adapter.CostUSD, bool, error) {
	if taskID == "" {
		return 0, false, nil
	}
	// Fast path: read lock.
	s.mu.RLock()
	e, ok := s.items[taskID]
	s.mu.RUnlock()
	if !ok {
		return 0, false, nil
	}
	if !e.expiresAt.IsZero() && !s.now().Before(e.expiresAt) {
		// Expired — evict under write lock and report miss. Use a
		// double-check to avoid evicting an entry refreshed between the
		// RUnlock and the Lock.
		s.mu.Lock()
		cur, stillThere := s.items[taskID]
		if stillThere && !cur.expiresAt.IsZero() && !s.now().Before(cur.expiresAt) {
			delete(s.items, taskID)
		}
		s.mu.Unlock()
		return 0, false, nil
	}
	return e.cost, true, nil
}

// Delete removes the entry. Missing key is silent.
func (s *InMemoryPendingStore) Delete(_ context.Context, taskID string) error {
	if taskID == "" {
		return nil
	}
	s.mu.Lock()
	delete(s.items, taskID)
	s.mu.Unlock()
	return nil
}

// Len returns the current number of entries (including expired but
// not-yet-evicted). Test helper; not part of the PendingCostStore interface.
func (s *InMemoryPendingStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.items)
}

// ─────────────────────────────────────────────────────────────────────────
// RedisPendingStore
// ─────────────────────────────────────────────────────────────────────────

// redisPayload is the JSON envelope written to Redis. taskID is duplicated
// into the value (as well as the key) so log/debug dumps of the value
// remain self-describing. Timestamp is unix seconds.
type redisPayload struct {
	Cost      adapter.CostUSD `json:"cost"`
	TaskID    string          `json:"task_id"`
	Timestamp int64           `json:"ts"`
}

// RedisClient is the subset of *redis.Client the store uses. Defining the
// interface here keeps the public RedisPendingStore tied to a real client
// via redis.UniversalClient while still allowing test injection without
// running a real Redis server.
type RedisClient interface {
	Set(ctx context.Context, key string, value any, ttl time.Duration) *redis.StatusCmd
	Get(ctx context.Context, key string) *redis.StringCmd
	Del(ctx context.Context, keys ...string) *redis.IntCmd
}

// RedisPendingStore is a Redis-backed PendingCostStore. Use this when the
// backend runs as more than one replica.
type RedisPendingStore struct {
	client RedisClient
	now    func() time.Time
}

// NewRedisPendingStore wraps a redis client. The client lifecycle (dialing,
// closing) is owned by the caller — wallet does not Close() it.
func NewRedisPendingStore(client RedisClient) *RedisPendingStore {
	if client == nil {
		panic("wallet: NewRedisPendingStore requires non-nil client")
	}
	return &RedisPendingStore{
		client: client,
		now:    func() time.Time { return time.Now() },
	}
}

// SetClock overrides the time source for tests.
func (s *RedisPendingStore) SetClock(now func() time.Time) {
	if now != nil {
		s.now = now
	}
}

// Set marshals the payload and writes with TTL.
func (s *RedisPendingStore) Set(ctx context.Context, taskID string, cost adapter.CostUSD, ttl time.Duration) error {
	if taskID == "" {
		return errors.New("wallet: pending Set requires non-empty taskID")
	}
	if ttl <= 0 {
		return errors.New("wallet: pending Set requires positive ttl")
	}
	payload := redisPayload{
		Cost:      cost,
		TaskID:    taskID,
		Timestamp: s.now().Unix(),
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("wallet: marshal pending payload: %w", err)
	}
	if err := s.client.Set(ctx, PendingRedisKey(taskID), b, ttl).Err(); err != nil {
		return fmt.Errorf("wallet: redis SET pending: %w", err)
	}
	return nil
}

// Get reads and unmarshals. Missing key → (0, false, nil).
func (s *RedisPendingStore) Get(ctx context.Context, taskID string) (adapter.CostUSD, bool, error) {
	if taskID == "" {
		return 0, false, nil
	}
	raw, err := s.client.Get(ctx, PendingRedisKey(taskID)).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("wallet: redis GET pending: %w", err)
	}
	var payload redisPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return 0, false, fmt.Errorf("wallet: decode pending payload: %w", err)
	}
	return payload.Cost, true, nil
}

// Delete removes the key. Missing key is not an error (DEL returns 0).
func (s *RedisPendingStore) Delete(ctx context.Context, taskID string) error {
	if taskID == "" {
		return nil
	}
	if err := s.client.Del(ctx, PendingRedisKey(taskID)).Err(); err != nil {
		return fmt.Errorf("wallet: redis DEL pending: %w", err)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────
// Construction from environment
// ─────────────────────────────────────────────────────────────────────────

// PendingStoreEnvVar is the env var that selects the implementation.
// Empty / unset → in-memory. "redis" → Redis-backed (requires REDIS_URL).
const PendingStoreEnvVar = "WALLET_PENDING_STORE"

// PendingStoreRedisURLEnv is the connection string for Redis when
// WALLET_PENDING_STORE=redis. Format: standard go-redis URL —
// `redis://[:password@]host:port[/db]` or `rediss://...` for TLS.
const PendingStoreRedisURLEnv = "REDIS_URL"

// NewPendingStoreFromEnv returns a PendingCostStore selected by env var.
//
// If WALLET_PENDING_STORE is unset or empty, returns an in-memory store
// and DOES NOT touch Redis at all. This keeps single-instance dev/CI from
// requiring Redis.
//
// If WALLET_PENDING_STORE=redis, parses REDIS_URL and dials lazily. Errors
// from URL parsing are returned; the connection itself is not pinged here
// (go-redis dials lazily on first command).
//
// `getenv` is injected so callers can pass os.Getenv in production and
// fakes in tests; the function does not import "os" itself.
func NewPendingStoreFromEnv(getenv func(string) string) (PendingCostStore, error) {
	switch getenv(PendingStoreEnvVar) {
	case "", "memory", "in-memory":
		return NewInMemoryPendingStore(), nil
	case "redis":
		url := getenv(PendingStoreRedisURLEnv)
		if url == "" {
			return nil, fmt.Errorf("wallet: %s=redis but %s is empty", PendingStoreEnvVar, PendingStoreRedisURLEnv)
		}
		opts, err := redis.ParseURL(url)
		if err != nil {
			return nil, fmt.Errorf("wallet: parse %s: %w", PendingStoreRedisURLEnv, err)
		}
		client := redis.NewClient(opts)
		return NewRedisPendingStore(client), nil
	default:
		return nil, fmt.Errorf("wallet: unknown %s=%q (want \"\" or \"redis\")",
			PendingStoreEnvVar, getenv(PendingStoreEnvVar))
	}
}
