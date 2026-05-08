# T3 Report — Sprint 2 / F7: Postgres testcontainer harness

## What was built

A new `internal/testutil` package exposing a `NewPostgresDB(t)` harness that
spins up a real Postgres 16 testcontainer once per `go test` process and hands
each caller a freshly-migrated, logically-isolated `*sql.DB`. This is the
foundation T4 (wallet) and T5 (task) will use to lift `internal/wallet` and
`internal/task` to ~95% coverage by exercising their Postgres-only SQL paths
and the SERIALIZABLE retry classifier in `wallet/tx.go`.

### Public API

```go
// internal/testutil/postgres.go
func NewPostgresDB(t *testing.T) *sql.DB
func TruncateAllTables(t *testing.T, db *sql.DB)
```

Cleanup is registered via `t.Cleanup`; callers don't manage teardown.

### Design choices

- **One container per test session**: a `sync.Once` boots Postgres
  16-alpine the first time `NewPostgresDB` is called. Subsequent calls
  reuse the container — paying the ~3-5s startup cost once for the
  whole `go test` run.
- **One database per call**: each `NewPostgresDB` mints a unique
  `harness_<sanitized-test-name>_<unix-nanos>` database inside the
  shared container, applies migrations against it, and drops it on
  `t.Cleanup`. No per-test transaction-rollback hackery; tests get a
  pristine schema and can run `go test -parallel N` safely.
- **Migrations via goose** (`pressly/goose/v3`, already a project dep).
  The harness walks up the directory tree to find `go.mod`, then loads
  every file in `./migrations/`. A `silentLogger` mutes goose's
  per-migration chatter so `go test` output stays readable.
- **Channels stub**: `migrations/0003_channel_modality.sql` ALTERs the
  `channels` table, which in production is owned by GORM AutoMigrate
  (see `docs/migrations.md` "split policy" + the 0003 header comment).
  The harness pre-creates a minimal `channels (id BIGSERIAL PRIMARY
  KEY)` stub before invoking goose so all 5 migrations apply cleanly.
  Wallet/task SQL never touches `channels`, so the stub stays minimal.
- **Skip-on-no-Docker**: `testcontainers-go`'s `MustExtractDockerHost`
  *panics* (instead of returning an error) when no Docker socket is
  reachable. `startSession` defers a `recover` and converts any such
  panic into a `t.Skipf("docker not available: %v", ...)`. CI runners
  and dev machines without a daemon stay green with a clear message.
- **Goose process-global state guard**: goose v3 keeps the dialect, base
  FS, and logger as package-globals. A `sync.Mutex` (`gooseLock`) wraps
  every call so concurrent harness users don't trip on each other.

## Files added / modified

| Action | Path | Notes |
| --- | --- | --- |
| Add | `internal/testutil/postgres.go` | The harness (~250 LoC, fully commented). |
| Add | `internal/testutil/postgres_test.go` | Smoke tests (4 cases). |
| Add | `T3-REPORT.md` | This file. |
| Edit | `go.mod` | Added direct deps; transitive set rebalanced by `go mod tidy`. |
| Edit | `go.sum` | Hash entries for the new dependency tree. |

No code outside `internal/testutil/`, `T3-REPORT.md`, `go.mod`, or
`go.sum` was modified.

## Dependency added

| Module | Version | Why |
| --- | --- | --- |
| `github.com/testcontainers/testcontainers-go` | `v0.41.0` | Container lifecycle. Pinned at v0.41.0 because v0.42.0 was unavailable from every Go proxy reachable from this host (goproxy.cn, goproxy.io, and `direct` all returned `unexpected EOF` / TLS errors during the dependency-add step). v0.41.0 is the latest version that resolved cleanly. |
| `github.com/testcontainers/testcontainers-go/modules/postgres` | `v0.41.0` | Postgres-specific helpers (`tcpostgres.Run`, `WithDatabase`, `WithUsername`, `WithPassword`). |

`go mod tidy` pulled in the standard testcontainers transitive set
(moby, otel exporters, sirupsen/logrus, etc.). All transitive additions
are first-party from the testcontainers / OpenTelemetry / Docker
ecosystems — no new lightly-maintained side projects.

## Test output

```
$ go test -v ./internal/testutil/...
=== RUN   TestNewPostgresDB_AppliesAllMigrations
    postgres_test.go:31: docker not available: rootless Docker is not supported on Windows
--- SKIP: TestNewPostgresDB_AppliesAllMigrations (0.00s)
=== RUN   TestNewPostgresDB_TablesExist
    postgres_test.go:74: docker not available: rootless Docker is not supported on Windows
--- SKIP: TestNewPostgresDB_TablesExist (0.00s)
=== RUN   TestNewPostgresDB_IsolatedPerTest
    postgres_test.go:99: docker not available: rootless Docker is not supported on Windows
--- SKIP: TestNewPostgresDB_IsolatedPerTest (0.00s)
=== RUN   TestTruncateAllTables
    postgres_test.go:128: docker not available: rootless Docker is not supported on Windows
--- SKIP: TestTruncateAllTables (0.00s)
PASS
ok  	github.com/QuantumNous/new-api/internal/testutil	9.075s
```

All four smoke tests skip cleanly with a clear, actionable message —
exactly the behaviour F7 requires for environments where Docker is
absent.

Sanity verifications run alongside:

- `go build ./internal/... ./cmd/...` → exit 0 (the pre-existing
  `web/dist` embed failure on `./...` is unrelated to F7).
- `go vet ./internal/... ./cmd/...` → exit 0.
- `go test ./internal/wallet/... ./internal/task/...` →
  `ok internal/wallet 22.391s` and `ok internal/task 59.098s` —
  no regressions in the packages T4/T5 will touch.

## Caveats / things to know before T4 / T5 land

1. **Dev box for this PR has no Docker daemon.** The harness logic
   itself was not exercised against a live Postgres in this branch;
   only the build path, the goose API surface, and the skip-on-
   no-Docker recovery are verified. T4/T5 should run their first test
   on a host with Docker before merging to confirm migrations apply
   end-to-end. The harness *should* work — every step is covered by
   well-trodden testcontainers-go + goose patterns — but a real run is
   the only way to be sure.
2. **testcontainers-go is at v0.41.0, not the v0.42.0 latest.** The
   newer version was not retrievable through any reachable Go proxy
   today; if v0.42.0 brings a fix that matters, a follow-up PR can bump
   it.
3. **Container reaping relies on Ryuk.** testcontainers spawns a
   sidecar (`testcontainers/ryuk`) that kills the Postgres container at
   process exit. If a test process is `kill -9`'d, the Postgres
   container can leak. Standard `go test` finishes register the
   cleanup correctly.
4. **`channels` stub is intentionally minimal.** It exists only so
   `0003_channel_modality.sql`'s `ADD COLUMN IF NOT EXISTS modality /
   task_kind` can succeed. T4/T5 must not assume any other channel
   columns are present in the harness DB; if a future migration starts
   relying on richer `channels` shape, the stub will need to grow.
5. **Postgres-only.** This harness is for the production dialect path.
   The existing SQLite suite (`coverage_test.go` etc.) still owns
   cross-dialect shape coverage; nothing here replaces it.
