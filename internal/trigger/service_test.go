package trigger_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
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
	"github.com/ticatwolves/notifex/internal/credential"
	"github.com/ticatwolves/notifex/internal/crypto"
	"github.com/ticatwolves/notifex/internal/mail"
	"github.com/ticatwolves/notifex/internal/store"
	"github.com/ticatwolves/notifex/internal/store/entstore"
	"github.com/ticatwolves/notifex/internal/store/storetest"
	"github.com/ticatwolves/notifex/internal/tenant"
	"github.com/ticatwolves/notifex/internal/trigger"
	"github.com/ticatwolves/notifex/internal/workflow"
)

type env struct {
	st     *entstore.Store
	ten    *tenant.Service
	wf     *workflow.Service
	creds  *credential.Service
	cipher *crypto.Cipher
	svc    *trigger.Service
}

func newEnv(t *testing.T) env {
	t.Helper()
	mk := make([]byte, 32)
	_, _ = rand.Read(mk)
	kp, err := crypto.NewLocal(mk)
	require.NoError(t, err)
	st := entstore.New(storetest.DB(t))
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ts := tenant.NewService(st, kp, &mail.Recorder{}, log, "https://console.test", tenant.DefaultPolicy)
	c := crypto.NewCipher(kp, st.Tenants(), crypto.CipherOptions{})
	cs := credential.NewService(st, c)
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	svc := trigger.NewService(st, ts, cs, c, nil, trigger.Config{RecipientHashKey: key, PlatformSMSDailyCap: 1_000_000}, log, nil)
	return env{st: st, ten: ts, wf: workflow.NewService(st, ts, log), creds: cs, cipher: c, svc: svc}
}

// world is a tenant with an application, two environments, and an active
// workflow "order" with email and SMS actions (en templates).
type world struct {
	owner tenant.Actor
	app   store.Application
	prod  auth.Principal
	dev   auth.Principal
	wf    workflow.View
	sms   store.Action
	email store.Action
}

func sp(s string) *string { return &s }

func (e env) world(t *testing.T) world {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	u, err := e.st.Users().Create(ctx, store.NewUser{Email: "u" + strings.ReplaceAll(uuid.NewString()[:12], "-", "") + "@example.com", EmailVerifiedAt: &now})
	require.NoError(t, err)
	user := tenant.User{ID: u.ID, Email: u.Email, EmailVerified: true, ReauthenticatedAt: now}
	ten, err := e.ten.Create(ctx, user, "Acme", "t-"+strings.ReplaceAll(uuid.NewString()[:10], "-", ""))
	require.NoError(t, err)
	a, _, err := e.ten.Access(ctx, user, ten.ID)
	require.NoError(t, err)
	app, err := e.ten.CreateApplication(ctx, a, store.NewApplication{Name: "Shop", Key: "shop"})
	require.NoError(t, err)
	prod, err := e.ten.CreateEnvironment(ctx, a, app.ID, store.EnvProd)
	require.NoError(t, err)
	dev, err := e.ten.CreateEnvironment(ctx, a, app.ID, store.EnvDev)
	require.NoError(t, err)
	wf, err := e.wf.Create(ctx, a, app.ID, workflow.CreateInput{Key: "order", Name: "Order",
		PayloadSchema: map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}}})
	require.NoError(t, err)
	email, err := e.wf.CreateAction(ctx, a, app.ID, wf.ID, "email", true)
	require.NoError(t, err)
	_, err = e.wf.PutTemplate(ctx, a, app.ID, wf.ID, email.ID, "en", workflow.Content{Subject: sp("Order {{payload.id}}"), Text: sp("Hi")})
	require.NoError(t, err)
	sms, err := e.wf.CreateAction(ctx, a, app.ID, wf.ID, "sms", true)
	require.NoError(t, err)
	_, err = e.wf.PutTemplate(ctx, a, app.ID, wf.ID, sms.ID, "en", workflow.Content{Text: sp("Order {{payload.id}}")})
	require.NoError(t, err)
	active := store.WorkflowActive
	view, err := e.wf.Update(ctx, a, app.ID, wf.ID, workflow.Patch{Status: &active})
	require.NoError(t, err)
	// New tenants have no platform SMS until an operator sets a limit.
	limit := 1000
	_, err = e.st.Tenants().Update(ctx, ten.ID, store.TenantPatch{PlatformSMSDailyLimit: &limit})
	require.NoError(t, err)
	p := func(env store.Environment) auth.Principal {
		return auth.Principal{TenantID: ten.ID, ApplicationID: app.ID, EnvironmentID: env.ID, Environment: env.Name, APIKeyID: uuid.Must(uuid.NewV7())}
	}
	return world{owner: a, app: app, prod: p(prod), dev: p(dev), wf: view, sms: sms, email: email}
}

