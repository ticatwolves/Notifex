package partition_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ticatwolves/notifex/internal/db"
	"github.com/ticatwolves/notifex/internal/db/partition"
	"github.com/ticatwolves/notifex/internal/store/storetest"
)

func date(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t
}

// parent creates a uniquely named table partitioned like notifications.
func parent(t *testing.T, sqlDB *sql.DB) string {
	t.Helper()
	name := "pt_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	_, err := sqlDB.Exec(fmt.Sprintf(
		`CREATE TABLE %s (id uuid NOT NULL, created_at timestamptz NOT NULL, body text, PRIMARY KEY (id, created_at))
		 PARTITION BY RANGE (created_at)`, name))
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = sqlDB.Exec(`DROP TABLE IF EXISTS ` + name + ` CASCADE`) })
	return name
}

func manager(sqlDB *sql.DB, now time.Time) *partition.Manager {
	m := partition.New(sqlDB)
	m.Now = func() time.Time { return now }
	m.LockTimeout = 2 * time.Second
	return m
}

func insert(t *testing.T, sqlDB *sql.DB, table string, at time.Time) (string, error) {
	t.Helper()
	var part string
	err := sqlDB.QueryRow(fmt.Sprintf(
		`INSERT INTO %s (id, created_at) VALUES ($1, $2) RETURNING tableoid::regclass::text`, table),
		uuid.New(), at).Scan(&part)
	return part, err
}

func TestEnsureCreatesContiguousDailyPartitionsIdempotently(t *testing.T) {
	t.Parallel()
	sqlDB := storetest.DB(t)
	ctx := context.Background()
	table := parent(t, sqlDB)
	m := manager(sqlDB, time.Now())

	// 15:00 in India is 09:30 UTC on the same day: days are UTC.
	from := date("2026-03-10T15:00:00+05:30")
	created, err := m.Ensure(ctx, table, from, 5)
	require.NoError(t, err)
	require.Len(t, created, 5)
	assert.Equal(t, table+"_p20260310", created[0])
	assert.Equal(t, table+"_p20260314", created[4])

	again, err := m.Ensure(ctx, table, from, 5)
	require.NoError(t, err)
	assert.Empty(t, again, "second run creates nothing")

	more, err := m.Ensure(ctx, table, from, 7)
	require.NoError(t, err)
	assert.Equal(t, []string{table + "_p20260315", table + "_p20260316"}, more, "extending only adds the missing days")

	parts, err := m.List(ctx, table)
	require.NoError(t, err)
	require.Len(t, parts, 7)
	for i, p := range parts {
		day := date("2026-03-10T00:00:00Z").AddDate(0, 0, i)
		assert.True(t, p.From.Equal(day), "partition %d from %s", i, p.From)
		assert.True(t, p.To.Equal(day.AddDate(0, 0, 1)), "partition %d to %s", i, p.To)
		assert.Equal(t, partition.Name(table, day), p.Name)
	}
	cov, err := m.Coverage(ctx, table)
	require.NoError(t, err)
	assert.True(t, cov.Equal(date("2026-03-17T00:00:00Z")))
}

func TestRowsRouteByUTCDayAndFailOutsideCoverage(t *testing.T) {
	t.Parallel()
	sqlDB := storetest.DB(t)
	table := parent(t, sqlDB)
	_, err := manager(sqlDB, time.Now()).Ensure(context.Background(), table, date("2026-03-10T00:00:00Z"), 2)
	require.NoError(t, err)

	tests := []struct {
		at   string
		want string
	}{
		{"2026-03-10T00:00:00Z", "_p20260310"},
		{"2026-03-10T23:59:59.999999Z", "_p20260310"},
		{"2026-03-11T00:00:00Z", "_p20260311"},
		{"2026-03-11T04:00:00+05:30", "_p20260310"}, // 22:30 UTC on the 10th
	}
	for _, tt := range tests {
		part, err := insert(t, sqlDB, table, date(tt.at))
		require.NoError(t, err, tt.at)
		assert.Equal(t, table+tt.want, part, tt.at)
	}

	// No DEFAULT partition: rows beyond coverage fail loudly.
	_, err = insert(t, sqlDB, table, date("2026-03-12T00:00:00Z"))
	require.ErrorContains(t, err, "no partition")
}

