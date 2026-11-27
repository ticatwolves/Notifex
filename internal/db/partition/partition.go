// Package partition manages daily range partitions on created_at (ADR-006).
//
// notifications and notification_events are partitioned by day so metadata
// retention (90 days) is a cheap partition drop instead of a mass DELETE. The
// scheduler (leader) calls Ensure to keep partitions created ahead of time and
// Drop to remove expired ones (wired in Phase 5).
//
// Safety properties:
//   - every DDL statement runs under lock_timeout, so maintenance never queues
//     behind (and then blocks) the accept path; a timed-out run is retried later;
//   - drops use DETACH PARTITION ... CONCURRENTLY, then DROP, so they are not
//     blocked by, and do not block, ordinary reads and writes on the parent;
//   - Drop refuses to remove partitions newer than MinRetention, whatever cutoff
//     the caller passes;
//   - partition bounds are read from the catalog, not inferred from names;
//   - there is no DEFAULT partition: a row outside the created range fails
//     loudly rather than landing somewhere that makes later partition creation
//     scan it. Coverage reports how far ahead partitions exist (alert on it).
package partition

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Defaults.
const (
	DefaultLockTimeout = 5 * time.Second
	// DefaultMinRetention is the ADR-006 metadata retention: nothing younger is
	// ever dropped.
	DefaultMinRetention = 90 * 24 * time.Hour
	// MaxAhead bounds Ensure so a bad argument can't create thousands of tables.
	MaxAhead = 60
)

var (
	// ErrNotPartitioned means the table is not range-partitioned on one column.
	ErrNotPartitioned = errors.New("partition: table is not range-partitioned on a single column")
	// ErrRetentionGuard means Drop was asked to remove data younger than MinRetention.
	ErrRetentionGuard = errors.New("partition: cutoff is within the minimum retention period")

	tableRE = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,40}$`)
)

// Manager creates and drops daily partitions.
type Manager struct {
	db *sql.DB
	// LockTimeout bounds how long each DDL statement waits for locks.
	LockTimeout time.Duration
	// MinRetention is the youngest data Drop may remove.
	MinRetention time.Duration
	// Now overrides the clock (tests).
	Now func() time.Time
}

// New returns a Manager with production defaults.
func New(db *sql.DB) *Manager {
	return &Manager{db: db, LockTimeout: DefaultLockTimeout, MinRetention: DefaultMinRetention, Now: time.Now}
}

// Partition is one child partition with its bounds [From, To).
type Partition struct {
	Name string
	From time.Time
	To   time.Time
}

// Name returns the partition name for table and day: <table>_pYYYYMMDD (UTC).
func Name(table string, day time.Time) string {
	return table + "_p" + day.UTC().Format("20060102")
}

// Day truncates t to the start of its UTC day.
func Day(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// Ensure creates the daily partitions covering [Day(from), Day(from)+days).
// It is idempotent and safe to run concurrently. It returns the partitions it
// created.
func (m *Manager) Ensure(ctx context.Context, table string, from time.Time, days int) ([]string, error) {
	if days < 1 || days > MaxAhead {
		return nil, fmt.Errorf("partition: days must be in [1, %d], got %d", MaxAhead, days)
	}
	conn, err := m.conn(ctx, table)
	if err != nil {
		return nil, err
	}
	defer m.release(conn)

	existing, err := list(ctx, conn, table)
	if err != nil {
		return nil, err
	}
	have := make(map[time.Time]bool, len(existing))
	for _, p := range existing {
		have[p.From] = true
	}

	var created []string
	start := Day(from)
	for i := range days {
		day := start.AddDate(0, 0, i)
		if have[day] {
			continue
		}
		name := Name(table, day)
		// No IF NOT EXISTS: a same-named table that is *not* an attached
		// partition (e.g. a detached leftover) must not silently leave a gap.
		// DDL can't take bind parameters. Identifiers are regex-validated and
		// quoted; bounds are formatted from time.Time, never caller strings.
		stmt := fmt.Sprintf(`CREATE TABLE %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')`, //nolint:gosec // G201: see above
			ident(name), ident(table), day.Format(time.RFC3339), day.AddDate(0, 0, 1).Format(time.RFC3339))
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			var pg *pgconn.PgError
			if errors.As(err, &pg) && pg.Code == pgDuplicateTable {
				// Lost a race with a concurrent Ensure: fine only if the day is
				// now covered by an attached partition.
				ok, cerr := covered(ctx, conn, table, day)
				if cerr != nil {
					return created, cerr
				}
				if ok {
					continue
				}
			}
			return created, fmt.Errorf("partition: create %s: %w", name, err)
		}
		created = append(created, name)
	}
	return created, nil
}