func recipients(n int) []trigger.Recipient {
	out := make([]trigger.Recipient, n)
	for i := range out {
		out[i] = trigger.Recipient{ID: fmt.Sprintf("r%d", i), Email: fmt.Sprintf("user%d@example.com", i), Phone: fmt.Sprintf("+9198765432%02d", i)}
	}
	return out
}

func byStatus(outs []trigger.Outcome) map[string]int {
	m := map[string]int{}
	for _, o := range outs {
		m[o.Status+"/"+o.ErrorCode]++
	}
	return m
}

func TestAcceptFanOutAndStorage(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	w := e.world(t)

	res, err := e.svc.Accept(ctx, w.prod, trigger.Request{Workflow: "order", Recipients: recipients(2), Payload: map[string]any{"id": "A1"}, Source: "rest"})
	require.NoError(t, err)
	assert.Equal(t, store.RunAccepted, res.Run.Status)
	require.Len(t, res.Notifications, 4, "2 actions × 2 recipients")
	assert.Equal(t, map[string]int{"queued/": 4}, byStatus(res.Notifications))

	// Payload and recipients are encrypted, bound to their rows.
	run, err := e.st.Runs().Get(ctx, w.prod.TenantID, res.Run.ID)
	require.NoError(t, err)
	assert.NotContains(t, string(run.Payload), "A1")
	pt, err := e.cipher.Decrypt(ctx, crypto.AAD{TenantID: w.prod.TenantID.UUID(), RowID: run.ID, Column: "payload"}, run.Payload)
	require.NoError(t, err)
	assert.JSONEq(t, `{"id":"A1"}`, string(pt))
	n, err := e.st.Notifications().Get(ctx, w.prod.TenantID, res.Notifications[0].ID)
	require.NoError(t, err)
	assert.NotContains(t, string(n.Recipient), "example.com")
	rec, err := e.cipher.Decrypt(ctx, crypto.AAD{TenantID: w.prod.TenantID.UUID(), RowID: n.ID, Column: "recipient"}, n.Recipient)
	require.NoError(t, err)
	var r trigger.Recipient
	require.NoError(t, json.Unmarshal(rec, &r))
	assert.Equal(t, "r0", r.ID)
	assert.Contains(t, n.RecipientMasked, "***")
	assert.Len(t, n.RecipientHash, 32)
	assert.Equal(t, "en", n.Locale)
	assert.Equal(t, "platform", n.CredentialMode, "no credentials: platform default")

	// Platform SMS bills on sent, email on delivered (ADR-007).
	ns, err := e.st.Notifications().ListForRun(ctx, w.prod.TenantID, run)
	require.NoError(t, err)
	for _, n := range ns {
		want := map[string]string{"sms": "sent", "email": "delivered"}[n.Channel]
		assert.Equal(t, want, n.BillingBasis, n.Channel)
	}

	// One outbox row per queued notification; one accepted event each.
	db := storetest.DB(t)
	var outbox, events int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM dispatch_outbox WHERE run_id = $1`, run.ID).Scan(&outbox))
	assert.Equal(t, 4, outbox)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM notification_events WHERE tenant_id = $1 AND type = 'accepted'`, w.prod.TenantID.UUID()).Scan(&events))
	assert.Equal(t, 4, events)

	// The snapshot is stored once and reused for identical config.
	res2, err := e.svc.Accept(ctx, w.prod, trigger.Request{Workflow: "order", Recipients: recipients(1), Payload: map[string]any{"id": "B2"}})
	require.NoError(t, err)
	run2, err := e.st.Runs().Get(ctx, w.prod.TenantID, res2.Run.ID)
	require.NoError(t, err)
	assert.Equal(t, run.SnapshotHash, run2.SnapshotHash, "same config, same snapshot")
	var snaps int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM config_snapshots WHERE tenant_id = $1`, w.prod.TenantID.UUID()).Scan(&snaps))
	assert.Equal(t, 1, snaps)
	snap, err := e.st.Snapshots().Get(ctx, w.prod.TenantID, run.SnapshotHash)
	require.NoError(t, err)
	assert.Equal(t, "order", snap.Doc["workflow"].(map[string]any)["key"])

	// Editing a template changes the snapshot; in-flight runs keep theirs.
	_, err = e.wf.PutTemplate(ctx, w.owner, w.app.ID, w.wf.ID, w.sms.ID, "en", workflow.Content{Text: sp("Changed {{payload.id}}")})
	require.NoError(t, err)
	res3, err := e.svc.Accept(ctx, w.prod, trigger.Request{Workflow: "order", Recipients: recipients(1)})
	require.NoError(t, err)
	run3, err := e.st.Runs().Get(ctx, w.prod.TenantID, res3.Run.ID)
	require.NoError(t, err)
	assert.NotEqual(t, run.SnapshotHash, run3.SnapshotHash)
}

func TestPerNotificationRules(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	w := e.world(t)

	// No phone → SMS recipient_missing; email still accepted.
	res, err := e.svc.Accept(ctx, w.prod, trigger.Request{Workflow: "order", Recipients: []trigger.Recipient{{ID: "x", Email: "a@example.com"}}})
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"queued/": 1, "failed/recipient_missing": 1}, byStatus(res.Notifications))

	// Locale chain: hi-IN → hi template; fr → brand default (en).
	_, err = e.wf.PutTemplate(ctx, w.owner, w.app.ID, w.wf.ID, w.email.ID, "hi", workflow.Content{Subject: sp("आदेश"), Text: sp("नमस्ते")})
	require.NoError(t, err)
	res, err = e.svc.Accept(ctx, w.prod, trigger.Request{Workflow: "order", Channels: []string{"email"},
		Recipients: []trigger.Recipient{{Email: "a@example.com", Locale: "hi-IN"}, {Email: "b@example.com", Locale: "fr"}}})
	require.NoError(t, err)
	locales := []string{}
	for _, o := range res.Notifications {
		n, err := e.st.Notifications().Get(ctx, w.prod.TenantID, o.ID)
		require.NoError(t, err)
		locales = append(locales, n.Locale)
	}
	assert.ElementsMatch(t, []string{"hi", "en"}, locales)

	// Brand default without a template → template_missing.
	_, err = e.ten.PutBrand(ctx, w.owner, tenant.Brand{DefaultLocale: sp("de")})
	require.NoError(t, err)
	res, err = e.svc.Accept(ctx, w.prod, trigger.Request{Workflow: "order", Channels: []string{"sms"}, Recipients: []trigger.Recipient{{Phone: "+919876543210", Locale: "fr"}}})
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"failed/template_missing": 1}, byStatus(res.Notifications))
	assert.Equal(t, store.RunFailed, res.Run.Status, "nothing accepted: still 202, run failed")
	_, err = e.ten.PutBrand(ctx, w.owner, tenant.Brand{})
	require.NoError(t, err)

	// Policy allows email only → SMS channel_not_allowed.
	_, err = e.ten.PutPolicy(ctx, w.owner, tenant.SendingPolicy{AllowedChannels: []string{"email"}})
	require.NoError(t, err)
	res, err = e.svc.Accept(ctx, w.prod, trigger.Request{Workflow: "order", Recipients: recipients(1)})
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"queued/": 1, "failed/channel_not_allowed": 1}, byStatus(res.Notifications))
	_, err = e.ten.PutPolicy(ctx, w.owner, tenant.SendingPolicy{})
	require.NoError(t, err)

	// Slack has no platform account → credential_unavailable.
	slack, err := e.wf.CreateAction(ctx, w.owner, w.app.ID, w.wf.ID, "slack", false)
	require.NoError(t, err)
	_, err = e.wf.PutTemplate(ctx, w.owner, w.app.ID, w.wf.ID, slack.ID, "en", workflow.Content{Text: sp("New order")})
	require.NoError(t, err)
	_, err = e.wf.SetActionEnabled(ctx, w.owner, w.app.ID, w.wf.ID, slack.ID, true)
	require.NoError(t, err)
	res, err = e.svc.Accept(ctx, w.prod, trigger.Request{Workflow: "order", Channels: []string{"slack"}, Recipients: recipients(1)})
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"failed/credential_unavailable": 1}, byStatus(res.Notifications))

	// Platform SMS daily limit: per notification, with retry_after.
	limit := 1
	_, err = e.st.Tenants().Update(ctx, w.prod.TenantID, store.TenantPatch{PlatformSMSDailyLimit: &limit})
	require.NoError(t, err)
	res, err = e.svc.Accept(ctx, w.prod, trigger.Request{Workflow: "order", Channels: []string{"sms"}, Recipients: recipients(3)})
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"queued/": 1, "failed/platform_sms_daily_limit": 2}, byStatus(res.Notifications))
	for _, o := range res.Notifications {
		if o.ErrorCode == trigger.CodePlatformSMSLimit {
			require.NotNil(t, o.RetryAfter)
			assert.True(t, o.RetryAfter.After(time.Now()))
		}
	}
}

func TestValidationAndScheduling(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	w := e.world(t)

	_, err := e.svc.Accept(ctx, w.prod, trigger.Request{Workflow: "nope", Recipients: recipients(1)})
	require.ErrorIs(t, err, trigger.ErrWorkflowNotFound)
	draft, err := e.wf.Create(ctx, w.owner, w.app.ID, workflow.CreateInput{Key: "draft", Name: "Draft"})
	require.NoError(t, err)
	_, err = e.svc.Accept(ctx, w.prod, trigger.Request{Workflow: draft.Key, Recipients: recipients(1)})
	require.ErrorIs(t, err, trigger.ErrWorkflowNotActive)
	_, err = e.svc.Accept(ctx, w.prod, trigger.Request{Workflow: "order", Channels: []string{"push"}, Recipients: recipients(1)})
	require.ErrorIs(t, err, store.ErrInvalid, "channels can only narrow")
	far := time.Now().Add(8 * 24 * time.Hour)
	_, err = e.svc.Accept(ctx, w.prod, trigger.Request{Workflow: "order", Recipients: recipients(1), ScheduleAt: &far})
	require.ErrorIs(t, err, trigger.ErrScheduleHorizon)
	_, err = e.svc.Accept(ctx, w.prod, trigger.Request{Workflow: "order", Recipients: recipients(1), Payload: map[string]any{"id": 5}})
	require.ErrorIs(t, err, store.ErrInvalid, "payload schema")
	_, err = e.svc.Accept(ctx, w.prod, trigger.Request{Workflow: "order", Recipients: recipients(trigger.MaxRecipients + 1)})
	require.ErrorIs(t, err, store.ErrInvalid)
	_, err = e.svc.Accept(ctx, w.prod, trigger.Request{Workflow: "order"})
	require.ErrorIs(t, err, store.ErrInvalid, "recipients required")

	db := storetest.DB(t)
	// Within 15 minutes: outbox with a delay.
	soon := time.Now().Add(5 * time.Minute)
	res, err := e.svc.Accept(ctx, w.prod, trigger.Request{Workflow: "order", Channels: []string{"email"}, Recipients: recipients(1), ScheduleAt: &soon})
	require.NoError(t, err)
	var delay int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT delay_s FROM dispatch_outbox WHERE run_id = $1`, res.Run.ID).Scan(&delay))
	assert.InDelta(t, 300, delay, 5)
	// Later: deferred with a scheduled_dispatch row, no outbox row.
	later := time.Now().Add(2 * time.Hour)
	res, err = e.svc.Accept(ctx, w.prod, trigger.Request{Workflow: "order", Channels: []string{"email"}, Recipients: recipients(1), ScheduleAt: &later})
	require.NoError(t, err)
	assert.Equal(t, store.NotifDeferred, res.Notifications[0].Status)
	var sched, out int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM scheduled_dispatch WHERE run_id = $1 AND reason = 'scheduled'`, res.Run.ID).Scan(&sched))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM dispatch_outbox WHERE run_id = $1`, res.Run.ID).Scan(&out))
	assert.Equal(t, 1, sched)
	assert.Equal(t, 0, out)
	// In the past: now.
	past := time.Now().Add(-time.Hour)
	res, err = e.svc.Accept(ctx, w.prod, trigger.Request{Workflow: "order", Channels: []string{"email"}, Recipients: recipients(1), ScheduleAt: &past})
	require.NoError(t, err)
	assert.Nil(t, res.Run.ScheduleAt)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT delay_s FROM dispatch_outbox WHERE run_id = $1`, res.Run.ID).Scan(&delay))
	assert.Equal(t, 0, delay)
}

func TestIdempotency(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	w := e.world(t)
	req := trigger.Request{Workflow: "order", Recipients: recipients(1), Payload: map[string]any{"id": "A1"}, IdempotencyKey: "order-A1-" + uuid.NewString()}

	first, err := e.svc.Accept(ctx, w.prod, req)
	require.NoError(t, err)
	again, err := e.svc.Accept(ctx, w.prod, req)
	require.NoError(t, err)
	assert.True(t, again.Replayed)
	assert.Equal(t, first.Run.ID, again.Run.ID)
	assert.Len(t, again.Notifications, len(first.Notifications))

	changed := req
	changed.Payload = map[string]any{"id": "A2"}
	_, err = e.svc.Accept(ctx, w.prod, changed)
	require.ErrorIs(t, err, trigger.ErrIdempotencyConflict)
	_, err = e.svc.Accept(ctx, w.dev, req)
	require.ErrorIs(t, err, trigger.ErrIdempotencyConflict, "the key belongs to another environment's run")

	// Concurrent duplicates: one run, everyone gets it.
	race := req
	race.IdempotencyKey = "race-" + uuid.NewString()
	ids := make(chan uuid.UUID, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			r, err := e.svc.Accept(ctx, w.prod, race)
			if assert.NoError(t, err) {
				ids <- r.Run.ID
			}
		})
	}
	wg.Wait()
	close(ids)
	seen := map[uuid.UUID]bool{}
	for id := range ids {
		seen[id] = true
	}
	assert.Len(t, seen, 1)
	var runs int
	require.NoError(t, storetest.DB(t).QueryRowContext(ctx, `SELECT count(*) FROM runs WHERE idempotency_key = $1`, race.IdempotencyKey).Scan(&runs))
	assert.Equal(t, 1, runs)
}

// TestPlatformSMSLimitUnderConcurrency: concurrent triggers never accept
// more platform SMS than the tenant's daily limit.
func TestPlatformSMSLimitUnderConcurrency(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	w := e.world(t)
	limit := 5
	_, err := e.st.Tenants().Update(ctx, w.prod.TenantID, store.TenantPatch{PlatformSMSDailyLimit: &limit})
	require.NoError(t, err)
	var mu sync.Mutex
	counts := map[string]int{}
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Go(func() {
			res, err := e.svc.Accept(ctx, w.prod, trigger.Request{Workflow: "order", Channels: []string{"sms"},
				Recipients: []trigger.Recipient{{Phone: fmt.Sprintf("+9198765400%02d", i)}}})
			if assert.NoError(t, err) {
				mu.Lock()
				counts[res.Notifications[0].Status+"/"+res.Notifications[0].ErrorCode]++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	assert.Equal(t, map[string]int{"queued/": 5, "failed/platform_sms_daily_limit": 7}, counts)
	n, err := e.st.SMSLimits().Count(ctx, w.prod.TenantID, time.Now())
	require.NoError(t, err)
	assert.Equal(t, 5, n)
}

func TestReadsAreScoped(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	w := e.world(t)
	res, err := e.svc.Accept(ctx, w.prod, trigger.Request{Workflow: "order", Recipients: recipients(1)})
	require.NoError(t, err)

	v, err := e.svc.GetRun(ctx, w.prod, res.Run.ID)
	require.NoError(t, err)
	assert.Len(t, v.Notifications, 2)
	nv, err := e.svc.GetNotification(ctx, w.prod, res.Notifications[0].ID)
	require.NoError(t, err)
	assert.Nil(t, nv.Notification.Recipient, "views never carry ciphertext")
	require.Len(t, nv.Events, 1)
	assert.Equal(t, "accepted", nv.Events[0].Type)

	other := e.world(t)
	for name, p := range map[string]auth.Principal{"other environment": w.dev, "other tenant": other.prod} {
		_, err = e.svc.GetRun(ctx, p, res.Run.ID)
		require.ErrorIs(t, err, store.ErrNotFound, name)
		_, err = e.svc.GetNotification(ctx, p, res.Notifications[0].ID)
		require.ErrorIs(t, err, store.ErrNotFound, name)
	}
	_, err = e.svc.Accept(ctx, other.prod, trigger.Request{Workflow: "order", Recipients: recipients(1), IdempotencyKey: "cross-" + uuid.NewString()})
	require.NoError(t, err, "same workflow key in another tenant is that tenant's own workflow")
}
