package worker_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ticatwolves/notifex/internal/auth"
	"github.com/ticatwolves/notifex/internal/channel"
	"github.com/ticatwolves/notifex/internal/channel/vendor"
	"github.com/ticatwolves/notifex/internal/credential"
	"github.com/ticatwolves/notifex/internal/crypto"
	"github.com/ticatwolves/notifex/internal/dispatch"
	"github.com/ticatwolves/notifex/internal/dispatch/inmem"
	"github.com/ticatwolves/notifex/internal/dispatch/sqsqueue"
	"github.com/ticatwolves/notifex/internal/dispatch/sqsqueue/sqstest"
	"github.com/ticatwolves/notifex/internal/dispatch/worker"
	"github.com/ticatwolves/notifex/internal/mail"
	"github.com/ticatwolves/notifex/internal/store"
	"github.com/ticatwolves/notifex/internal/store/entstore"
	"github.com/ticatwolves/notifex/internal/store/storetest"
	"github.com/ticatwolves/notifex/internal/tenant"
	"github.com/ticatwolves/notifex/internal/trigger"
	"github.com/ticatwolves/notifex/internal/workflow"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// clock is a settable clock shared by a worker and the test.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

// h is one tenant's world plus a worker over an in-memory queue. Each test
// has its own tenant; nothing here is cross-tenant, so tests run in parallel.
type h struct {
	t      *testing.T
	st     *entstore.Store
	db     *sql.DB
	cipher *crypto.Cipher
	ten    *tenant.Service
	wf     *workflow.Service
	creds  *credential.Service
	svc    *trigger.Service
	q      *inmem.Queue
	mock   *vendor.MockVendorClient
	reg    *channel.Registry
	w      *worker.Worker
	clk    *clock

	owner tenant.Actor
	app   store.Application
	prod  auth.Principal
	wfID  uuid.UUID
}

func sp(s string) *string { return &s }

func newH(t *testing.T) *h {
	t.Helper()
	ctx := context.Background()
	mk := make([]byte, 32)
	_, _ = rand.Read(mk)
	kp, err := crypto.NewLocal(mk)
	require.NoError(t, err)
	db := storetest.DB(t)
	st := entstore.New(db)
	ts := tenant.NewService(st, kp, &mail.Recorder{}, quiet, "https://console.test", tenant.DefaultPolicy)
	c := crypto.NewCipher(kp, st.Tenants(), crypto.CipherOptions{})
	cs := credential.NewService(st, c)
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	q := inmem.New()
	svc := trigger.NewService(st, ts, cs, c, dispatch.OutboxPublisher{Q: q},
		trigger.Config{RecipientHashKey: key, PlatformSMSDailyCap: 1_000_000}, quiet, nil)
	x := &h{t: t, st: st, db: db, cipher: c, ten: ts, wf: workflow.NewService(st, ts, quiet), creds: cs, svc: svc, q: q,
		mock: vendor.NewMock(), reg: channel.NewRegistry(), clk: &clock{t: time.Now()}}
	x.mock.RegisterAll(x.reg)
	x.w = x.newWorker(x.reg, x.clk)

	now := time.Now()
	u, err := st.Users().Create(ctx, store.NewUser{Email: "u" + strings.ReplaceAll(uuid.NewString()[:12], "-", "") + "@example.com", EmailVerifiedAt: &now})
	require.NoError(t, err)
	user := tenant.User{ID: u.ID, Email: u.Email, EmailVerified: true, ReauthenticatedAt: now}
	te, err := ts.Create(ctx, user, "Acme", "t-"+strings.ReplaceAll(uuid.NewString()[:10], "-", ""))
	require.NoError(t, err)
	x.owner, _, err = ts.Access(ctx, user, te.ID)
	require.NoError(t, err)
	x.app, err = ts.CreateApplication(ctx, x.owner, store.NewApplication{Name: "Shop", Key: "shop"})
	require.NoError(t, err)
	env, err := ts.CreateEnvironment(ctx, x.owner, x.app.ID, store.EnvProd)
	require.NoError(t, err)
	x.prod = auth.Principal{TenantID: te.ID, ApplicationID: x.app.ID, EnvironmentID: env.ID, Environment: env.Name, APIKeyID: uuid.Must(uuid.NewV7())}
	wf, err := x.wf.Create(ctx, x.owner, x.app.ID, workflow.CreateInput{Key: "order", Name: "Order"})
	require.NoError(t, err)
	x.wfID = wf.ID
	limit := 1000
	_, err = st.Tenants().Update(ctx, te.ID, store.TenantPatch{PlatformSMSDailyLimit: &limit})
	require.NoError(t, err)
	return x
}

