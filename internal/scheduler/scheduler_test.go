package scheduler_test

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ticatwolves/notifex/internal/audit"
	"github.com/ticatwolves/notifex/internal/crypto"
	"github.com/ticatwolves/notifex/internal/db/partition"
	"github.com/ticatwolves/notifex/internal/dispatch"
	"github.com/ticatwolves/notifex/internal/dispatch/inmem"
	"github.com/ticatwolves/notifex/internal/dispatch/sqsqueue"
	"github.com/ticatwolves/notifex/internal/dispatch/sqsqueue/sqstest"
	"github.com/ticatwolves/notifex/internal/scheduler"
	"github.com/ticatwolves/notifex/internal/store"
	"github.com/ticatwolves/notifex/internal/store/entstore"
	"github.com/ticatwolves/notifex/internal/store/storetest"
	"github.com/ticatwolves/notifex/internal/tenant"
)

// The relay, mover and lease are cross-tenant by design, so these tests
// share global state in the test database and run sequentially (no
// t.Parallel), or one test's relay would publish another's rows.
var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func newTenant(t *testing.T, st store.Store) store.TenantID {
	t.Helper()
	mk := make([]byte, 32)
	_, _ = rand.Read(mk)
	kp, err := crypto.NewLocal(mk)
	require.NoError(t, err)
	ten, err := tenant.Provision(context.Background(), st, kp, audit.System, tenant.ProvisionInput{
		Name: "Acme", Slug: "t-" + strings.ReplaceAll(uuid.NewString()[:10], "-", ""), Plan: "free",
	})
	require.NoError(t, err)
	return ten.ID
}

func enqueue(t *testing.T, st store.Store, tid store.TenantID, n int, channel string) []uuid.UUID {
	t.Helper()
	msgs := make([]store.OutboxMessage, n)
	ids := make([]uuid.UUID, n)
	for i := range msgs {
		ids[i] = uuid.Must(uuid.NewV7())
		msgs[i] = store.OutboxMessage{NotificationID: ids[i], RunID: uuid.Must(uuid.NewV7()), Channel: channel, TraceParent: "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"}
	}
	_, err := st.Outbox().Enqueue(context.Background(), tid, msgs, time.Now())
	require.NoError(t, err)
	return ids
}

// failingQueue fails every publish (an SQS outage).
type failingQueue struct{ dispatch.JobQueue }

func (failingQueue) Publish(context.Context, []dispatch.OutboundMessage) error {
	return errors.New("sqs unavailable")
}

func published(q *inmem.Queue, channel string, among []uuid.UUID) int {
	want := map[uuid.UUID]bool{}
	for _, id := range among {
		want[id] = true
	}
	n := 0
	for _, e := range q.Peek(channel) {
		if want[e.NotificationID] {
			n++
		}
	}
	return n
}

// TestRelayAfterAPICrash: rows committed without a fast-path publish (the
// API crashed after commit) are published by the relay, once.
func TestRelayAfterAPICrash(t *testing.T) {
	db := storetest.DB(t)
	st := entstore.New(db)
	tid := newTenant(t, st)
	ids := enqueue(t, st, tid, 7, "email")

	// While SQS is down nothing is marked published.
	down := scheduler.New(st, nil, failingQueue{}, "down", quiet, nil)
	_, err := down.RelayOnce(context.Background())
	require.Error(t, err)

	q := inmem.New()
	s := scheduler.New(st, nil, q, "a", quiet, nil)
	for {
		n, err := s.RelayOnce(context.Background())
		require.NoError(t, err)
		if n == 0 {
			break
		}
	}
	assert.Equal(t, 7, published(q, "email", ids), "every committed row is published once")
	envs := q.Peek("email")
	for _, e := range envs {
		if e.NotificationID == ids[0] {
			assert.Equal(t, tid.UUID(), e.TenantID)
			assert.Equal(t, 1, e.Attempt)
			assert.NotEmpty(t, e.TraceParent, "trace context travels in the envelope")
		}
	}
	n, err := s.RelayOnce(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, n, "nothing left")
}

// TestConcurrentRelaysPublishOnce: two relay tasks never publish the same
// row (FOR UPDATE SKIP LOCKED).
func TestConcurrentRelaysPublishOnce(t *testing.T) {
	db := storetest.DB(t)
	st := entstore.New(db)
	tid := newTenant(t, st)
	ids := enqueue(t, st, tid, 250, "sms")
	q := inmem.New()
	var wg sync.WaitGroup
	for i := range 3 {
		wg.Go(func() {
			s := scheduler.New(st, nil, q, string(rune('a'+i)), quiet, nil)
			for {
				n, err := s.RelayOnce(context.Background())
				assert.NoError(t, err)
				if n == 0 {
					return
				}
			}
		})
	}
	wg.Wait()
	assert.Equal(t, 250, published(q, "sms", ids), "each row exactly once across relays")
}