func TestEnsureConcurrentRunsCreateEachDayOnce(t *testing.T) {
	t.Parallel()
	sqlDB := storetest.DB(t)
	ctx := context.Background()
	table := parent(t, sqlDB)
	m := manager(sqlDB, time.Now())

	var mu sync.Mutex
	var all []string
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			created, err := m.Ensure(ctx, table, date("2026-05-01T00:00:00Z"), 10)
			errs <- err
			mu.Lock()
			all = append(all, created...)
			mu.Unlock()
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	assert.Len(t, all, 10, "each day created exactly once across racing runs")
	parts, err := m.List(ctx, table)
	require.NoError(t, err)
	assert.Len(t, parts, 10)
}

func TestEnsureRefusesToHideGapBehindDetachedTable(t *testing.T) {
	t.Parallel()
	sqlDB := storetest.DB(t)
	table := parent(t, sqlDB)
	// A plain table squatting on the partition name (e.g. a detached leftover).
	_, err := sqlDB.Exec(fmt.Sprintf(`CREATE TABLE %s_p20260401 (LIKE %s)`, table, table))
	require.NoError(t, err)

	_, err = manager(sqlDB, time.Now()).Ensure(context.Background(), table, date("2026-04-01T00:00:00Z"), 1)
	require.Error(t, err, "must not report success while the day has no partition")
	_, err = insert(t, sqlDB, table, date("2026-04-01T12:00:00Z"))
	require.ErrorContains(t, err, "no partition")
}

func TestDropRemovesOnlyExpiredPartitions(t *testing.T) {
	t.Parallel()
	sqlDB := storetest.DB(t)
	ctx := context.Background()
	table := parent(t, sqlDB)
	m := manager(sqlDB, date("2026-04-15T10:00:00Z")) // MinRetention 90d -> oldest droppable cutoff 2026-01-15

	_, err := m.Ensure(ctx, table, date("2026-01-01T00:00:00Z"), 10)
	require.NoError(t, err)
	for i := range 10 {
		_, err := insert(t, sqlDB, table, date("2026-01-01T12:00:00Z").AddDate(0, 0, i))
		require.NoError(t, err)
	}

	// Cutoff mid-day is truncated to the day: partitions ending on or before
	// 2026-01-05 00:00 go (Jan 1-4); Jan 5 onwards stays.
	dropped, err := m.Drop(ctx, table, date("2026-01-05T18:00:00Z"))
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{
		table + "_p20260101", table + "_p20260102", table + "_p20260103", table + "_p20260104",
	}, dropped)

	parts, err := m.List(ctx, table)
	require.NoError(t, err)
	require.Len(t, parts, 6)
	assert.True(t, parts[0].From.Equal(date("2026-01-05T00:00:00Z")))

	var n int
	require.NoError(t, sqlDB.QueryRow(`SELECT count(*) FROM `+table).Scan(&n))
	assert.Equal(t, 6, n, "rows in kept partitions are intact")
	var exists bool
	require.NoError(t, sqlDB.QueryRow(`SELECT to_regclass($1) IS NOT NULL`, table+"_p20260101").Scan(&exists))
	assert.False(t, exists, "dropped, not just detached")

	again, err := m.Drop(ctx, table, date("2026-01-05T18:00:00Z"))
	require.NoError(t, err)
	assert.Empty(t, again, "idempotent")
}