func (x *h) newWorker(reg *channel.Registry, clk *clock) *worker.Worker {
	w := worker.New(x.st, x.cipher, x.creds, reg, worker.DefaultConfig, quiet, nil)
	w.SetClock(clk.now, func() float64 { return 0.5 })
	return w
}

// action adds an active action with an "en" template.
func (x *h) action(ch string, c workflow.Content) {
	x.t.Helper()
	ctx := context.Background()
	a, err := x.wf.CreateAction(ctx, x.owner, x.app.ID, x.wfID, ch, true)
	require.NoError(x.t, err)
	_, err = x.wf.PutTemplate(ctx, x.owner, x.app.ID, x.wfID, a.ID, "en", c)
	require.NoError(x.t, err)
}

func (x *h) activate() {
	x.t.Helper()
	active := store.WorkflowActive
	_, err := x.wf.Update(context.Background(), x.owner, x.app.ID, x.wfID, workflow.Patch{Status: &active})
	require.NoError(x.t, err)
}

func (x *h) sms() { x.action("sms", workflow.Content{Text: sp("Order {{payload.id}}")}) }
func (x *h) email() {
	x.action("email", workflow.Content{Subject: sp("Order {{payload.id}}"), Text: sp("Hi")})
}

func (x *h) webhook() uuid.UUID {
	x.t.Helper()
	x.action("webhook", workflow.Content{Body: map[string]any{"order": "{{payload.id}}"}})
	c, _, err := x.creds.Create(context.Background(), x.owner, credential.CreateInput{Channel: "webhook", Provider: "signing", Name: "hooks",
		Config: map[string]any{"url": "https://hooks.acme.test/notifex"}})
	require.NoError(x.t, err)
	return c.ID
}

func (x *h) trigger(n int) []trigger.Outcome {
	x.t.Helper()
	rs := make([]trigger.Recipient, n)
	for i := range rs {
		rs[i] = trigger.Recipient{ID: fmt.Sprintf("r%d", i), Email: fmt.Sprintf("user%d@example.com", i), Phone: fmt.Sprintf("+9198765432%02d", i)}
	}
	res, err := x.svc.Accept(context.Background(), x.prod, trigger.Request{Workflow: "order", Recipients: rs, Payload: map[string]any{"id": "A1"}, Source: "rest"})
	require.NoError(x.t, err)
	return res.Notifications
}

func (x *h) only(outs []trigger.Outcome, ch string) trigger.Outcome {
	x.t.Helper()
	for _, o := range outs {
		if o.Channel == ch {
			return o
		}
	}
	x.t.Fatalf("no %s notification", ch)
	return trigger.Outcome{}
}

// drain handles every visible message on ch; it returns how many.
func (x *h) drain(w *worker.Worker, ch string) int {
	n := 0
	for d := x.q.ReceiveOne(ch); d != nil; d = x.q.ReceiveOne(ch) {
		w.Handle(context.Background(), d)
		n++
	}
	return n
}

// redeliver publishes an envelope for id (what the relay does with the
// outbox row a retry or deferral wrote).
func (x *h) redeliver(id uuid.UUID, ch string, attempt int) {
	x.t.Helper()
	n := x.get(id)
	require.NoError(x.t, x.q.Publish(context.Background(), []dispatch.OutboundMessage{{Envelope: dispatch.Envelope{
		V: dispatch.EnvelopeVersion, TenantID: x.prod.TenantID.UUID(), RunID: n.RunID, NotificationID: id, Channel: ch,
		Attempt: attempt, EnqueuedAt: time.Now().UTC(),
	}}}))
}

func (x *h) get(id uuid.UUID) store.Notification {
	x.t.Helper()
	n, err := x.st.Notifications().Get(context.Background(), x.prod.TenantID, id)
	require.NoError(x.t, err)
	return n
}

func (x *h) count(q string, args ...any) int {
	x.t.Helper()
	var n int
	require.NoError(x.t, x.db.QueryRowContext(context.Background(), q, args...).Scan(&n))
	return n
}

