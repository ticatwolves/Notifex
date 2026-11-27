// Package db opens Postgres connections and applies schema migrations.
package db

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // registers pgx5://
	"github.com/golang-migrate/migrate/v4/source/iofs"

	"github.com/ticatwolves/notifex/migrations"
)

// MigrationsTable is golang-migrate's bookkeeping table.
const MigrationsTable = "schema_migrations"

// MigrateOptions bound every migration statement. They are set on the
// migration connection, so generated and hand-written migrations alike get
// them without per-file SETs (squawk require-lock-timeout /
// require-statement-timeout are satisfied at the connection, ADR-010).
type MigrateOptions struct {
	// LockTimeout makes DDL give up instead of queueing behind a long
	// transaction and blocking the accept path behind it.
	LockTimeout time.Duration
	// StatementTimeout caps any single statement (backfills included).
	StatementTimeout time.Duration
}

// DefaultMigrateOptions are used by cmd/migrate.
var DefaultMigrateOptions = MigrateOptions{LockTimeout: 5 * time.Second, StatementTimeout: 15 * time.Minute}

// MigrateUp applies all pending migrations. It is safe to run concurrently
// (golang-migrate takes a Postgres advisory lock) and is a no-op when the
// schema is current. A dirty database (a migration failed midway) is reported,
// never auto-fixed: an operator inspects and forces the version.
func MigrateUp(ctx context.Context, databaseURL string, log *slog.Logger) (uint, error) {
	return migrateUp(ctx, databaseURL, migrations.FS, MigrationsTable, DefaultMigrateOptions, log)
}

// migrateUp applies the migrations in src. Each migration file is sent as a
// single multi-statement query, which Postgres runs as one implicit
// transaction: a failing file leaves no partial schema (TestMigrationFileIsAtomic).
func migrateUp(ctx context.Context, databaseURL string, src fs.FS, table string, opts MigrateOptions, log *slog.Logger) (uint, error) {
	m, err := newMigrator(databaseURL, src, table, opts)
	if err != nil {
		return 0, err
	}
	defer closeMigrator(m, log)

	stop := context.AfterFunc(ctx, func() { m.GracefulStop <- true })
	defer stop()

	from, dirty, err := m.Version()
	if err != nil && !errors.Is(err, migrate.ErrNilVersion) {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	if dirty {
		return from, fmt.Errorf("database is dirty at version %d: inspect and fix manually", from)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return 0, fmt.Errorf("migrate up: %w", err)
	}
	to, _, err := m.Version()
	if err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	log.InfoContext(ctx, "migrations applied", slog.Uint64("from", uint64(from)), slog.Uint64("to", uint64(to)))
	return to, nil
}

// SchemaVersion reports the applied version and whether it is dirty.
func SchemaVersion(databaseURL string, log *slog.Logger) (uint, bool, error) {
	m, err := newMigrator(databaseURL, migrations.FS, MigrationsTable, DefaultMigrateOptions)
	if err != nil {
		return 0, false, err
	}
	defer closeMigrator(m, log)
	v, dirty, err := m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read schema version: %w", err)
	}
	return v, dirty, nil
}

func newMigrator(databaseURL string, fsys fs.FS, table string, opts MigrateOptions) (*migrate.Migrate, error) {
	src, err := iofs.New(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("load migrations: %w", err)
	}
	dsn, err := migrateDSN(databaseURL, table, opts)
	if err != nil {
		return nil, err
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect for migrations: %w", err)
	}
	return m, nil
}

func closeMigrator(m *migrate.Migrate, log *slog.Logger) {
	srcErr, dbErr := m.Close()
	if err := errors.Join(srcErr, dbErr); err != nil {
		log.Warn("close migrator", slog.Any("error", err))
	}
}

// migrateDSN converts a postgres:// URL to golang-migrate's pgx5:// scheme,
// pins its bookkeeping table, and sets lock and statement timeouts as
// connection startup parameters (pgx passes unknown params to the server).
func migrateDSN(databaseURL, table string, opts MigrateOptions) (string, error) {
	u, err := url.Parse(databaseURL)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return "", errors.New("DATABASE_URL must be a postgres:// URL")
	}
	u.Scheme = "pgx5"
	q := u.Query()
	q.Set("x-migrations-table", table)
	q.Set("lock_timeout", strconv.FormatInt(opts.LockTimeout.Milliseconds(), 10))
	q.Set("statement_timeout", strconv.FormatInt(opts.StatementTimeout.Milliseconds(), 10))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// redactURL hides the password for logs.
func redactURL(databaseURL string) string {
	u, err := url.Parse(databaseURL)
	if err != nil {
		return "<invalid>"
	}
	return strings.Replace(u.Redacted(), "xxxxx", "***", 1)
}
