package entstore

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/ticatwolves/notifex/ent"
	"github.com/ticatwolves/notifex/ent/auththrottle"
	"github.com/ticatwolves/notifex/internal/store"
)

type throttle struct{ c *ent.Client }

func (t throttle) BlockedUntil(ctx context.Context, key string, now time.Time) (time.Time, error) {
	row, err := t.c.AuthThrottle.Query().Where(auththrottle.ID(key)).Only(ctx)
	if ent.IsNotFound(err) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("throttle.blocked_until: %w", mapErr(err, opRead))
	}
	if row.BlockedUntil.After(now) {
		return row.BlockedUntil, nil
	}
	return time.Time{}, nil
}

// RecordFailure is a single upsert that counts the failure (restarting the
// count once the window has passed) and computes the new block, so failures
// racing across API instances are never undercounted.
func (t throttle) RecordFailure(ctx context.Context, key string, now time.Time, p store.ThrottlePolicy) (store.ThrottleState, error) {
	if p.FreeFailures < 1 || p.Window <= 0 || p.MaxBackoff <= 0 {
		return store.ThrottleState{}, invalid("throttle policy")
	}
	const newFailures = `CASE WHEN auth_throttles.last_failure_at < $2::timestamptz - make_interval(secs => $3) THEN 1 ELSE auth_throttles.failures + 1 END`
	q := `
		INSERT INTO auth_throttles (key, failures, last_failure_at, blocked_until)
		VALUES ($1, 1, $2, $6)
		ON CONFLICT (key) DO UPDATE SET
			failures = ` + newFailures + `,
			last_failure_at = $2,
			blocked_until = CASE
				WHEN (` + newFailures + `) >= $4
				THEN $2::timestamptz + make_interval(secs => LEAST($5::float8, power(2, (` + newFailures + `) - $4 + 1)))
				ELSE $2::timestamptz END
		RETURNING failures, blocked_until`
	rows, err := t.c.QueryContext(ctx, q, key, now, p.Window.Seconds(), p.FreeFailures, p.MaxBackoff.Seconds(), firstBlock(now, p))
	if err != nil {
		return store.ThrottleState{}, fmt.Errorf("throttle.record_failure: %w", mapErr(err, opWrite))
	}
	defer func() { _ = rows.Close() }()
	var st store.ThrottleState
	if !rows.Next() {
		return st, fmt.Errorf("throttle.record_failure: no row returned: %w", rows.Err())
	}
	if err := rows.Scan(&st.Failures, &st.BlockedUntil); err != nil {
		return st, fmt.Errorf("throttle.record_failure: %w", err)
	}
	return st, nil
}

func (t throttle) Reset(ctx context.Context, key string) error {
	if _, err := t.c.AuthThrottle.Delete().Where(auththrottle.ID(key)).Exec(ctx); err != nil {
		return fmt.Errorf("throttle.reset: %w", mapErr(err, opDelete))
	}
	return nil
}

// firstBlock is the block for a key's first recorded failure.
func firstBlock(now time.Time, p store.ThrottlePolicy) time.Time {
	if 1 < p.FreeFailures {
		return now
	}
	secs := math.Min(p.MaxBackoff.Seconds(), math.Pow(2, float64(1-p.FreeFailures+1)))
	return now.Add(time.Duration(secs * float64(time.Second)))
}
