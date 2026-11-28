package entstore

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ticatwolves/notifex/internal/store"
	"github.com/ticatwolves/notifex/internal/store/storetest"
)

// v7At returns a UUIDv7 whose timestamp is t (random tail).
func v7At(t time.Time) uuid.UUID {
	id := uuid.Must(uuid.NewV7())
	ms := uint64(t.UnixMilli())
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], ms<<16)
	copy(id[:6], b[:6])
	return id
}

func (f fixture) newRun(t *testing.T, g graph) store.Run {
	t.Helper()
	ctx := context.Background()
	hash := sha256.Sum256([]byte(uuid.NewString()))
	require.NoError(t, f.s.Snapshots().Put(ctx, g.tenant.ID, hash[:], map[string]any{"workflow": "k"}, time.Now()))
	r, err := f.s.Runs().Create(ctx, g.tenant.ID, store.Run{
		ID: uuid.Must(uuid.NewV7()), ApplicationID: g.app.ID, EnvironmentID: g.env.ID, WorkflowID: uuid.Must(uuid.NewV7()),
		WorkflowKey: "order", SnapshotHash: hash[:], Payload: []byte("ciphertext"), Source: "rest", ActorType: "api_key", ActorID: "k",
	})
	require.NoError(t, err)
	return r
}

func newNotif(r store.Run, channel string) store.Notification {
	return store.Notification{
		ID: uuid.Must(uuid.NewV7()), RunID: r.ID, ApplicationID: r.ApplicationID, EnvironmentID: r.EnvironmentID,
		ActionID: uuid.Must(uuid.NewV7()), Channel: channel, Status: store.NotifQueued, Recipient: []byte("enc"),
		RecipientMasked: "a***@b***.test", CredentialMode: "platform", Provider: "platform", BillingBasis: "delivered",
	}
}

