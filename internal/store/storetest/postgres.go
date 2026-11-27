// Package storetest provides a real PostgreSQL for integration tests: one
// container per test binary, migrated with the embedded production migrations
// (not ent auto-migrate), so tests exercise exactly what ships.
//
// Tests share the database and stay independent by creating their own
// tenants. Set NOTIFEX_TEST_DATABASE_URL to use an existing empty database
// instead of a container. `go test -short` skips these tests.
package storetest

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/sbshobhit/notifex/internal/db"
)

// Image matches docker-compose.yml and ent/migrate/main.go.
const Image = "postgres:18-alpine@sha256:0115267ab5a509022df9319c77be365cef335adb71556c75f60f5360ee247e71"

var (
	once      sync.Once
	shared    *sql.DB
	sharedURL string
	initErr   error
)

// DB returns a migrated *sql.DB shared by all tests in the binary.
func DB(t testing.TB) *sql.DB {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test: needs Docker/Postgres (run without -short)")
	}
	once.Do(func() { shared, sharedURL, initErr = start() })
	if initErr != nil {
		t.Fatalf("storetest: %v", initErr)
	}
	return shared
}

// URL returns the shared database URL (after DB has been called).
func URL(t testing.TB) string {
	t.Helper()
	DB(t)
	return sharedURL
}

func start() (*sql.DB, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	url := os.Getenv("NOTIFEX_TEST_DATABASE_URL")
	if url == "" {
		// The container is reaped by testcontainers' Ryuk sidecar when the
		// test binary exits.
		pg, err := postgres.Run(ctx, Image,
			postgres.WithDatabase("notifex_test"), postgres.WithUsername("test"), postgres.WithPassword("test"),
			postgres.BasicWaitStrategies())
		if err != nil {
			return nil, "", fmt.Errorf("start postgres container: %w", err)
		}
		if url, err = pg.ConnectionString(ctx, "sslmode=disable"); err != nil {
			return nil, "", fmt.Errorf("postgres connection string: %w", err)
		}
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := db.MigrateUp(ctx, url, log); err != nil {
		return nil, "", err
	}
	conn, err := db.Open(ctx, url, db.PoolConfig{MaxOpen: 20, MaxIdle: 20, MaxLifetime: time.Hour, MaxIdleTime: time.Hour})
	if err != nil {
		return nil, "", err
	}
	return conn, url, nil
}
