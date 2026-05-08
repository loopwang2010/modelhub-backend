# T6 — F5: Redis-backed pendingCost store

Sprint 2 / F5 follow-up to `plans/CODE-REVIEW.md`. Promotes the
single-instance `map[taskID]CostUSD` in `subscriber.go` to a pluggable
`PendingCostStore` with two implementations (in-memory + Redis), keeping
single-instance dev workflows untouched while unblocking horizontal
scaling per ADR-005.

Branch: `feat/sprint2-f5-redis-pending`
Base: `dd7a119f`
Commit: see "Commit" section below.

## Files changed

| File | Status | Purpose |
|------|--------|---------|
| `internal/wallet/pendingstore.go` | new | `PendingCostStore` interface + `InMemoryPendingStore` + `RedisPendingStore` + env selector |
| `internal/wallet/pendingstore_test.go` | new | Contract tests parameterized over both impls (via miniredis); subscriber-level error-injection tests |
| `internal/wallet/subscriber.go` | refactor | Replaces `costMu` + `pendingCost map` with injected `PendingCostStore`. Backwards-compatible `NewSubscriber(...)` shim defaults to in-memory |
| `controller/modelhub_auth.go` | additive | New `SetWalletPendingStore` / `GetWalletPendingStore` singleton accessors (companion to `SetWallet`) |
| `main.go` | additive | New `initModelhubPendingStore()` reads `WALLET_PENDING_STORE` env and stages the chosen store on the controller singleton |
| `go.mod` / `go.sum` | bumped | `github.com/redis/go-redis/v9 v9.17.3` (prod) + `github.com/alicebob/miniredis/v2 v2.37.0` (test) |

No changes to `internal/wallet/dialect.go`, `tx.go`, `wallet.go`,
`internal/task/`, or any wallet public API surface (per the brief's
HARD constraints).

## Wiring contract

```
initModelhubWallet()                                      // main.go
  └── controller.SetWallet(w, db, dialect)
  └── initModelhubPendingStore()                          // main.go
        └── wallet.NewPendingStoreFromEnv(os.Getenv)      // pendingstore.go
              ├── ""           / "memory" / "in-memory" → *InMemoryPendingStore
              ├── "redis"                               → *RedisPendingStore (REDIS_URL required)
              └── otherwise                             → error (logged, subscriber stays unstarted)
        └── controller.SetWalletPendingStore(store)
```

The actual `wallet.NewSubscriber*` call is **not yet** wired in main.go —
the EventBus startup site lives in a Sprint-3 ticket. The pending store is
constructed and parked on the controller singleton so the subscriber
wiring can pick it up via `controller.GetWalletPendingStore()` without
re-reading env (single source of truth).

When tests construct a subscriber directly via `wallet.NewSubscriber(w,
bus, fractionFn)` the in-memory store is created automatically — the
existing 12-test subscriber suite in `coverage_test.go` is unchanged.

### Subscriber error-handling contract

The subscriber treats `PendingCostStore` failures as transient and
fail-safe:

- `Set` failure on `TaskSucceeded` → log; `AssetHosted` will fall through
  to the F9 "no prior TaskSucceeded → Refund" path.
- `Get` failure on `AssetHosted` → log + treat as miss + Refund. NEVER
  Settle on a guessed amount (preserves ADR-005 integrity boundary).
- `Delete` failure → log only; entry survives until TTL.

## Env vars

| Variable | Values | Effect |
|----------|--------|--------|
| `WALLET_PENDING_STORE` | unset / `""` / `memory` / `in-memory` | In-memory store (default; no Redis dependency at runtime) |
| `WALLET_PENDING_STORE` | `redis` | Redis store (requires `REDIS_URL`) |
| `WALLET_PENDING_STORE` | anything else | Init returns error → subscriber not started → ops alert via SysLog |
| `REDIS_URL` | go-redis URL: `redis://[user:pass@]host:port[/db]` or `rediss://...` | Required only when `WALLET_PENDING_STORE=redis` |

Default TTL for staged costs: `wallet.DefaultPendingTTL` (24h). Override
with `(*Subscriber).SetPendingTTL(d)` before `Start`. Single TTL chosen
because the worker SLA caps the AssetHosted gap at minutes; 24h is
generous slack for retry storms and safe against unbounded growth.

Redis key shape: `wallet:pending:{taskID}` (exposed via
`wallet.PendingRedisKey` for ops debugging). Value: JSON envelope
`{"cost":<int64 micro-USD>, "task_id":"...", "ts":<unix>}`.

## Test coverage — wallet package

Run: `go test ./internal/wallet/... -count=1 -short -cover`

| Run | Coverage |
|-----|----------|
| Pre-change baseline (commit `dd7a119f`) | **78.1 %** |
| Post-change | **80.9 %** |

Per-file delta on the surfaces touched:

| Symbol | Coverage |
|--------|----------|
| `pendingstore.go::PendingRedisKey/InMem/RedisPendingStore` | 80.0 – 100 % per func |
| `pendingstore.go::NewPendingStoreFromEnv` | 100 % |
| `subscriber.go::handle` | 88.9 % → 100 % |
| `subscriber.go::onAssetHosted` | 65.0 % → 75.0 % (new store-error branch covered) |
| `subscriber.go::NewSubscriber*` | 100 % / 90.9 % |
| `subscriber.go::SetPendingTTL` | 100 % |

Race detection: regular tests pass (`-count=1 -short` clean). `-race`
requires CGO/gcc which is not available in the local Windows toolchain;
the contract test (`TestPendingCostStore_ConcurrentAccess_NoRace`)
exercises 32 goroutines × 100 ops over Set/Get/Delete and runs clean
without race-detector instrumentation. Recommend running `go test -race`
in CI (Linux runners typically have gcc).

## What's NOT in this PR

- No subscriber lifecycle wiring in main.go (`Subscriber.Start(ctx)` is
  still test-only) — out of scope; that's the Sprint-3 EventBus startup
  ticket. The store singleton is already plumbed so that ticket only has
  to read it.
- No DB-backed PendingStore (brief explicitly excluded).
- No metrics/traces (brief explicitly excluded).
- No public wallet API changes — the `PendingCostStore` interface is
  internal injection plumbing; existing callers compile untouched.

## Verification

```
go build ./internal/wallet/... ./controller/...                # clean
go vet  ./internal/... ./controller/...                        # clean
go test ./internal/wallet/...   -count=1 -short -cover         # 80.9%, 0 failures
go test ./controller/...        -count=1 -short                # 0 failures
go test ./internal/task/...     -count=1 -short                # 0 failures
```

## Commit

`feat(wallet): F5 distributed pendingCost store with Redis backend`
SHA: `7291d1b1b2f3851525084463d08ab6a33c915ece`
Branch: `feat/sprint2-f5-redis-pending`
Base: `dd7a119f`