func (x *h) usage(id uuid.UUID) int {
	return x.count(`SELECT count(*) FROM usage_records WHERE notification_id = $1`, id)
}

func (x *h) smsUsed() int {
	return x.count(`SELECT COALESCE(sum(count), 0) FROM platform_sms_usage WHERE tenant_id = $1`, x.prod.TenantID.UUID())
}

func (x *h) events(id uuid.UUID, typ string) int {
	return x.count(`SELECT count(*) FROM notification_events WHERE tenant_id = $1 AND notification_id = $2 AND type = $3`,
		x.prod.TenantID.UUID(), id, typ)
}

func (x *h) policy(p tenant.SendingPolicy) {
	x.t.Helper()
	_, err := x.ten.PutPolicy(context.Background(), x.owner, p)
	require.NoError(x.t, err)
}

func TestHappyPathAndBilling(t *testing.T) {
	t.Parallel()
	x := newH(t)
	x.email()
	x.sms()
	x.webhook()
	x.activate()
	outs := x.trigger(1)
	require.Len(t, outs, 3)
	assert.Equal(t, 1, x.smsUsed(), "reserved at accept")

	for _, ch := range []string{"email", "sms", "webhook"} {
		assert.Equal(t, 1, x.drain(x.w, ch), ch)
		assert.Equal(t, 0, x.q.Len(ch), "%s acked", ch)
	}

	email, sms, hook := x.get(x.only(outs, "email").ID), x.get(x.only(outs, "sms").ID), x.get(x.only(outs, "webhook").ID)
	// ADR-007: email bills on delivered (no receipt yet: sent, unbilled);
	// platform SMS bills on sent; webhooks are delivered synchronously.
	for _, c := range []struct {
		n      store.Notification
		status string
		billed int
	}{{email, store.NotifSent, 0}, {sms, store.NotifSent, 1}, {hook, store.NotifDelivered, 1}} {
		assert.Equal(t, c.status, c.n.Status, c.n.Channel)
		assert.Equal(t, c.billed, x.usage(c.n.ID), "%s usage", c.n.Channel)
		assert.Equal(t, 1, c.n.Attempt, c.n.Channel)
		assert.Nil(t, c.n.LeaseUntil, "lease cleared")
		assert.NotEmpty(t, c.n.VendorMessageID)
		assert.Equal(t, 1, x.events(c.n.ID, c.status), c.n.Channel)
	}
	assert.NotNil(t, hook.TerminalAt, "delivered is terminal")
	assert.Nil(t, sms.TerminalAt, "sent awaits a receipt")
	assert.Equal(t, 1, x.smsUsed(), "a send that reached the vendor keeps its reservation")

	// What the vendor saw, rendered from the snapshot.
	byCh := map[string]vendor.Sent{}
	for _, s := range x.mock.Sent() {
		byCh[s.Message.Channel] = s
	}
	assert.Equal(t, "user0@example.com", byCh["email"].Message.To)
	assert.Equal(t, "Order A1", byCh["email"].Message.Email.Subject)
	assert.Equal(t, "+919876543200", byCh["sms"].Message.To)
	assert.Equal(t, "Order A1", byCh["sms"].Message.SMS.Text)
	assert.NotEmpty(t, byCh["webhook"].Credential.Secret, "BYO secret decrypted for the send")
	assert.Equal(t, "byo", byCh["webhook"].Credential.Mode)

	// Rendered content is stored encrypted, bound to the notification row.
	require.NotNil(t, sms.RenderedContent)
	assert.NotContains(t, string(sms.RenderedContent), "Order A1")
	pt, err := x.cipher.Decrypt(context.Background(), crypto.AAD{TenantID: x.prod.TenantID.UUID(), RowID: sms.ID, Column: "rendered_content"}, sms.RenderedContent)
	require.NoError(t, err)
	assert.Contains(t, string(pt), "Order A1")
}

// TestDuplicateDeliveryOneSend: at-least-once delivery; the claim makes the
// second copy harmless (acked, no send, no second usage record).
func TestDuplicateDeliveryOneSend(t *testing.T) {
	t.Parallel()
	x := newH(t)
	x.sms()
	x.activate()
	o := x.only(x.trigger(1), "sms")
	x.redeliver(o.ID, "sms", 1)
	assert.Equal(t, 2, x.drain(x.w, "sms"))
	assert.Equal(t, 1, x.mock.SendsFor(o.ID))
	assert.Equal(t, 0, x.q.Len("sms"), "both copies acked")
	assert.Equal(t, 1, x.usage(o.ID))
	assert.Equal(t, 1, x.get(o.ID).Attempt)
}

