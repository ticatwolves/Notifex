// Package scheduler is the leader-elected background loop (ADR-004,
// ADR-014 §7): outbox relay, scheduled-dispatch mover, partition
// maintenance and cleanup. Exactly one scheduler task is leader at a time,
// through a lease row; the others stand by.
package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ticatwolves/notifex/internal/db/partition"
	"github.com/ticatwolves/notifex/internal/dispatch"
	"github.com/ticatwolves/notifex/internal/store"
)

// Defaults (ADR-014 §7).
const (
	LeaseName        = "scheduler"
	LeaseTTL         = 15 * time.Second
	RenewEvery       = 5 * time.Second
	Tick             = time.Second
	MaintainEvery    = time.Hour
	BatchSize        = 100
	PartitionsAhead  = 14
	PublishedKeepFor = 24 * time.Hour
)

// PartitionedTables are kept PartitionsAhead days ahead.
var PartitionedTables = []string{"notifications", "notification_events"}

// Scheduler runs the leader jobs. It is a lifecycle.Component.
type Scheduler struct {
	st     store.Store
	parts  *partition.Manager
	q      dispatch.JobQueue
	holder string
	log    *slog.Logger
	now    func() time.Time

	leader       bool
	lastRenew    time.Time
	lastMaintain time.Time

	mLeader   prometheus.Gauge
	mLag      prometheus.Gauge
	mRelayed  prometheus.Counter
	mMoved    prometheus.Counter
	mFailures *prometheus.CounterVec
}

// New builds a scheduler. holder identifies this task (hostname + pid).
func New(st store.Store, db *sql.DB, q dispatch.JobQueue, holder string, log *slog.Logger, reg prometheus.Registerer) *Scheduler {
	s := &Scheduler{
		st: st, q: q, holder: holder, log: log, now: time.Now,
		mLeader:   prometheus.NewGauge(prometheus.GaugeOpts{Name: "notifex_scheduler_leader", Help: "1 while this task holds the scheduler lease."}),
		mLag:      prometheus.NewGauge(prometheus.GaugeOpts{Name: "notifex_outbox_lag_seconds", Help: "Age of the oldest unpublished outbox row."}),
		mRelayed:  prometheus.NewCounter(prometheus.CounterOpts{Name: "notifex_outbox_relayed_total", Help: "Outbox rows published by the relay."}),
		mMoved:    prometheus.NewCounter(prometheus.CounterOpts{Name: "notifex_scheduled_moved_total", Help: "Due scheduled_dispatch rows moved to the outbox."}),
		mFailures: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "notifex_scheduler_job_failures_total", Help: "Scheduler job failures by job."}, []string{"job"}),
	}
	if db != nil {
		s.parts = partition.New(db)
	}
	if reg != nil {
		reg.MustRegister(s.mLeader, s.mLag, s.mRelayed, s.mMoved, s.mFailures)
	}
	return s
}

// SetClock overrides the clock (tests).
func (s *Scheduler) SetClock(now func() time.Time) { s.now = now }

// Name implements lifecycle.Component.
func (s *Scheduler) Name() string { return "scheduler" }

