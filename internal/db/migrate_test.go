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
	"testing/fstest"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ticatwolves/notifex/internal/db"
	"github.com/ticatwolves/notifex/internal/store/storetest"
	"github.com/ticatwolves/notifex/migrations"
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

// TestMigrationFileIsAtomic backs `assume_in_transaction` in .squawk.toml:
// a migration file that fails part way leaves nothing behind.
func TestMigrationFileIsAtomic(t *testing.T) {
	sqlDB := storetest.DB(t)
	url := storetest.URL(t)
	ctx := context.Background()
	sfx := strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	src := fstest.MapFS{
		"1_ok.up.sql":  {Data: []byte("CREATE TABLE atomic_ok_" + sfx + " (id int);")},
		"2_bad.up.sql": {Data: []byte("CREATE TABLE atomic_half_" + sfx + " (id int);\nCREATE INDEX ON atomic_half_" + sfx + " (id);\nSELECT 1/0;")},
	}
	t.Cleanup(func() {
		_, _ = sqlDB.Exec(`DROP TABLE IF EXISTS atomic_ok_` + sfx + `, atomic_half_` + sfx + `, mig_atomic_` + sfx)
	})
	_, err := db.MigrateUpWith(ctx, url, src, "mig_atomic_"+sfx, db.DefaultMigrateOptions, quiet)
	require.ErrorContains(t, err, "division by zero")

	exists := func(name string) bool {
		var ok bool
		require.NoError(t, sqlDB.QueryRow(`SELECT to_regclass($1) IS NOT NULL`, name).Scan(&ok))
		return ok
	}
	assert.True(t, exists("atomic_ok_"+sfx), "earlier migration applied")
	assert.False(t, exists("atomic_half_"+sfx), "the failing file rolled back as a whole")
}

// TestMigrationsRunWithTimeouts backs excluding squawk's require-lock-timeout
// and require-statement-timeout: the migration connection carries both, and
// DDL gives up instead of queueing behind a lock.
func TestMigrationsRunWithTimeouts(t *testing.T) {
	sqlDB := storetest.DB(t)
	url := storetest.URL(t)
	ctx := context.Background()
	sfx := strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	tbl := "locked_" + sfx
	_, err := sqlDB.Exec(`CREATE TABLE ` + tbl + ` (id int)`)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = sqlDB.Exec(`DROP TABLE IF EXISTS ` + tbl + `, mig_to_` + sfx) })

	check := fstest.MapFS{"1_check.up.sql": {Data: []byte(`DO $$ BEGIN
  IF current_setting('lock_timeout') <> '200ms' OR current_setting('statement_timeout') <> '30s' THEN
    RAISE EXCEPTION 'timeouts not set: lock=% stmt=%', current_setting('lock_timeout'), current_setting('statement_timeout');
  END IF;
END $$;
ALTER TABLE ` + tbl + ` ADD COLUMN note text;`)}}

	holder, err := sqlDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	// Registered after the DROP cleanup, so it runs first (LIFO): a failed
	// assertion must not leave the lock held while the table is dropped.
	t.Cleanup(func() { _ = holder.Rollback() })
	_, err = holder.Exec(`LOCK TABLE ` + tbl + ` IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)

	opts := db.MigrateOptions{LockTimeout: 200 * time.Millisecond, StatementTimeout: 30 * time.Second}
	start := time.Now()
	_, err = db.MigrateUpWith(ctx, url, check, "mig_to_"+sfx, opts, quiet)
	require.ErrorContains(t, err, "lock timeout")
	assert.Less(t, time.Since(start), 10*time.Second, "gave up instead of queueing behind the lock")
}