// List returns the table's partitions ordered by From. Partitions that are not
// plain [from, to) timestamp ranges (e.g. DEFAULT) are omitted.
func (m *Manager) List(ctx context.Context, table string) ([]Partition, error) {
	conn, err := m.conn(ctx, table)
	if err != nil {
		return nil, err
	}
	defer m.release(conn)
	return list(ctx, conn, table)
}

// Coverage returns the exclusive upper bound of the newest partition: rows
// with created_at >= Coverage have nowhere to go. Zero if none exist.
func (m *Manager) Coverage(ctx context.Context, table string) (time.Time, error) {
	parts, err := m.List(ctx, table)
	if err != nil || len(parts) == 0 {
		return time.Time{}, err
	}
	return parts[len(parts)-1].To, nil
}

// Drop removes partitions whose upper bound is at or before Day(cutoff). It
// refuses cutoffs younger than MinRetention. It also finalises detaches left
// pending by an interrupted run and drops detached leftovers named like our
// partitions. It returns the tables dropped.
func (m *Manager) Drop(ctx context.Context, table string, cutoff time.Time) ([]string, error) {
	cutoff = Day(cutoff)
	if guard := Day(m.Now().Add(-m.MinRetention)); cutoff.After(guard) {
		return nil, fmt.Errorf("%w: cutoff %s, oldest allowed %s", ErrRetentionGuard, cutoff.Format(time.DateOnly), guard.Format(time.DateOnly))
	}
	conn, err := m.conn(ctx, table)
	if err != nil {
		return nil, err
	}
	defer m.release(conn)

	// 1. Finalise detaches interrupted mid-way (they block further detaches).
	pending, err := pendingDetach(ctx, conn, table)
	if err != nil {
		return nil, err
	}
	for _, name := range pending {
		if _, err := conn.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE %s DETACH PARTITION %s FINALIZE`, ident(table), ident(name))); err != nil {
			return nil, fmt.Errorf("partition: finalize detach %s: %w", name, err)
		}
	}

	// 2. Detach expired partitions concurrently, then drop them.
	parts, err := list(ctx, conn, table)
	if err != nil {
		return nil, err
	}
	var dropped []string
	for _, p := range parts {
		if p.To.After(cutoff) {
			continue
		}
		// DETACH ... CONCURRENTLY cannot run inside a transaction block; each
		// statement here is its own implicit transaction.
		if _, err := conn.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE %s DETACH PARTITION %s CONCURRENTLY`, ident(table), ident(p.Name))); err != nil {
			return dropped, fmt.Errorf("partition: detach %s: %w", p.Name, err)
		}
		if _, err := conn.ExecContext(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, ident(p.Name))); err != nil {
			return dropped, fmt.Errorf("partition: drop %s: %w", p.Name, err)
		}
		dropped = append(dropped, p.Name)
	}

	// 3. Detached leftovers from a run that died between DETACH and DROP.
	orphans, err := orphans(ctx, conn, table, cutoff)
	if err != nil {
		return dropped, err
	}
	for _, name := range orphans {
		if _, err := conn.ExecContext(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, ident(name))); err != nil {
			return dropped, fmt.Errorf("partition: drop orphan %s: %w", name, err)
		}
		dropped = append(dropped, name)
	}
	return dropped, nil
}

// conn pins a connection with lock_timeout set, after validating table.
func (m *Manager) conn(ctx context.Context, table string) (*sql.Conn, error) {
	if !tableRE.MatchString(table) {
		return nil, fmt.Errorf("partition: invalid table name %q", table)
	}
	conn, err := m.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("partition: acquire conn: %w", err)
	}
	ms := m.LockTimeout.Milliseconds()
	if ms <= 0 {
		ms = DefaultLockTimeout.Milliseconds()
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf(`SET lock_timeout = %d`, ms)); err != nil {
		m.discard(conn)
		return nil, fmt.Errorf("partition: set lock_timeout: %w", err)
	}
	var strategy string
	var nKeys int
	err = conn.QueryRowContext(ctx, `
		SELECT pt.partstrat, pt.partnatts
		FROM pg_partitioned_table pt
		JOIN pg_class c ON c.oid = pt.partrelid
		WHERE c.relname = $1 AND c.relnamespace = current_schema()::regnamespace`, table).Scan(&strategy, &nKeys)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (strategy != "r" || nKeys != 1)) {
		m.release(conn)
		return nil, fmt.Errorf("%w: %s", ErrNotPartitioned, table)
	}
	if err != nil {
		m.release(conn)
		return nil, fmt.Errorf("partition: inspect %s: %w", table, err)
	}
	return conn, nil
}

