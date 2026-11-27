// Package db opens Postgres connections and applies schema migrations.
package db

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // registers pgx5://
	"github.com/golang-migrate/migrate/v4/source/iofs"

	"github.com/ticatwolves/notifex/migrations"
)

// MigrationsTable is golang-migrate's bookkeeping table.
const MigrationsTable = "schema_migrations"

// MigrateUp applies all pending migrations. It is safe to run concurrently
// (golang-migrate takes a Postgres advisory lock) and is a no-op when the
// schema is current. A dirty database (a migration failed midway) is reported,
// never auto-fixed: an operator inspects and forces the version.
func MigrateUp(ctx context.Context, databaseURL string, log *slog.Logger) (uint, error) {
	m, err := newMigrator(databaseURL)
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
	m, err := newMigrator(databaseURL)
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

func newMigrator(databaseURL string) (*migrate.Migrate, error) {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("load migrations: %w", err)
	}
	dsn, err := migrateDSN(databaseURL)
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

// migrateDSN converts a postgres:// URL to golang-migrate's pgx5:// scheme and
// pins its bookkeeping table.
func migrateDSN(databaseURL string) (string, error) {
	u, err := url.Parse(databaseURL)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return "", errors.New("DATABASE_URL must be a postgres:// URL")
	}
	u.Scheme = "pgx5"
	q := u.Query()
	q.Set("x-migrations-table", MigrationsTable)
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
