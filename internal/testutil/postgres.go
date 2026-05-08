// Package testutil provides a Postgres testcontainer harness so tests can
// exercise production-only SQL paths (Postgres dialects in
// internal/wallet/dialect.go and internal/task/dialect.go, plus the
// SERIALIZABLE retry path in internal/wallet/tx.go) against a real
// Postgres 16 server instead of the SQLite mock used elsewhere.
//
// Per F7 in plans/CODE-REVIEW.md, this harness is the foundation for
// follow-up work T4 (wallet) and T5 (task) which lift each package to
// ~95% coverage in subsequent PRs.
//
// ─── Design ──────────────────────────────────────────────────────────────
//
// One container per test session, one logical database per NewPostgresDB
// call:
//
//   - sync.Once spins up Postgres 16 the first time NewPostgresDB is called.
//     Subsequent calls reuse the running container, paying ~3-5s of
//     container startup once for the whole `go test` run.
//   - Each NewPostgresDB invocation creates a fresh database inside that
//     container (CREATE DATABASE harness_<random>) and applies the goose
//     migrations from ./migrations against it. This gives every test a
//     pristine schema with no cross-test bleed without paying for a new
//     container.
//   - t.Cleanup hooks drop the per-test database and close the *sql.DB.
//     The container itself is reaped at process exit by testcontainers'
//     Ryuk reaper sidecar.
//
// ─── Skip-on-no-Docker ───────────────────────────────────────────────────
//
// CI runners and dev machines without Docker shouldn't see test failures
// from a missing daemon. NewPostgresDB calls t.Skip with a clear message
// when the testcontainers SDK reports the docker daemon is unreachable.
//
// ─── Channels stub ───────────────────────────────────────────────────────
//
// migrations/0003_channel_modality.sql ALTERs the `channels` table, which
// in production is owned by GORM AutoMigrate (see docs/migrations.md
// "split policy" + 0003 header comment). To let goose apply 0003 against
// a fresh testcontainer, the harness pre-creates a minimal `channels`
// stub table before invoking goose. The stub is intentionally tiny — just
// enough columns to exist and accept the ALTER — because none of the
// SQL strings under test (wallet/task dialects) touch `channels`.

package testutil

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // pgx driver — registered for sql.Open("pgx", …)
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	postgresImage   = "postgres:16-alpine"
	postgresUser    = "harness"
	postgresPass    = "harness"
	postgresAdminDB = "harness_admin"

	containerStartupTimeout = 60 * time.Second
)

// gooseLock serialises goose calls. Goose v3 keeps package-global state
// (dialect, base FS) and isn't safe to drive from concurrent goroutines.
// Tests calling NewPostgresDB in parallel must take this lock for the
// duration of their migration run.
var gooseLock sync.Mutex

// gooseSetDialectOnce ensures we set the goose dialect exactly once per
// test process, which avoids races with any other code that may have
// initialised goose for a different driver.
var gooseSetDialectOnce sync.Once

// session holds the lazily-initialised testcontainer plus the connection
// info needed to mint per-test databases. Constructed exactly once via
// sessionOnce.
type session struct {
	container testcontainers.Container
	host      string
	port      string
	skipMsg   string // non-empty when Docker is unavailable
	startErr  error  // non-nil when startup failed for a reason other than missing Docker
}

var (
	sessionOnce sync.Once
	sharedSess  *session
)

