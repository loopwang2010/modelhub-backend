// cmd/migrate — pressly/goose wrapper per ADR-008.
//
// One binary, one job: run SQL migrations from ./migrations against the
// database identified by the DATABASE_URL env var. Production deploys ship
// this binary alongside the main API server and run `migrate up` before
// the API starts.
//
// Subcommands (delegated to goose verbatim):
//
//	migrate up                       — apply all pending migrations
//	migrate up-by-one                — apply the next pending migration
//	migrate up-to VERSION            — apply migrations up to VERSION
//	migrate down                     — roll back the latest applied migration
//	migrate down-to VERSION          — roll back to VERSION
//	migrate redo                     — down + up of the latest
//	migrate reset                    — roll all migrations back
//	migrate status                   — print applied/pending list
//	migrate version                  — print current DB version
//	migrate create NAME [sql|go]     — scaffold a new migration in ./migrations
//	migrate fix                      — renumber migrations sequentially
//	migrate validate                 — sanity-check migration files
//
// Driver: defaults to postgres (matches our prod stack). Override with
// MIGRATE_DRIVER=mysql or sqlite3 for tests.
//
// AP-guard: this binary MUST NOT silently apply migrations against an
// unknown DB. Missing DATABASE_URL is a fatal error.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/pressly/goose/v3"

	// Database drivers — registered via blank import so goose can dial.
	_ "github.com/jackc/pgx/v5/stdlib"
)

const (
	defaultMigrationsDir = "migrations"
	defaultDriver        = "pgx" // pgx is faster than lib/pq and already a dep
	envDatabaseURL       = "DATABASE_URL"
	envMigrateDriver     = "MIGRATE_DRIVER"
	envMigrationsDir     = "MIGRATIONS_DIR"
)

func main() {
	dir := flag.String("dir", "", "migrations directory (overrides MIGRATIONS_DIR)")
	driver := flag.String("driver", "", "database driver (overrides MIGRATE_DRIVER, default pgx)")
	dsn := flag.String("dsn", "", "database DSN (overrides DATABASE_URL)")
	flag.Usage = printUsage
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		printUsage()
		os.Exit(2)
	}

	cmd := args[0]
	cmdArgs := args[1:]

	// `create` does not need a DB connection — goose handles it from disk.
	if cmd == "create" {
		runCreate(resolveDir(*dir), cmdArgs)
		return
	}

	resolvedDriver := resolveString(*driver, envMigrateDriver, defaultDriver)
	resolvedDSN := resolveString(*dsn, envDatabaseURL, "")
	if resolvedDSN == "" {
		fatalf("no DSN provided: set DATABASE_URL or pass -dsn")
	}

	db, err := sql.Open(resolvedDriver, resolvedDSN)
	if err != nil {
		fatalf("open database: %v", err)
	}
	defer db.Close()

	pingCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		fatalf("ping database: %v", err)
	}

	if err := goose.SetDialect(dialectFor(resolvedDriver)); err != nil {
		fatalf("set dialect: %v", err)
	}

	resolvedDir := resolveDir(*dir)
	if err := goose.RunContext(context.Background(), cmd, db, resolvedDir, cmdArgs...); err != nil {
		fatalf("goose %s: %v", cmd, err)
	}
}

// resolveString picks the first non-empty source: explicit flag, env var,
// fallback default.
func resolveString(flagValue, envName, fallback string) string {
	if flagValue != "" {
		return flagValue
	}
	if v := os.Getenv(envName); v != "" {
		return v
	}
	return fallback
}

// resolveDir returns the migrations dir from flag, env, or default. Always
// converted to an absolute path so error messages reference a real location.
func resolveDir(flagValue string) string {
	dir := resolveString(flagValue, envMigrationsDir, defaultMigrationsDir)
	abs, err := filepath.Abs(dir)
	if err != nil {
		// Fall back to the literal value; goose will surface the error.
		return dir
	}
	return abs
}

// dialectFor maps the driver string to a goose dialect string. goose calls
// these "dialects" not "drivers" and not all driver names match.
func dialectFor(driver string) string {
	switch driver {
	case "pgx", "postgres":
		return "postgres"
	case "mysql":
		return "mysql"
	case "sqlite", "sqlite3":
		return "sqlite3"
	default:
		// Default to postgres for production safety; tests must opt in
		// explicitly via -driver.
		return "postgres"
	}
}

// runCreate scaffolds a new migration; goose handles filename numbering
// and template emission.
func runCreate(dir string, args []string) {
	if len(args) == 0 {
		fatalf("create: usage: migrate create NAME [sql|go]")
	}
	name := args[0]
	migrationType := "sql"
	if len(args) > 1 {
		migrationType = args[1]
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatalf("mkdir %q: %v", dir, err)
	}
	if err := goose.Create(nil, dir, name, migrationType); err != nil {
		if errors.Is(err, os.ErrExist) {
			fatalf("create: migration with name %q already exists", name)
		}
		fatalf("create: %v", err)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "migrate: "+format+"\n", args...)
	os.Exit(1)
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "usage: migrate [-dir DIR] [-driver D] [-dsn DSN] COMMAND [ARGS...]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Commands: up, up-by-one, up-to VERSION, down, down-to VERSION,")
	fmt.Fprintln(os.Stderr, "          redo, reset, status, version, create NAME [sql|go],")
	fmt.Fprintln(os.Stderr, "          fix, validate")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Required env: DATABASE_URL  (e.g. postgres://user:pw@host:5432/db?sslmode=disable)")
	fmt.Fprintln(os.Stderr, "Optional env: MIGRATE_DRIVER (pgx|postgres|mysql|sqlite3, default pgx)")
	fmt.Fprintln(os.Stderr, "              MIGRATIONS_DIR (default ./migrations)")
}