// TestCrashMidSend: a worker died holding the claim. While its lease is
// valid a redelivery is left unacked (no send); after the lease expires the
// next delivery reclaims and sends.
func TestCrashMidSend(t *testing.T) {
	t.Parallel()
	x := newH(t)
	x.sms()
	x.activate()
	ctx := context.Background()
	o := x.only(x.trigger(1), "sms")

	_, ok, err := x.st.Notifications().Claim(ctx, x.prod.TenantID, o.ID, time.Now(), time.Minute)
	require.NoError(t, err)
	require.True(t, ok, "the crashed worker's claim")
	assert.Equal(t, 1, x.drain(x.w, "sms"))
	assert.Equal(t, 0, x.mock.SendsFor(o.ID), "in flight elsewhere: no send")
	assert.Equal(t, 1, x.q.Len("sms"), "not acked: the queue redelivers it")

	// The lease runs out; the redelivery (after the visibility timeout) wins.
	x.clk.set(time.Now().Add(2 * time.Minute))
	x.q.SetClock(x.clk.now)
	assert.Equal(t, 1, x.drain(x.w, "sms"))
	n := x.get(o.ID)
	assert.Equal(t, store.NotifSent, n.Status)
	assert.Equal(t, 2, n.Attempt, "the crashed attempt counts")
	assert.Equal(t, 1, x.mock.SendsFor(o.ID))
	assert.Equal(t, 0, x.q.Len("sms"))
}

// TestLostClaim: worker A stalls in the vendor call past its lease; B
// reclaims and finishes. A's late result is discarded (conditional update)
// and its message acked: one status, one usage record, two vendor calls
// (the documented at-least-once cost).
func TestLostClaim(t *testing.T) {
	t.Parallel()
	x := newH(t)
	x.sms()
	x.activate()
	o := x.only(x.trigger(1), "sms")

	slow := vendor.NewMock()
	slow.Block = make(chan struct{})
	slowReg := channel.NewRegistry()
	slow.RegisterAll(slowReg)
	a := x.newWorker(slowReg, &clock{t: time.Now()})

	dA := x.q.ReceiveOne("sms")
	require.NotNil(t, dA)
	done := make(chan struct{})
	go func() { defer close(done); a.Handle(context.Background(), dA) }()
	require.Eventually(t, func() bool { return x.get(o.ID).Status == store.NotifSending }, 5*time.Second, 10*time.Millisecond)

	// B runs two minutes later (A's lease has expired) with a fast vendor.
	x.clk.set(time.Now().Add(2 * time.Minute))
	x.redeliver(o.ID, "sms", 1)
	assert.Equal(t, 1, x.drain(x.w, "sms"))
	require.Equal(t, store.NotifSent, x.get(o.ID).Status)

	close(slow.Block)
	<-done
	n := x.get(o.ID)
	assert.Equal(t, store.NotifSent, n.Status)
	assert.Equal(t, 2, n.Attempt)
	assert.Equal(t, 1, x.usage(o.ID), "billed once")
	assert.Equal(t, 1, x.events(o.ID, store.NotifSent), "A's outcome was not recorded")
	assert.Equal(t, 0, x.q.Len("sms"), "A acked its copy")
}

func TestRetryThenSuccess(t *testing.T) {
	t.Parallel()
	x := newH(t)
	x.sms()
	x.activate()
	x.mock.Outcome = func(m channel.Message, attempt int) (channel.Result, error) {
		if attempt == 1 {
			return channel.Result{}, &channel.SendError{Code: "vendor_unavailable", Message: "503", Retryable: true}
		}
		return channel.Result{VendorMessageID: "ok"}, nil
	}
	o := x.only(x.trigger(1), "sms")
	x.drain(x.w, "sms")

	n := x.get(o.ID)
	assert.Equal(t, store.NotifRetrying, n.Status)
	assert.Equal(t, "vendor_unavailable", n.LastErrorCode)
	require.NotNil(t, n.NextAttemptAt)
	// Full jitter at 0.5 of base 10 s = 5 s: an SQS delay through the outbox.
	var delay, attempt int
	require.NoError(t, x.db.QueryRowContext(context.Background(),
		`SELECT delay_s, attempt FROM dispatch_outbox WHERE notification_id = $1 AND published_at IS NULL`, o.ID).Scan(&delay, &attempt))
	assert.Equal(t, 5, delay)
	assert.Equal(t, 2, attempt)
	assert.Equal(t, 0, x.usage(o.ID))
	assert.Equal(t, 1, x.smsUsed(), "reached the vendor: reservation kept for the retry")

	x.redeliver(o.ID, "sms", 2)
	x.drain(x.w, "sms")
	n = x.get(o.ID)
	assert.Equal(t, store.NotifSent, n.Status)
	assert.Equal(t, 2, n.Attempt)
	assert.Empty(t, n.LastErrorCode, "cleared on success")
	assert.Equal(t, 1, x.usage(o.ID))
}