// release resets session state before the connection returns to the pool.
func (m *Manager) release(conn *sql.Conn) {
	if _, err := conn.ExecContext(context.Background(), `RESET lock_timeout`); err != nil {
		m.discard(conn)
		return
	}
	_ = conn.Close()
}

// discard closes the connection without returning it to the pool, so session
// settings can never leak to other callers.
func (m *Manager) discard(conn *sql.Conn) {
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	_ = conn.Close()
}

const pgDuplicateTable = "42P07"

func covered(ctx context.Context, conn *sql.Conn, table string, day time.Time) (bool, error) {
	parts, err := list(ctx, conn, table)
	if err != nil {
		return false, err
	}
	for _, p := range parts {
		if !p.From.After(day) && p.To.After(day) {
			return true, nil
		}
	}
	return false, nil
}

// list reads attached partitions and their bounds from the catalog. Bounds are
// extracted and cast back to timestamptz in SQL, so the session's TimeZone and
// DateStyle can't skew them.
func list(ctx context.Context, conn *sql.Conn, table string) ([]Partition, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT c.relname,
		       (regexp_match(pg_get_expr(c.relpartbound, c.oid), 'FROM \(''([^'']+)''\)'))[1]::timestamptz,
		       (regexp_match(pg_get_expr(c.relpartbound, c.oid), 'TO \(''([^'']+)''\)'))[1]::timestamptz
		FROM pg_inherits i
		JOIN pg_class c ON c.oid = i.inhrelid
		JOIN pg_class p ON p.oid = i.inhparent
		WHERE p.relname = $1 AND p.relnamespace = current_schema()::regnamespace
		  AND NOT i.inhdetachpending`, table)
	if err != nil {
		return nil, fmt.Errorf("partition: list %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	var out []Partition
	for rows.Next() {
		var name string
		var from, to sql.NullTime
		if err := rows.Scan(&name, &from, &to); err != nil {
			return nil, fmt.Errorf("partition: list %s: %w", table, err)
		}
		if !from.Valid || !to.Valid {
			continue // DEFAULT or MINVALUE/MAXVALUE bounds: not ours
		}
		out = append(out, Partition{Name: name, From: from.Time.UTC(), To: to.Time.UTC()})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("partition: list %s: %w", table, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].From.Before(out[j].From) })
	return out, nil
}

func pendingDetach(ctx context.Context, conn *sql.Conn, table string) ([]string, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT c.relname FROM pg_inherits i
		JOIN pg_class c ON c.oid = i.inhrelid
		JOIN pg_class p ON p.oid = i.inhparent
		WHERE p.relname = $1 AND p.relnamespace = current_schema()::regnamespace AND i.inhdetachpending`, table)
	if err != nil {
		return nil, fmt.Errorf("partition: pending detaches %s: %w", table, err)
	}
	return scanNames(rows)
}

// orphans finds detached tables named <table>_pYYYYMMDD whose day ends at or
// before cutoff. Names in that pattern are reserved for this package.
func orphans(ctx context.Context, conn *sql.Conn, table string, cutoff time.Time) ([]string, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT c.relname FROM pg_class c
		WHERE c.relnamespace = current_schema()::regnamespace
		  AND c.relkind = 'r' AND NOT c.relispartition
		  AND c.relname ~ ('^' || $1 || '_p[0-9]{8}$')
		  AND to_date(right(c.relname, 8), 'YYYYMMDD') + 1 <= $2::date`, table, cutoff.Format(time.DateOnly))
	if err != nil {
		return nil, fmt.Errorf("partition: orphans %s: %w", table, err)
	}
	return scanNames(rows)
}

func scanNames(rows *sql.Rows) ([]string, error) {
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("partition: scan: %w", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("partition: scan: %w", err)
	}
	return out, nil
}

func ident(name string) string { return pgx.Identifier{name}.Sanitize() }