// NewPostgresDB starts a Postgres 16 testcontainer (once per process),
// creates a fresh database inside it, applies all migrations from
// ./migrations/*.sql in version order, and returns a *sql.DB pointed at
// the new database.
//
// Cleanup is registered via t.Cleanup: when the test ends, the database
// is dropped and the *sql.DB is closed. The container is reaped by
// testcontainers' Ryuk sidecar at process exit.
//
// If Docker is unreachable, NewPostgresDB calls t.Skip with a clear
// message — CI runners and dev machines without Docker stay green.
func NewPostgresDB(t *testing.T) *sql.DB {
	t.Helper()

	sess := bootstrapSession(t)
	if sess.skipMsg != "" {
		t.Skipf("docker not available: %s", sess.skipMsg)
		return nil
	}
	if sess.startErr != nil {
		t.Fatalf("postgres testcontainer failed to start: %v", sess.startErr)
		return nil
	}

	dbName := uniqueDBName(t)

	// Step 1: connect to admin DB to issue CREATE DATABASE.
	adminDSN := dsnFor(sess, postgresAdminDB)
	adminDB, err := sql.Open("pgx", adminDSN)
	if err != nil {
		t.Fatalf("open admin db: %v", err)
	}
	defer adminDB.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := adminDB.ExecContext(ctx, fmt.Sprintf(`CREATE DATABASE %s`, quoteIdent(dbName))); err != nil {
		t.Fatalf("create database %q: %v", dbName, err)
	}

	// Step 2: connect to the new database.
	testDSN := dsnFor(sess, dbName)
	testDB, err := sql.Open("pgx", testDSN)
	if err != nil {
		dropDatabase(sess, dbName) // best effort cleanup
		t.Fatalf("open test db: %v", err)
	}

	if err := testDB.PingContext(ctx); err != nil {
		_ = testDB.Close()
		dropDatabase(sess, dbName)
		t.Fatalf("ping test db: %v", err)
	}

	// Step 3: pre-create channels stub (GORM-owned in prod) so 0003 can ALTER it.
	if _, err := testDB.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS channels (
			id BIGSERIAL PRIMARY KEY
		)
	`); err != nil {
		_ = testDB.Close()
		dropDatabase(sess, dbName)
		t.Fatalf("create channels stub: %v", err)
	}

	// Step 4: apply migrations.
	migrationsDir, err := findMigrationsDir()
	if err != nil {
		_ = testDB.Close()
		dropDatabase(sess, dbName)
		t.Fatalf("locate migrations dir: %v", err)
	}

	if err := applyMigrations(ctx, testDB, migrationsDir); err != nil {
		_ = testDB.Close()
		dropDatabase(sess, dbName)
		t.Fatalf("apply migrations: %v", err)
	}

	// Step 5: register cleanup.
	t.Cleanup(func() {
		_ = testDB.Close()
		dropDatabase(sess, dbName)
	})

	return testDB
}

// TruncateAllTables empties every user table in the public schema while
// preserving the schema itself (and the goose_db_version history). Useful
// when a single test wants to run several scenarios from a clean slate
// without paying the per-call CREATE DATABASE + migration cost.
func TruncateAllTables(t *testing.T, db *sql.DB) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rows, err := db.QueryContext(ctx, `
		SELECT table_name
		  FROM information_schema.tables
		 WHERE table_schema = 'public'
		   AND table_type = 'BASE TABLE'
		   AND table_name <> 'goose_db_version'
	`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan table name: %v", err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	if len(names) == 0 {
		return
	}

	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = quoteIdent(n)
	}
	stmt := fmt.Sprintf(`TRUNCATE TABLE %s RESTART IDENTITY CASCADE`, strings.Join(quoted, ", "))
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

// ─── internals ───────────────────────────────────────────────────────────

func bootstrapSession(t *testing.T) *session {
	t.Helper()
	sessionOnce.Do(func() {
		sharedSess = startSession()
	})
	return sharedSess
}

func startSession() (sess *session) {
	ctx, cancel := context.WithTimeout(context.Background(), containerStartupTimeout)
	defer cancel()

	// testcontainers-go's docker host detection (MustExtractDockerHost)
	// panics when no Docker socket / named pipe can be located instead of
	// returning an error. Treat any panic from the provider init as
	// "Docker not usable here" so dev/CI machines without a daemon hit
	// t.Skip rather than crashing the whole test binary.
	defer func() {
		if r := recover(); r != nil {
			sess = &session{skipMsg: fmt.Sprintf("%v", r)}
		}
	}()

	container, err := tcpostgres.Run(
		ctx,
		postgresImage,
		tcpostgres.WithDatabase(postgresAdminDB),
		tcpostgres.WithUsername(postgresUser),
		tcpostgres.WithPassword(postgresPass),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(containerStartupTimeout),
		),
	)
	if err != nil {
		if isDockerUnavailable(err) {
			return &session{skipMsg: err.Error()}
		}
		return &session{startErr: fmt.Errorf("start container: %w", err)}
	}

	host, err := container.Host(ctx)
	if err != nil {
		_ = container.Terminate(context.Background())
		return &session{startErr: fmt.Errorf("container host: %w", err)}
	}
	port, err := container.MappedPort(ctx, "5432/tcp")
	if err != nil {
		_ = container.Terminate(context.Background())
		return &session{startErr: fmt.Errorf("container mapped port: %w", err)}
	}

	return &session{
		container: container,
		host:      host,
		port:      port.Port(),
	}
}

// isDockerUnavailable returns true if the error indicates the Docker
// daemon is not reachable (vs a real container startup failure).
func isDockerUnavailable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	signals := []string{
		"cannot connect to the docker daemon",
		"docker daemon",
		"connection refused",
		"open //./pipe/docker",      // windows named pipe missing
		"dockerdesktoplinuxengine",  // windows desktop missing
		"no such file or directory", // typical for missing /var/run/docker.sock
		"is the docker daemon running",
		"failed to find rootless", // rootless docker not configured
		"failed to create",        // generic creation failure when daemon down
		"could not get docker info",
	}
	for _, s := range signals {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

func dsnFor(s *session, dbName string) string {
	return fmt.Sprintf(
		"postgres://%s:%s@%s:%s/%s?sslmode=disable",
		postgresUser, postgresPass, s.host, s.port, dbName,
	)
}

// uniqueDBName returns a Postgres-safe identifier derived from the test
// name plus a nanosecond suffix so parallel sub-tests don't collide.
func uniqueDBName(t *testing.T) string {
	t.Helper()
	// Postgres identifiers: <= 63 bytes, lowercase, alphanumeric + '_'.
	sanitized := make([]rune, 0, len(t.Name()))
	for _, r := range strings.ToLower(t.Name()) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			sanitized = append(sanitized, r)
		default:
			sanitized = append(sanitized, '_')
		}
	}
	suffix := fmt.Sprintf("_%d", time.Now().UnixNano())
	prefix := "harness_"
	maxName := 63 - len(prefix) - len(suffix)
	if len(sanitized) > maxName {
		sanitized = sanitized[:maxName]
	}
	return prefix + string(sanitized) + suffix
}

// quoteIdent quotes a Postgres identifier safely. We control the input
// (database/table names we generated ourselves), so this is belt-and-
// suspenders rather than a true defence against injection.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func dropDatabase(s *session, name string) {
	if s == nil || s.host == "" {
		return
	}
	adminDSN := dsnFor(s, postgresAdminDB)
	db, err := sql.Open("pgx", adminDSN)
	if err != nil {
		return
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Force-disconnect lingering sessions before drop. WITH (FORCE) is a
	// Postgres 13+ extension; the testcontainer is pinned to 16-alpine so
	// it's always available.
	_, _ = db.ExecContext(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, quoteIdent(name)))
}

// findMigrationsDir walks up from the current working directory looking
// for a sibling `migrations/` directory next to a `go.mod` file. This lets
// callers from any package (internal/testutil/postgres_test.go,
// internal/wallet/postgres_test.go, etc.) locate the canonical migrations
// folder without each test having to hard-code a relative path.
func findMigrationsDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getwd: %w", err)
	}
	dir := wd
	for {
		goMod := filepath.Join(dir, "go.mod")
		if _, err := os.Stat(goMod); err == nil {
			candidate := filepath.Join(dir, "migrations")
			if info, err := os.Stat(candidate); err == nil && info.IsDir() {
				return candidate, nil
			}
			return "", fmt.Errorf("found go.mod at %q but no migrations/ sibling", dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("walked to filesystem root without finding go.mod")
		}
		dir = parent
	}
}

// applyMigrations runs goose up against the supplied DB. Goose v3 has
// process-global state (dialect, base FS, logger), so we serialise calls
// through gooseLock to be safe under `go test -parallel N` workloads.
func applyMigrations(ctx context.Context, db *sql.DB, dir string) error {
	gooseLock.Lock()
	defer gooseLock.Unlock()

	var dialectErr error
	gooseSetDialectOnce.Do(func() {
		dialectErr = goose.SetDialect("postgres")
	})
	if dialectErr != nil {
		return fmt.Errorf("goose set dialect: %w", dialectErr)
	}
	// Re-set defensively in case some other test process has changed it.
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("goose set dialect: %w", err)
	}

	// Goose's default logger writes to stderr; quiet it for tests.
	goose.SetLogger(silentLogger{})

	if err := goose.UpContext(ctx, db, dir); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}
	return nil
}

// silentLogger satisfies goose.Logger and discards all output. Test
// failures still surface through the error path; we don't need migration
// chatter in `go test` output.
type silentLogger struct{}

func (silentLogger) Fatalf(format string, v ...interface{}) {}
func (silentLogger) Printf(format string, v ...interface{}) {}