// TestLongBackoffGoesToScheduler: a delay over 15 minutes (Retry-After)
// becomes a scheduled_dispatch row, not an SQS delay.
func TestLongBackoffGoesToScheduler(t *testing.T) {
	t.Parallel()
	x := newH(t)
	x.sms()
	x.activate()
	x.mock.Outcome = func(channel.Message, int) (channel.Result, error) {
		return channel.Result{}, &channel.SendError{Code: "throttled", Message: "slow down", Retryable: true, RetryAfter: time.Hour}
	}
	o := x.only(x.trigger(1), "sms")
	x.drain(x.w, "sms")
	assert.Equal(t, store.NotifRetrying, x.get(o.ID).Status)
	assert.Equal(t, 1, x.count(`SELECT count(*) FROM scheduled_dispatch WHERE notification_id = $1 AND attempt = 2 AND reason = 'backoff' AND due_at > now() + interval '50 minutes'`, o.ID))
	assert.Equal(t, 0, x.count(`SELECT count(*) FROM dispatch_outbox WHERE notification_id = $1 AND published_at IS NULL`, o.ID))
}

func TestPermanentAndExhausted(t *testing.T) {
	t.Parallel()
	x := newH(t)
	x.sms()
	x.email()
	x.activate()
	two := 2
	x.policy(tenant.SendingPolicy{MaxAttempts: &two})
	x.mock.Outcome = func(m channel.Message, _ int) (channel.Result, error) {
		if m.Channel == "sms" {
			return channel.Result{}, &channel.SendError{Code: "invalid_recipient", Message: "bad number"}
		}
		return channel.Result{}, &channel.SendError{Code: "vendor_unavailable", Message: "503", Retryable: true}
	}
	outs := x.trigger(1)
	sms, email := x.only(outs, "sms"), x.only(outs, "email")

	x.drain(x.w, "sms")
	n := x.get(sms.ID)
	assert.Equal(t, store.NotifFailed, n.Status, "permanent: no retry")
	assert.Equal(t, "invalid_recipient", n.LastErrorCode)
	assert.NotNil(t, n.TerminalAt)
	assert.Equal(t, 0, x.count(`SELECT count(*) FROM dispatch_outbox WHERE notification_id = $1 AND published_at IS NULL`, sms.ID))
	assert.Equal(t, 1, x.smsUsed(), "it reached the vendor (which may charge): the reservation stands")
	assert.Equal(t, 0, x.usage(sms.ID))

	x.drain(x.w, "email")
	assert.Equal(t, store.NotifRetrying, x.get(email.ID).Status)
	x.redeliver(email.ID, "email", 2)
	x.drain(x.w, "email")
	n = x.get(email.ID)
	assert.Equal(t, store.NotifFailed, n.Status)
	assert.Equal(t, "max_attempts_exceeded", n.LastErrorCode, "policy max_attempts 2")
	assert.Equal(t, 2, x.mock.SendsFor(email.ID))
}