func TestDropRetentionGuard(t *testing.T) {
	t.Parallel()
	sqlDB := storetest.DB(t)
	ctx := context.Background()
	table := parent(t, sqlDB)
	m := manager(sqlDB, date("2026-04-15T10:00:00Z"))
	_, err := m.Ensure(ctx, table, date("2026-01-10T00:00:00Z"), 30)
	require.NoError(t, err)

	for _, cutoff := range []string{"2026-01-16T00:00:00Z", "2026-04-15T00:00:00Z", "2027-01-01T00:00:00Z"} {
		dropped, err := m.Drop(ctx, table, date(cutoff))
		require.ErrorIs(t, err, partition.ErrRetentionGuard, cutoff)
		assert.Empty(t, dropped)
	}
	parts, err := m.List(ctx, table)
	require.NoError(t, err)
	assert.Len(t, parts, 30, "nothing dropped when the guard trips")

	dropped, err := m.Drop(ctx, table, date("2026-01-15T00:00:00Z"))
	require.NoError(t, err, "exactly at the guard is allowed")
	assert.Len(t, dropped, 5) // Jan 10-14
}

func TestDropCleansUpInterruptedRuns(t *testing.T) {
	t.Parallel()
	sqlDB := storetest.DB(t)
	ctx := context.Background()
	table := parent(t, sqlDB)
	m := manager(sqlDB, date("2026-06-30T00:00:00Z"))
	_, err := m.Ensure(ctx, table, date("2026-01-01T00:00:00Z"), 4)
	require.NoError(t, err)

	// A run that died between DETACH and DROP leaves a detached table behind.
	_, err = sqlDB.Exec(fmt.Sprintf(`ALTER TABLE %s DETACH PARTITION %s_p20260101`, table, table))
	require.NoError(t, err)
	// An unrelated table that merely shares the prefix must survive.
	_, err = sqlDB.Exec(fmt.Sprintf(`CREATE TABLE %s_archive (id int)`, table))
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = sqlDB.Exec(fmt.Sprintf(`DROP TABLE IF EXISTS %s_archive`, table)) })

	dropped, err := m.Drop(ctx, table, date("2026-01-03T00:00:00Z"))
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{table + "_p20260101", table + "_p20260102"}, dropped)
	var exists bool
	require.NoError(t, sqlDB.QueryRow(`SELECT to_regclass($1) IS NOT NULL`, table+"_archive").Scan(&exists))
	assert.True(t, exists)
}