func TestRunsAndNotifications(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	g := f.graph(t)
	tid := g.tenant.ID
	r := f.newRun(t, g)

	created, err := uuidTime(r.ID)
	require.NoError(t, err)
	assert.Equal(t, created, r.CreatedAt.UTC(), "created_at is the UUIDv7 time")
	got, err := f.s.Runs().GetForEnv(ctx, tid, g.app.ID, g.env.ID, r.ID)
	require.NoError(t, err)
	assert.Equal(t, []byte("ciphertext"), got.Payload)
	_, err = f.s.Runs().GetForEnv(ctx, tid, g.app.ID, uuid.Must(uuid.NewV7()), r.ID)
	require.ErrorIs(t, err, store.ErrNotFound, "another environment")
	now := time.Now()
	require.NoError(t, f.s.Runs().SetStatus(ctx, tid, r.ID, store.RunCompleted, &now))

	_, err = f.s.Runs().Create(ctx, tid, store.Run{ID: uuid.New(), WorkflowKey: "k", SnapshotHash: []byte("h"), Source: "rest", ActorType: "a", ActorID: "b"})
	require.ErrorIs(t, err, store.ErrInvalid, "run ids must be UUIDv7")
	_, err = f.s.Runs().Create(ctx, tid, store.Run{ID: uuid.Must(uuid.NewV7()), ApplicationID: g.app.ID, EnvironmentID: g.env.ID,
		WorkflowKey: "k", SnapshotHash: []byte("no such snapshot"), Source: "rest", ActorType: "a", ActorID: "b"})
	require.ErrorIs(t, err, store.ErrNotFound, "snapshot must exist (composite FK)")

	ns := []store.Notification{newNotif(r, "email"), newNotif(r, "sms")}
	require.NoError(t, f.s.Notifications().CreateBatch(ctx, tid, ns))
	n, err := f.s.Notifications().Get(ctx, tid, ns[0].ID)
	require.NoError(t, err)
	assert.Equal(t, "email", n.Channel)
	assert.Equal(t, []byte("enc"), n.Recipient)
	list, err := f.s.Notifications().ListForRun(ctx, tid, r)
	require.NoError(t, err)
	assert.Len(t, list, 2)

	err = f.s.Notifications().CreateBatch(ctx, tid, []store.Notification{{ID: uuid.New(), RunID: r.ID, Channel: "email", Status: "queued"}})
	require.ErrorIs(t, err, store.ErrInvalid, "notification ids must be UUIDv7")

	// No partition 60 days out: the insert fails loudly instead of landing in
	// a DEFAULT partition.
	far := newNotif(r, "email")
	far.ID = v7At(time.Now().AddDate(0, 0, 60))
	require.Error(t, f.s.Notifications().CreateBatch(ctx, tid, []store.Notification{far}))

	// The row lives in today's partition.
	db := storetest.DB(t)
	var part string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT tableoid::regclass::text FROM notifications WHERE id = $1`, ns[0].ID).Scan(&part))
	assert.Equal(t, "notifications_p"+created.Format("20060102"), part)
}

func TestClaimSemantics(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	g := f.graph(t)
	tid := g.tenant.ID
	r := f.newRun(t, g)
	n := newNotif(r, "email")
	require.NoError(t, f.s.Notifications().CreateBatch(ctx, tid, []store.Notification{n}))
	ns := f.s.Notifications()
	now := time.Now().UTC().Truncate(time.Microsecond)

	got, ok, err := ns.Claim(ctx, tid, n.ID, now, time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, store.NotifSending, got.Status)
	assert.Equal(t, 1, got.Attempt)
	_, ok, err = ns.Claim(ctx, tid, n.ID, now, time.Minute)
	require.NoError(t, err)
	assert.False(t, ok, "a held lease can't be claimed again")

	extended, err := ns.ExtendLease(ctx, tid, n.ID, now.Add(30*time.Second), time.Minute)
	require.NoError(t, err)
	assert.True(t, extended)

	// After the lease expires a crashed worker's notification is reclaimed.
	got, ok, err = ns.Claim(ctx, tid, n.ID, now.Add(2*time.Minute), time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 2, got.Attempt)

	// Conditional transitions: only from the expected status.
	sent := store.NotifSent
	ok, err = ns.Update(ctx, tid, n.ID, []string{store.NotifQueued}, store.NotificationChange{Status: &sent}, now)
	require.NoError(t, err)
	assert.False(t, ok, "not queued any more")
	ok, err = ns.Update(ctx, tid, n.ID, []string{store.NotifSending}, store.NotificationChange{Status: &sent, ClearLease: true, VendorMessageID: ptr("m-1")}, now)
	require.NoError(t, err)
	assert.True(t, ok)
	_, ok, err = ns.Claim(ctx, tid, n.ID, now.Add(time.Hour), time.Minute)
	require.NoError(t, err)
	assert.False(t, ok, "a sent notification is never claimed")
	extended, err = ns.ExtendLease(ctx, tid, n.ID, now, time.Minute)
	require.NoError(t, err)
	assert.False(t, extended)
}

// TestClaimIsExclusive: concurrent claims of one notification (duplicate SQS
// deliveries) produce exactly one owner.
func TestClaimIsExclusive(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	g := f.graph(t)
	r := f.newRun(t, g)
	n := newNotif(r, "sms")
	require.NoError(t, f.s.Notifications().CreateBatch(ctx, g.tenant.ID, []store.Notification{n}))
	var won atomic.Int32
	var wg sync.WaitGroup
	now := time.Now()
	for range 10 {
		wg.Go(func() {
			_, ok, err := f.s.Notifications().Claim(ctx, g.tenant.ID, n.ID, now, time.Minute)
			assert.NoError(t, err)
			if ok {
				won.Add(1)
			}
		})
	}
	wg.Wait()
	assert.Equal(t, int32(1), won.Load())
}

func TestEventsAndSnapshots(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	g := f.graph(t)
	tid := g.tenant.ID
	r := f.newRun(t, g)
	n := newNotif(r, "email")
	require.NoError(t, f.s.Notifications().CreateBatch(ctx, tid, []store.Notification{n}))
	evs := []store.NotificationEvent{
		{ID: uuid.Must(uuid.NewV7()), NotificationID: n.ID, Type: "queued"},
		{ID: uuid.Must(uuid.NewV7()), NotificationID: n.ID, Type: "sending", Detail: map[string]any{"attempt": float64(1)}},
	}
	require.NoError(t, f.s.Events().Append(ctx, tid, evs))
	got, err := f.s.Events().List(ctx, tid, n.ID)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "queued", got[0].Type)
	assert.Equal(t, float64(1), got[1].Detail["attempt"])

	hash := sha256.Sum256([]byte("doc"))
	require.NoError(t, f.s.Snapshots().Put(ctx, tid, hash[:], map[string]any{"v": "1"}, time.Now()))
	require.NoError(t, f.s.Snapshots().Put(ctx, tid, hash[:], map[string]any{"v": "ignored"}, time.Now()), "idempotent")
	s, err := f.s.Snapshots().Get(ctx, tid, hash[:])
	require.NoError(t, err)
	assert.Equal(t, "1", s.Doc["v"], "first write wins; snapshots are immutable")
}

func TestOutboxAndScheduled(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	g := f.graph(t)
	tid := g.tenant.ID
	now := time.Now().UTC().Truncate(time.Microsecond)
	msg := func() store.OutboxMessage {
		return store.OutboxMessage{NotificationID: uuid.Must(uuid.NewV7()), RunID: uuid.Must(uuid.NewV7()), Channel: "email"}
	}

	_, err := f.s.Outbox().Enqueue(ctx, tid, []store.OutboxMessage{{NotificationID: uuid.New(), RunID: uuid.New(), Channel: "email", DelaySeconds: 901}}, now)
	require.ErrorIs(t, err, store.ErrInvalid, "SQS delay is at most 15 minutes")

	var msgs []store.OutboxMessage
	for range 6 {
		msgs = append(msgs, msg())
	}
	ids, err := f.s.Outbox().Enqueue(ctx, tid, msgs, now)
	require.NoError(t, err)
	require.Len(t, ids, 6)
	oldest, err := f.s.Outbox().OldestUnpublished(ctx)
	require.NoError(t, err)
	assert.False(t, oldest.IsZero())

	// Two relays claim disjoint sets (SKIP LOCKED).
	first := make(chan []int64, 1)
	hold := make(chan struct{})
	go func() {
		_ = f.s.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
			got, err := tx.Outbox().ClaimUnpublished(ctx, 3)
			require.NoError(t, err)
			ids := make([]int64, len(got))
			for i, m := range got {
				ids[i] = m.ID
			}
			first <- ids
			<-hold
			return nil
		})
	}()
	a := <-first
	var b []int64
	require.NoError(t, f.s.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		got, err := tx.Outbox().ClaimUnpublished(ctx, 100)
		for _, m := range got {
			b = append(b, m.ID)
		}
		return err
	}))
	close(hold)
	assert.Len(t, a, 3)
	for _, id := range a {
		assert.NotContains(t, b, id, "a row locked by one relay is skipped by the other")
	}

	require.NoError(t, f.s.Outbox().MarkPublished(ctx, ids, now))
	n, err := f.s.Outbox().DeletePublishedBefore(ctx, now.Add(time.Second), 100)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, 6)

	// Scheduled dispatch: only due rows are taken, once.
	nid := uuid.Must(uuid.NewV7())
	require.NoError(t, f.s.Scheduled().Add(ctx, tid, []store.ScheduledMessage{
		{NotificationID: nid, RunID: uuid.Must(uuid.NewV7()), Channel: "sms", DueAt: now.Add(-time.Second), Reason: "scheduled"},
		{NotificationID: uuid.Must(uuid.NewV7()), RunID: uuid.Must(uuid.NewV7()), Channel: "sms", DueAt: now.Add(time.Hour), Reason: "deferred"},
	}, now))
	due, err := f.s.Scheduled().TakeDue(ctx, now, 100)
	require.NoError(t, err)
	found := false
	for _, d := range due {
		found = found || d.NotificationID == nid
		assert.False(t, d.DueAt.After(now), "only due rows")
	}
	assert.True(t, found)
	again, err := f.s.Scheduled().TakeDue(ctx, now, 100)
	require.NoError(t, err)
	for _, d := range again {
		assert.NotEqual(t, nid, d.NotificationID, "taken once")
	}
}

func TestIdempotencyUsageAndLeases(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	g := f.graph(t)
	tid := g.tenant.ID
	now := time.Now().UTC()

	rec := store.IdempotencyRecord{Key: "key-" + uuid.NewString(), RequestHash: []byte("h"), RunID: uuid.Must(uuid.NewV7()), ExpiresAt: now.Add(24 * time.Hour)}
	require.NoError(t, f.s.Idempotency().Insert(ctx, tid, rec, now))
	require.ErrorIs(t, f.s.Idempotency().Insert(ctx, tid, rec, now), store.ErrConflict)
	got, err := f.s.Idempotency().Get(ctx, tid, rec.Key, now)
	require.NoError(t, err)
	assert.Equal(t, rec.RunID, got.RunID)
	_, err = f.s.Idempotency().Get(ctx, tid, rec.Key, now.Add(25*time.Hour))
	require.ErrorIs(t, err, store.ErrNotFound, "expired")

	// Usage: one record per notification, even under concurrency.
	nid := uuid.Must(uuid.NewV7())
	var inserted atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			ok, err := f.s.Usage().Record(ctx, tid, store.UsageRecord{NotificationID: nid, Channel: "sms", CredentialMode: "platform", BillingBasis: "sent", BillableAt: now})
			assert.NoError(t, err)
			if ok {
				inserted.Add(1)
			}
		})
	}
	wg.Wait()
	assert.Equal(t, int32(1), inserted.Load(), "never billed twice")
	count, err := f.s.Usage().Count(ctx, tid)
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	// Leases: one holder at a time; renewals; takeover after expiry.
	name := "relay-" + uuid.NewString()
	ok, err := f.s.Leases().Acquire(ctx, name, "a", now, 15*time.Second)
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = f.s.Leases().Acquire(ctx, name, "b", now.Add(5*time.Second), 15*time.Second)
	require.NoError(t, err)
	assert.False(t, ok, "held by a")
	ok, err = f.s.Leases().Acquire(ctx, name, "a", now.Add(10*time.Second), 15*time.Second)
	require.NoError(t, err)
	assert.True(t, ok, "a renews")
	ok, err = f.s.Leases().Acquire(ctx, name, "b", now.Add(26*time.Second), 15*time.Second)
	require.NoError(t, err)
	assert.True(t, ok, "b takes over after expiry")
	require.NoError(t, f.s.Leases().Release(ctx, name, "b"))
	ok, err = f.s.Leases().Acquire(ctx, name, "a", now.Add(27*time.Second), 15*time.Second)
	require.NoError(t, err)
	assert.True(t, ok, "free after release")
}

// TestSMSReservationIsExact: concurrent reservations never exceed the limit
// (ADR-005), per tenant and platform-wide.
func TestSMSReservationIsExact(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	g := f.graph(t)
	tid := g.tenant.ID
	d := time.Now()
	var ok atomic.Int32
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			got, err := f.s.SMSLimits().Reserve(ctx, tid, d, 1, 7)
			assert.NoError(t, err)
			if got {
				ok.Add(1)
			}
		})
	}
	wg.Wait()
	assert.Equal(t, int32(7), ok.Load())
	n, err := f.s.SMSLimits().Count(ctx, tid, d)
	require.NoError(t, err)
	assert.Equal(t, 7, n)

	require.NoError(t, f.s.SMSLimits().Release(ctx, tid, d, 2))
	got, err := f.s.SMSLimits().Reserve(ctx, tid, d, 2, 7)
	require.NoError(t, err)
	assert.True(t, got, "released capacity is reusable")
	got, err = f.s.SMSLimits().Reserve(ctx, tid, d, 1, 7)
	require.NoError(t, err)
	assert.False(t, got)
	require.NoError(t, f.s.SMSLimits().Release(ctx, tid, d, 100))
	n, err = f.s.SMSLimits().Count(ctx, tid, d)
	require.NoError(t, err)
	assert.Equal(t, 0, n, "never negative")

	// Platform-wide counter (a date far from other tests' days).
	pd := time.Date(2001, 1, int(time.Now().UnixNano()%28)+1, 0, 0, 0, 0, time.UTC)
	got, err = f.s.SMSLimits().ReservePlatform(ctx, pd, 3, 3)
	require.NoError(t, err)
	assert.True(t, got)
	got, err = f.s.SMSLimits().ReservePlatform(ctx, pd, 1, 3)
	require.NoError(t, err)
	assert.False(t, got)
	require.NoError(t, f.s.SMSLimits().ReleasePlatform(ctx, pd, 3))
}