// TestFailBeforeVendorReleasesSMS: render failures and expiry never reach
// SNS, so the platform-SMS reservation is given back (ADR-005).
func TestFailBeforeVendorReleasesSMS(t *testing.T) {
	t.Parallel()
	x := newH(t)
	x.sms()
	x.activate()
	ctx := context.Background()
	outs := x.trigger(2)
	require.Equal(t, 2, x.smsUsed())

	// No template for the notification's locale: render fails.
	_, err := x.db.ExecContext(ctx, `UPDATE notifications SET locale = 'xx' WHERE id = $1`, outs[0].ID)
	require.NoError(t, err)
	d := x.q.ReceiveOne("sms")
	for d.Envelope().NotificationID != outs[0].ID {
		x.q.SetClock(func() time.Time { return time.Now().Add(time.Hour) }) // let the other one come back later
		d = x.q.ReceiveOne("sms")
	}
	x.w.Handle(ctx, d)
	n := x.get(outs[0].ID)
	assert.Equal(t, store.NotifFailed, n.Status)
	assert.Equal(t, "render_failed", n.LastErrorCode)
	assert.False(t, n.SMSReserved)
	assert.Equal(t, 1, x.smsUsed(), "released")

	// Expiry: still pending after 48 hours.
	x.clk.set(time.Now().Add(49 * time.Hour))
	x.redeliver(outs[1].ID, "sms", 1)
	x.drain(x.w, "sms")
	n = x.get(outs[1].ID)
	assert.Equal(t, store.NotifExpired, n.Status)
	assert.Equal(t, 0, x.smsUsed(), "released")
	assert.Equal(t, 0, x.mock.SendsFor(outs[1].ID))
}

func TestQuietHoursDefer(t *testing.T) {
	t.Parallel()
	x := newH(t)
	x.sms()
	x.activate()
	now := time.Now().UTC()
	x.policy(tenant.SendingPolicy{QuietHours: &tenant.QuietHours{
		Start: now.Add(-time.Hour).Format("15:04"), End: now.Add(2 * time.Hour).Format("15:04"), Timezone: "UTC",
	}})
	o := x.only(x.trigger(1), "sms")
	x.drain(x.w, "sms")
	n := x.get(o.ID)
	assert.Equal(t, store.NotifDeferred, n.Status)
	assert.Equal(t, 0, n.Attempt, "a deferral doesn't spend an attempt")
	require.NotNil(t, n.NextAttemptAt)
	assert.WithinDuration(t, now.Add(2*time.Hour), *n.NextAttemptAt, 2*time.Minute)
	assert.Equal(t, 0, x.mock.SendsFor(o.ID))
	assert.Equal(t, 1, x.events(o.ID, "deferred"))
	assert.Equal(t, 1, x.count(`SELECT count(*) FROM scheduled_dispatch WHERE notification_id = $1 AND reason = 'deferred' AND attempt = 1`, o.ID),
		"over 15 minutes: through the scheduler")
	assert.Equal(t, 0, x.q.Len("sms"))

	// After quiet hours it sends, as attempt 1.
	x.clk.set(now.Add(3 * time.Hour))
	x.redeliver(o.ID, "sms", 1)
	x.drain(x.w, "sms")
	n = x.get(o.ID)
	assert.Equal(t, store.NotifSent, n.Status)
	assert.Equal(t, 1, n.Attempt)
}

func TestRateLimitDefer(t *testing.T) {
	t.Parallel()
	x := newH(t)
	x.sms()
	x.activate()
	x.policy(tenant.SendingPolicy{RateLimitsPerMinute: map[string]int{"sms": 1}})
	outs := x.trigger(3)
	x.drain(x.w, "sms")
	st := map[string]int{}
	for _, o := range outs {
		n := x.get(o.ID)
		st[n.Status]++
		if n.Status == store.NotifDeferred {
			assert.Equal(t, 0, n.Attempt)
			var delay int
			require.NoError(t, x.db.QueryRowContext(context.Background(),
				`SELECT delay_s FROM dispatch_outbox WHERE notification_id = $1 AND published_at IS NULL`, o.ID).Scan(&delay))
			assert.Positive(t, delay, "short deferral: an SQS delay")
		}
	}
	assert.Equal(t, map[string]int{store.NotifSent: 1, store.NotifDeferred: 2}, st)
}

// TestCredentialGoneAfterAccept: the snapshot keeps the reference; a
// deleted or disabled credential fails the send permanently.
func TestCredentialGoneAfterAccept(t *testing.T) {
	t.Parallel()
	x := newH(t)
	id := x.webhook()
	x.activate()
	outs := x.trigger(2)
	require.NoError(t, x.creds.Delete(context.Background(), x.owner, id))
	x.drain(x.w, "webhook")
	for _, o := range outs {
		n := x.get(o.ID)
		assert.Equal(t, store.NotifFailed, n.Status)
		assert.Equal(t, "credential_unavailable", n.LastErrorCode)
	}
	assert.Empty(t, x.mock.Sent())
}