func TestDropFinalizesPendingDetach(t *testing.T) {
	t.Parallel()
	sqlDB := storetest.DB(t)
	ctx := context.Background()
	table := parent(t, sqlDB)
	m := manager(sqlDB, date("2026-06-30T00:00:00Z"))
	_, err := m.Ensure(ctx, table, date("2026-01-01T00:00:00Z"), 3)
	require.NoError(t, err)

	// An open transaction that has read the parent makes DETACH CONCURRENTLY
	// wait in its second phase. Cancelling it there, server-side, leaves the
	// partition "detach pending", as a crashed scheduler would. The cancel must
	// land before the reader commits, or the detach simply completes.
	reader, err := sqlDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = reader.Exec(`SELECT count(*) FROM ` + table)
	require.NoError(t, err)

	detacher, err := sqlDB.Conn(ctx)
	require.NoError(t, err)
	var pid int
	require.NoError(t, detacher.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
	detachErr := make(chan error, 1)
	go func() {
		_, err := detacher.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE %s DETACH PARTITION %s_p20260101 CONCURRENTLY`, table, table))
		detachErr <- err
	}()
	require.Eventually(t, func() bool { // blocked in phase 2, waiting on the reader
		var waiting bool
		_ = sqlDB.QueryRow(`SELECT wait_event_type IS NOT NULL AND state = 'active' FROM pg_stat_activity WHERE pid = $1`, pid).Scan(&waiting)
		return waiting
	}, 5*time.Second, 20*time.Millisecond)
	var cancelled bool
	require.NoError(t, sqlDB.QueryRow(`SELECT pg_cancel_backend($1)`, pid).Scan(&cancelled))
	require.True(t, cancelled)
	require.Error(t, <-detachErr, "detach was cancelled")
	_ = detacher.Close()
	require.NoError(t, reader.Commit())

	var pending bool
	require.NoError(t, sqlDB.QueryRow(`SELECT inhdetachpending FROM pg_inherits WHERE inhrelid = to_regclass($1)`, table+"_p20260101").Scan(&pending))
	require.True(t, pending, "setup: partition should be detach-pending")

	dropped, err := m.Drop(ctx, table, date("2026-01-02T00:00:00Z"))
	require.NoError(t, err)
	assert.Contains(t, dropped, table+"_p20260101")
}

func TestDDLRespectsLockTimeout(t *testing.T) {
	t.Parallel()
	sqlDB := storetest.DB(t)
	ctx := context.Background()
	table := parent(t, sqlDB)
	m := manager(sqlDB, time.Now())
	m.LockTimeout = 200 * time.Millisecond

	holder, err := sqlDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = holder.Exec(`LOCK TABLE ` + table + ` IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)

	start := time.Now()
	_, err = m.Ensure(ctx, table, date("2026-03-01T00:00:00Z"), 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "lock timeout")
	assert.Less(t, time.Since(start), 3*time.Second, "gives up instead of queueing behind the lock")

	require.NoError(t, holder.Rollback())
	_, err = m.Ensure(ctx, table, date("2026-03-01T00:00:00Z"), 1)
	require.NoError(t, err, "succeeds once the lock is released")
}

func TestSessionSettingsDoNotLeakToPool(t *testing.T) {
	t.Parallel()
	// A one-connection pool guarantees the next query reuses the same session.
	single, err := db.Open(context.Background(), storetest.URL(t), db.PoolConfig{MaxOpen: 1, MaxIdle: 1, MaxLifetime: time.Hour, MaxIdleTime: time.Hour})
	require.NoError(t, err)
	t.Cleanup(func() { _ = single.Close() })
	table := parent(t, storetest.DB(t))

	m := manager(single, time.Now())
	m.LockTimeout = 1234 * time.Millisecond
	_, err = m.Ensure(context.Background(), table, date("2026-03-01T00:00:00Z"), 1)
	require.NoError(t, err)

	var lt string
	require.NoError(t, single.QueryRow(`SHOW lock_timeout`).Scan(&lt))
	assert.Equal(t, "0", lt)
}

func TestValidation(t *testing.T) {
	t.Parallel()
	sqlDB := storetest.DB(t)
	ctx := context.Background()
	m := manager(sqlDB, time.Now())
	plain := "plain_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	listPart := "listp_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	_, err := sqlDB.Exec(fmt.Sprintf(`CREATE TABLE %s (created_at timestamptz)`, plain))
	require.NoError(t, err)
	_, err = sqlDB.Exec(fmt.Sprintf(`CREATE TABLE %s (kind text) PARTITION BY LIST (kind)`, listPart))
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = sqlDB.Exec(`DROP TABLE IF EXISTS ` + plain + `, ` + listPart) })

	_, err = m.Ensure(ctx, `x"; DROP TABLE users; --`, time.Now(), 1)
	require.ErrorContains(t, err, "invalid table name")
	_, err = m.Ensure(ctx, plain, time.Now(), 1)
	require.ErrorIs(t, err, partition.ErrNotPartitioned)
	_, err = m.Ensure(ctx, listPart, time.Now(), 1)
	require.ErrorIs(t, err, partition.ErrNotPartitioned)
	_, err = m.Ensure(ctx, "does_not_exist", time.Now(), 1)
	require.ErrorIs(t, err, partition.ErrNotPartitioned)

	table := parent(t, sqlDB)
	for _, days := range []int{0, -1, partition.MaxAhead + 1} {
		_, err = m.Ensure(ctx, table, time.Now(), days)
		require.Error(t, err, "days=%d", days)
	}
}

func TestNameAndDay(t *testing.T) {
	assert.Equal(t, "notifications_p20261127", partition.Name("notifications", date("2026-11-27T23:30:00Z")))
	assert.Equal(t, "notifications_p20261127", partition.Name("notifications", date("2026-11-28T04:00:00+05:30")))
	assert.True(t, partition.Day(date("2026-11-28T04:00:00+05:30")).Equal(date("2026-11-27T00:00:00Z")))
}
