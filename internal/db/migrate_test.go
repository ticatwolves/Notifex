package db_test

import (
	"context"
	"io"
	"io/fs"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sbshobhit/notifex/internal/db"
	"github.com/sbshobhit/notifex/internal/store/storetest"
	"github.com/sbshobhit/notifex/migrations"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func latestVersion(t *testing.T) uint {
	t.Helper()
	files, err := fs.Glob(migrations.FS, "*.up.sql")
	require.NoError(t, err)
	require.NotEmpty(t, files)
	var latest uint64
	for _, f := range files {
		v, err := strconv.ParseUint(strings.SplitN(f, "_", 2)[0], 10, 64)
		require.NoError(t, err, f)
		latest = max(latest, v)
	}
	return uint(latest)
}

func TestMigrateUpIsIdempotentAndConcurrentSafe(t *testing.T) {
	url := storetest.URL(t) // already migrated once by storetest
	want := latestVersion(t)

	// Several deploy tasks racing: the advisory lock serialises them and all
	// end at the latest version without error.
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Go(func() {
			v, err := db.MigrateUp(context.Background(), url, quiet)
			if err == nil && v != want {
				err = assert.AnError
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	v, dirty, err := db.SchemaVersion(url, quiet)
	require.NoError(t, err)
	assert.Equal(t, want, v)
	assert.False(t, dirty)
}

func TestMigrateRefusesDirtyDatabase(t *testing.T) {
	sqlDB := storetest.DB(t)
	url := storetest.URL(t)
	ctx := context.Background()
	_, err := sqlDB.ExecContext(ctx, `UPDATE `+db.MigrationsTable+` SET dirty = true`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = sqlDB.ExecContext(ctx, `UPDATE `+db.MigrationsTable+` SET dirty = false`)
	})
	_, err = db.MigrateUp(ctx, url, quiet)
	require.ErrorContains(t, err, "dirty")
}

func TestMigrateRejectsNonPostgresURL(t *testing.T) {
	_, err := db.MigrateUp(context.Background(), "mysql://u:p@h/db", quiet)
	require.ErrorContains(t, err, "postgres://")
}

func TestOpenRedactsPasswordOnError(t *testing.T) {
	_, err := db.Open(context.Background(), "postgres://notifex:s3cr3t-pw@127.0.0.1:1/x?sslmode=disable&connect_timeout=1", db.PoolConfig{})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "s3cr3t-pw")
}