// TestForgedEnvelopeTenant: an envelope naming another tenant can't touch
// this tenant's notification (every read and claim is tenant-scoped).
func TestForgedEnvelopeTenant(t *testing.T) {
	t.Parallel()
	x := newH(t)
	other := newH(t)
	x.sms()
	x.activate()
	o := x.only(x.trigger(1), "sms")
	n := x.get(o.ID)
	require.NoError(t, x.q.Publish(context.Background(), []dispatch.OutboundMessage{{Envelope: dispatch.Envelope{
		V: dispatch.EnvelopeVersion, TenantID: other.prod.TenantID.UUID(), RunID: n.RunID, NotificationID: o.ID,
		Channel: "sms", Attempt: 1, EnqueuedAt: time.Now().UTC(),
	}}}))
	// Handle only the forged copy.
	var forged dispatch.Delivery
	for d := x.q.ReceiveOne("sms"); d != nil; d = x.q.ReceiveOne("sms") {
		if d.Envelope().TenantID == other.prod.TenantID.UUID() {
			forged = d
		}
	}
	require.NotNil(t, forged)
	x.w.Handle(context.Background(), forged)
	assert.Equal(t, 0, x.mock.SendsFor(o.ID))
	got := x.get(o.ID)
	assert.Equal(t, store.NotifQueued, got.Status, "untouched")
	assert.Equal(t, 0, got.Attempt)
	for _, e := range x.q.Peek("sms") {
		assert.NotEqual(t, other.prod.TenantID.UUID(), e.TenantID, "the forged message was acked (dropped)")
	}
}

func TestBackoffBounds(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		attempt int
		jitter  float64
		want    time.Duration
	}{
		{1, 0.5, 5 * time.Second},
		{1, 0, time.Second}, // floor
		{3, 0.99, time.Duration(0.99 * float64(40*time.Second))},
		{30, 0.5, 3 * time.Hour}, // capped at 6 h
	} {
		assert.Equal(t, c.want, worker.Backoff(worker.DefaultConfig, c.attempt, c.jitter), "attempt %d", c.attempt)
	}
}

// TestDispatcherOverElasticMQ: trigger → SQS (ElasticMQ) → dispatcher
// consumers → sent, through the real queue implementation.
func TestDispatcherOverElasticMQ(t *testing.T) {
	t.Parallel()
	x := newH(t)
	x.sms()
	x.email()
	x.activate()
	ctx := context.Background()
	c := sqstest.Client(t)
	prefix := sqstest.Prefix()
	require.NoError(t, sqsqueue.EnsureQueues(ctx, c, prefix, 30*time.Second, 5))
	q := sqsqueue.New(c, sqsqueue.Options{Prefix: prefix, FairQueues: true, Wait: time.Second, Concurrency: 4})

	key := make([]byte, 32)
	_, _ = rand.Read(key)
	svc := trigger.NewService(x.st, x.ten, x.creds, x.cipher, dispatch.OutboxPublisher{Q: q},
		trigger.Config{RecipientHashKey: key, PlatformSMSDailyCap: 1_000_000}, quiet, nil)
	rs := make([]trigger.Recipient, 5)
	for i := range rs {
		rs[i] = trigger.Recipient{Email: fmt.Sprintf("e%d@example.com", i), Phone: fmt.Sprintf("+9198765432%02d", i)}
	}
	res, err := svc.Accept(ctx, x.prod, trigger.Request{Workflow: "order", Recipients: rs, Payload: map[string]any{"id": "Q"}})
	require.NoError(t, err)
	require.Len(t, res.Notifications, 10)

	w := worker.New(x.st, x.cipher, x.creds, x.reg, worker.DefaultConfig, quiet, nil)
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- worker.NewDispatcher(q, w, []string{"email", "sms"}).Start(cctx) }()
	defer func() { cancel(); require.NoError(t, <-done) }()

	require.Eventually(t, func() bool {
		for _, o := range res.Notifications {
			if x.get(o.ID).Status != store.NotifSent {
				return false
			}
		}
		return true
	}, 30*time.Second, 100*time.Millisecond)
	for _, o := range res.Notifications {
		assert.Equal(t, 1, x.mock.SendsFor(o.ID))
	}
	for _, sent := range x.mock.Sent() {
		assert.Equal(t, x.prod.TenantID.UUID(), sent.Message.TenantID)
	}
}