// Start implements lifecycle.Component: it ticks until ctx is done.
func (s *Scheduler) Start(ctx context.Context) error {
	t := time.NewTicker(Tick)
	defer t.Stop()
	for {
		s.Step(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Stop implements lifecycle.Component: it gives up the lease so another
// task takes over at once.
func (s *Scheduler) Stop(ctx context.Context) error {
	if !s.leader {
		return nil
	}
	s.leader = false
	s.mLeader.Set(0)
	if err := s.st.Leases().Release(ctx, LeaseName, s.holder); err != nil {
		return fmt.Errorf("scheduler: release lease: %w", err)
	}
	return nil
}

// IsLeader reports whether this task held the lease at its last renewal.
func (s *Scheduler) IsLeader() bool { return s.leader }

// Step renews (or takes) the lease and, as leader, runs one round of jobs.
func (s *Scheduler) Step(ctx context.Context) {
	now := s.now()
	if !s.leader || now.Sub(s.lastRenew) >= RenewEvery {
		ok, err := s.st.Leases().Acquire(ctx, LeaseName, s.holder, now, LeaseTTL)
		if err != nil {
			s.fail(ctx, "lease", err)
			ok = false
		}
		if ok != s.leader {
			s.log.InfoContext(ctx, "scheduler leadership changed", slog.Bool("leader", ok))
		}
		s.leader = ok
		s.lastRenew = now
		s.mLeader.Set(map[bool]float64{true: 1, false: 0}[ok])
	}
	if !s.leader {
		return
	}
	if _, err := s.MoveDue(ctx); err != nil {
		s.fail(ctx, "move", err)
	}
	for {
		n, err := s.RelayOnce(ctx)
		if err != nil {
			s.fail(ctx, "relay", err)
			break
		}
		if n < BatchSize || ctx.Err() != nil {
			break
		}
	}
	if oldest, err := s.st.Outbox().OldestUnpublished(ctx); err == nil {
		lag := 0.0
		if !oldest.IsZero() {
			lag = now.Sub(oldest).Seconds()
		}
		s.mLag.Set(lag)
	}
	if now.Sub(s.lastMaintain) >= MaintainEvery {
		if err := s.Maintain(ctx); err != nil {
			s.fail(ctx, "maintain", err)
		} else {
			s.lastMaintain = now
		}
	}
}

func (s *Scheduler) fail(ctx context.Context, job string, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	s.mFailures.WithLabelValues(job).Inc()
	s.log.WarnContext(ctx, "scheduler job failed", slog.String("job", job), slog.Any("error", err))
}

// RelayOnce publishes up to BatchSize unpublished outbox rows: lock them
// (SKIP LOCKED, so concurrent relays never take the same rows), publish,
// mark published, commit. A publish failure rolls back; the rows stay
// unpublished for the next round (ADR-004).
func (s *Scheduler) RelayOnce(ctx context.Context) (int, error) {
	n := 0
	err := s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		rows, err := tx.Outbox().ClaimUnpublished(ctx, BatchSize)
		if err != nil || len(rows) == 0 {
			return err
		}
		now := s.now().UTC()
		msgs := make([]dispatch.OutboundMessage, len(rows))
		ids := make([]int64, len(rows))
		for i, r := range rows {
			msgs[i] = dispatch.FromOutbox(r, now)
			ids[i] = r.ID
		}
		if err := s.q.Publish(ctx, msgs); err != nil {
			return err
		}
		if err := tx.Outbox().MarkPublished(ctx, ids, now); err != nil {
			return err
		}
		n = len(rows)
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("scheduler: relay: %w", err)
	}
	s.mRelayed.Add(float64(n))
	return n, nil
}

// MoveDue moves due scheduled_dispatch rows into the outbox in one
// transaction (taken rows are deleted as they are enqueued).
func (s *Scheduler) MoveDue(ctx context.Context) (int, error) {
	n := 0
	err := s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		now := s.now().UTC()
		due, err := tx.Scheduled().TakeDue(ctx, now, BatchSize)
		if err != nil || len(due) == 0 {
			return err
		}
		byTenant := map[store.TenantID][]store.OutboxMessage{}
		for _, d := range due {
			byTenant[d.TenantID] = append(byTenant[d.TenantID], store.OutboxMessage{
				TenantID: d.TenantID, NotificationID: d.NotificationID, RunID: d.RunID, Channel: d.Channel,
				Attempt: d.Attempt, TraceParent: d.TraceParent,
			})
		}
		for tid, msgs := range byTenant {
			if _, err := tx.Outbox().Enqueue(ctx, tid, msgs, now); err != nil {
				return err
			}
		}
		n = len(due)
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("scheduler: move due: %w", err)
	}
	s.mMoved.Add(float64(n))
	return n, nil
}

// Maintain keeps partitions ahead and deletes expired bookkeeping rows.
func (s *Scheduler) Maintain(ctx context.Context) error {
	now := s.now().UTC()
	if s.parts != nil {
		for _, t := range PartitionedTables {
			if _, err := s.parts.Ensure(ctx, t, now.AddDate(0, 0, -1), PartitionsAhead+2); err != nil {
				return fmt.Errorf("scheduler: partitions %s: %w", t, err)
			}
		}
	}
	if _, err := s.st.Outbox().DeletePublishedBefore(ctx, now.Add(-PublishedKeepFor), 10_000); err != nil {
		return fmt.Errorf("scheduler: outbox cleanup: %w", err)
	}
	if _, err := s.st.Idempotency().DeleteExpired(ctx, now, 10_000); err != nil {
		return fmt.Errorf("scheduler: idempotency cleanup: %w", err)
	}
	return nil
}