func TestMoveDueAndMaintain(t *testing.T) {
	db := storetest.DB(t)
	st := entstore.New(db)
	tid := newTenant(t, st)
	ctx := context.Background()
	now := time.Now().UTC()
	dueID, laterID := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	require.NoError(t, st.Scheduled().Add(ctx, tid, []store.ScheduledMessage{
		{NotificationID: dueID, RunID: uuid.Must(uuid.NewV7()), Channel: "email", DueAt: now.Add(-time.Second), Reason: "backoff", Attempt: 3},
		{NotificationID: laterID, RunID: uuid.Must(uuid.NewV7()), Channel: "email", DueAt: now.Add(time.Hour), Reason: "scheduled"},
	}, now))
	q := inmem.New()
	s := scheduler.New(st, db, q, "mover", quiet, nil)
	_, err := s.MoveDue(ctx)
	require.NoError(t, err)
	for {
		n, err := s.RelayOnce(ctx)
		require.NoError(t, err)
		if n == 0 {
			break
		}
	}
	var found bool
	for _, e := range q.Peek("email") {
		assert.NotEqual(t, laterID, e.NotificationID, "not due yet")
		if e.NotificationID == dueID {
			found = true
			assert.Equal(t, 3, e.Attempt, "the attempt survives the move")
		}
	}
	assert.True(t, found)

	// Maintenance: partitions 14 days ahead; old published rows and expired
	// idempotency records removed.
	require.NoError(t, st.Idempotency().Insert(ctx, tid, store.IdempotencyRecord{
		Key: "old-" + uuid.NewString(), RequestHash: []byte("h"), RunID: uuid.Must(uuid.NewV7()), ExpiresAt: now.Add(-time.Minute),
	}, now.Add(-25*time.Hour)))
	require.NoError(t, s.Maintain(ctx))
	cov, err := partition.New(db).Coverage(ctx, "notifications")
	require.NoError(t, err)
	assert.False(t, cov.Before(partition.Day(now).AddDate(0, 0, scheduler.PartitionsAhead)), "covered %s", cov)
	var expired int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM idempotency_records WHERE tenant_id = $1 AND expires_at <= now()`, tid.UUID()).Scan(&expired))
	assert.Equal(t, 0, expired)
}

// TestLeaderElection: one leader at a time; the stand-by takes over after
// the leader stops (released) or dies (lease expires).
func TestLeaderElection(t *testing.T) {
	st := entstore.New(storetest.DB(t))
	ctx := context.Background()
	clock := time.Now()
	var mu sync.Mutex
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	advance := func(d time.Duration) { mu.Lock(); clock = clock.Add(d); mu.Unlock() }

	require.NoError(t, st.Leases().Release(ctx, scheduler.LeaseName, "a"))
	require.NoError(t, st.Leases().Release(ctx, scheduler.LeaseName, "b"))
	a := scheduler.New(st, nil, inmem.New(), "a-"+uuid.NewString(), quiet, nil)
	b := scheduler.New(st, nil, inmem.New(), "b-"+uuid.NewString(), quiet, nil)
	a.SetClock(now)
	b.SetClock(now)
	// Clear any lease another test run left behind.
	advance(scheduler.LeaseTTL + time.Second)

	a.Step(ctx)
	b.Step(ctx)
	require.True(t, a.IsLeader() != b.IsLeader(), "exactly one leader")
	leader, standby := a, b
	if b.IsLeader() {
		leader, standby = b, a
	}
	advance(scheduler.RenewEvery)
	leader.Step(ctx)
	standby.Step(ctx)
	assert.True(t, leader.IsLeader(), "the leader renews")
	assert.False(t, standby.IsLeader())

	// Graceful stop releases the lease: the stand-by takes over at once.
	require.NoError(t, leader.Stop(ctx))
	standby.Step(ctx)
	assert.True(t, standby.IsLeader())

	// A dead leader (no renewals) loses the lease after the TTL.
	advance(scheduler.LeaseTTL + time.Second)
	leader.Step(ctx)
	assert.True(t, leader.IsLeader(), "takes over once the dead leader's lease expires")
	standby.Step(ctx)
	assert.False(t, standby.IsLeader(), "learns it lost the lease at its next renewal")
}

// TestRelayToSQS: committed outbox rows reach ElasticMQ as envelopes.
func TestRelayToSQS(t *testing.T) {
	db := storetest.DB(t)
	st := entstore.New(db)
	tid := newTenant(t, st)
	c := sqstest.Client(t)
	prefix := sqstest.Prefix()
	ctx := context.Background()
	require.NoError(t, sqsqueue.EnsureQueues(ctx, c, prefix, 30*time.Second, 5))
	q := sqsqueue.New(c, sqsqueue.Options{Prefix: prefix, FairQueues: true, Wait: time.Second})
	ids := enqueue(t, st, tid, 15, "webhook")
	s := scheduler.New(st, nil, q, "relay", quiet, nil)
	for {
		n, err := s.RelayOnce(ctx)
		require.NoError(t, err)
		if n == 0 {
			break
		}
	}
	var mu sync.Mutex
	got := map[uuid.UUID]bool{}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = q.Consume(cctx, "webhook", func(ctx context.Context, d dispatch.Delivery) {
			_ = d.Ack(ctx)
			mu.Lock()
			defer mu.Unlock()
			got[d.Envelope().NotificationID] = true
			if len(got) >= 15 {
				cancel()
			}
		})
	}()
	<-done
	mu.Lock()
	defer mu.Unlock()
	for _, id := range ids {
		assert.True(t, got[id], "relayed %s", id)
	}
}
