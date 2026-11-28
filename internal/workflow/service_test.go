package workflow_test

import (
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ticatwolves/notifex/internal/crypto"
	"github.com/ticatwolves/notifex/internal/mail"
	"github.com/ticatwolves/notifex/internal/store"
	"github.com/ticatwolves/notifex/internal/store/entstore"
	"github.com/ticatwolves/notifex/internal/store/storetest"
	"github.com/ticatwolves/notifex/internal/tenant"
	"github.com/ticatwolves/notifex/internal/workflow"
	"github.com/ticatwolves/notifex/internal/workflow/render"
)

type env struct {
	st  *entstore.Store
	ten *tenant.Service
	svc *workflow.Service
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
	return env{st: st, ten: ts, svc: workflow.NewService(st, ts, log)}
}

// owner creates a tenant with an application and returns the owner's actor.
func (e env) owner(t *testing.T) (tenant.Actor, store.Application) {
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
	return a, app
}

func (e env) member(t *testing.T, a tenant.Actor, role store.Role) tenant.Actor {
	t.Helper()
	ctx := context.Background()
	u, err := e.st.Users().Create(ctx, store.NewUser{Email: "m" + strings.ReplaceAll(uuid.NewString()[:12], "-", "") + "@example.com"})
	require.NoError(t, err)
	_, err = e.st.Memberships().Create(ctx, a.TenantID, u.ID, role)
	require.NoError(t, err)
	m, _, err := e.ten.Access(ctx, tenant.User{ID: u.ID, Email: u.Email}, a.TenantID)
	require.NoError(t, err)
	return m
}

func sp(s string) *string { return &s }

func emailContent() workflow.Content {
	return workflow.Content{Subject: sp("Order {{payload.id}} shipped"), HTML: sp("<p>Hi {{recipient.name}}</p>")}
}

func TestWorkflowLifecycle(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	a, app := e.owner(t)

	_, err := e.svc.Create(ctx, a, app.ID, workflow.CreateInput{Key: "Order Shipped", Name: "x"})
	require.ErrorIs(t, err, store.ErrInvalid)
	_, err = e.svc.Create(ctx, a, app.ID, workflow.CreateInput{Key: "k", Name: "x", PayloadSchema: map[string]any{"$ref": "https://evil.test/s.json"}})
	require.ErrorIs(t, err, store.ErrInvalid, "remote $ref")
	assert.Contains(t, err.Error(), "not allowed")
	_, err = e.svc.Create(ctx, a, app.ID, workflow.CreateInput{Key: "k", Name: "x", PayloadSchema: map[string]any{"$ref": "file:///etc/passwd"}})
	require.ErrorIs(t, err, store.ErrInvalid, "file $ref")
	_, err = e.svc.Create(ctx, a, app.ID, workflow.CreateInput{Key: "k", Name: "x", PayloadSchema: map[string]any{"type": "nonsense"}})
	require.ErrorIs(t, err, store.ErrInvalid, "invalid schema")

	wf, err := e.svc.Create(ctx, a, app.ID, workflow.CreateInput{Key: "order_shipped", Name: "Order shipped",
		PayloadSchema: map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}, "required": []any{"id"}}})
	require.NoError(t, err)
	assert.Equal(t, store.WorkflowDraft, wf.Status)
	_, err = e.svc.Create(ctx, a, app.ID, workflow.CreateInput{Key: "order_shipped", Name: "dup"})
	require.ErrorIs(t, err, store.ErrConflict)

	_, err = e.svc.CreateAction(ctx, a, app.ID, wf.ID, "push", true)
	require.ErrorIs(t, err, store.ErrInvalid, "Phase 4 channel")
	email, err := e.svc.CreateAction(ctx, a, app.ID, wf.ID, "email", true)
	require.NoError(t, err)
	_, err = e.svc.CreateAction(ctx, a, app.ID, wf.ID, "email", true)
	require.ErrorIs(t, err, store.ErrConflict, "one action per channel")

	active := store.WorkflowActive
	_, err = e.svc.Update(ctx, a, app.ID, wf.ID, workflow.Patch{Status: &active})
	require.ErrorIs(t, err, store.ErrInvalid, "no template yet")

	// Templates: channel fields and syntax are checked.
	_, err = e.svc.PutTemplate(ctx, a, app.ID, wf.ID, email.ID, "en", workflow.Content{Subject: sp("s"), Blocks: []any{}})
	require.ErrorIs(t, err, store.ErrInvalid, "blocks on email")
	_, err = e.svc.PutTemplate(ctx, a, app.ID, wf.ID, email.ID, "en", workflow.Content{Subject: sp("s"), HTML: sp("{{> partial}}")})
	require.ErrorIs(t, err, render.ErrInvalid)
	_, err = e.svc.PutTemplate(ctx, a, app.ID, wf.ID, email.ID, "english", emailContent())
	require.ErrorIs(t, err, store.ErrInvalid, "bad locale")
	tpl, err := e.svc.PutTemplate(ctx, a, app.ID, wf.ID, email.ID, "en", emailContent())
	require.NoError(t, err)
	assert.Equal(t, "email", tpl.Channel)
	assert.Equal(t, "Order {{payload.id}} shipped", *tpl.Content.Subject)

	got, err := e.svc.Update(ctx, a, app.ID, wf.ID, workflow.Patch{Status: &active})
	require.NoError(t, err)
	assert.Equal(t, store.WorkflowActive, got.Status)
	assert.Equal(t, []string{"en"}, got.Actions[0].Locales)

	// Invariants of an active workflow.
	_, err = e.svc.SetActionEnabled(ctx, a, app.ID, wf.ID, email.ID, false)
	require.ErrorIs(t, err, store.ErrConflict, "last enabled action")
	require.ErrorIs(t, e.svc.DeleteAction(ctx, a, app.ID, wf.ID, email.ID), store.ErrConflict)
	require.ErrorIs(t, e.svc.DeleteTemplate(ctx, a, app.ID, wf.ID, email.ID, "en"), store.ErrConflict, "last template")
	_, err = e.svc.CreateAction(ctx, a, app.ID, wf.ID, "sms", true)
	require.ErrorIs(t, err, store.ErrConflict, "enabled action without template on an active workflow")
	sms, err := e.svc.CreateAction(ctx, a, app.ID, wf.ID, "sms", false)
	require.NoError(t, err)
	_, err = e.svc.SetActionEnabled(ctx, a, app.ID, wf.ID, sms.ID, true)
	require.ErrorIs(t, err, store.ErrConflict, "enable before template")
	_, err = e.svc.PutTemplate(ctx, a, app.ID, wf.ID, sms.ID, "en", workflow.Content{Text: sp("Order {{payload.id}} shipped")})
	require.NoError(t, err)
	_, err = e.svc.SetActionEnabled(ctx, a, app.ID, wf.ID, sms.ID, true)
	require.NoError(t, err)
	_, err = e.svc.SetActionEnabled(ctx, a, app.ID, wf.ID, email.ID, false)
	require.NoError(t, err, "another action is enabled now")

	// A second locale can be removed; templates list in locale order.
	_, err = e.svc.PutTemplate(ctx, a, app.ID, wf.ID, sms.ID, "hi", workflow.Content{Text: sp("ऑर्डर {{payload.id}}")})
	require.NoError(t, err)
	list, err := e.svc.ListTemplates(ctx, a, app.ID, wf.ID, sms.ID)
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, "en", list[0].Locale)
	require.NoError(t, e.svc.DeleteTemplate(ctx, a, app.ID, wf.ID, sms.ID, "hi"))
	require.ErrorIs(t, e.svc.DeleteTemplate(ctx, a, app.ID, wf.ID, sms.ID, "hi"), store.ErrNotFound)

	archived := store.WorkflowArchived
	_, err = e.svc.Update(ctx, a, app.ID, wf.ID, workflow.Patch{Status: &archived, ClearSchema: true, Name: sp(" Renamed ")})
	require.NoError(t, err)
	view, err := e.svc.Get(ctx, a, app.ID, wf.ID)
	require.NoError(t, err)
	assert.Equal(t, "Renamed", view.Name)
	assert.Nil(t, view.PayloadSchema)
	require.NoError(t, e.svc.DeleteAction(ctx, a, app.ID, wf.ID, sms.ID), "archived: no invariant")

	require.NoError(t, e.svc.Delete(ctx, a, app.ID, wf.ID))
	_, err = e.svc.Get(ctx, a, app.ID, wf.ID)
	require.ErrorIs(t, err, store.ErrNotFound)

	evs, err := e.st.Audit().List(ctx, a.TenantID, store.Page{Limit: store.MaxPageSize})
	require.NoError(t, err)
	seen := map[string]bool{}
	for _, ev := range evs {
		seen[ev.Action] = true
	}
	for _, act := range []string{"workflow.create", "workflow.update", "workflow.delete", "action.create", "action.update", "action.delete", "template.put", "template.delete"} {
		assert.True(t, seen[act], act)
	}
}

// TestLastEnabledActionUnderConcurrency: concurrent disables of every
// enabled action of an active workflow always leave exactly one enabled.
// Repeated rounds with four actions make the race likely without the lock.
func TestLastEnabledActionUnderConcurrency(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	a, app := e.owner(t)
	wf, err := e.svc.Create(ctx, a, app.ID, workflow.CreateInput{Key: "k", Name: "k"})
	require.NoError(t, err)
	contents := map[string]workflow.Content{
		"email": emailContent(), "sms": {Text: sp("x")}, "slack": {Text: sp("x")},
		"webhook": {Body: map[string]any{"a": "x"}},
	}
	var ids []uuid.UUID
	for _, ch := range []string{"email", "sms", "slack", "webhook"} {
		act, err := e.svc.CreateAction(ctx, a, app.ID, wf.ID, ch, true)
		require.NoError(t, err)
		_, err = e.svc.PutTemplate(ctx, a, app.ID, wf.ID, act.ID, "en", contents[ch])
		require.NoError(t, err)
		ids = append(ids, act.ID)
	}
	active := store.WorkflowActive
	_, err = e.svc.Update(ctx, a, app.ID, wf.ID, workflow.Patch{Status: &active})
	require.NoError(t, err)

	for round := range 10 {
		start := make(chan struct{})
		var ok atomic.Int32
		var wg sync.WaitGroup
		for _, id := range ids {
			wg.Go(func() {
				<-start
				_, err := e.svc.SetActionEnabled(ctx, a, app.ID, wf.ID, id, false)
				if err == nil {
					ok.Add(1)
				} else {
					assert.ErrorIs(t, err, store.ErrConflict)
				}
			})
		}
		close(start)
		wg.Wait()
		require.Equal(t, int32(len(ids)-1), ok.Load(), "round %d", round)
		view, err := e.svc.Get(ctx, a, app.ID, wf.ID)
		require.NoError(t, err)
		enabled := 0
		for _, act := range view.Actions {
			if act.Enabled {
				enabled++
			}
		}
		require.Equal(t, 1, enabled, "round %d: an active workflow keeps one enabled action", round)
		for _, id := range ids {
			_, err := e.svc.SetActionEnabled(ctx, a, app.ID, wf.ID, id, true)
			require.NoError(t, err)
		}
	}
}

func TestWorkflowPermissionsAndScope(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	a, app := e.owner(t)
	wf, err := e.svc.Create(ctx, a, app.ID, workflow.CreateInput{Key: "k", Name: "k"})
	require.NoError(t, err)
	act, err := e.svc.CreateAction(ctx, a, app.ID, wf.ID, "sms", true)
	require.NoError(t, err)

	viewer := e.member(t, a, store.RoleViewer)
	_, err = e.svc.Get(ctx, viewer, app.ID, wf.ID)
	require.NoError(t, err)
	_, err = e.svc.List(ctx, viewer, app.ID, nil, store.Page{})
	require.NoError(t, err)
	_, err = e.svc.Create(ctx, viewer, app.ID, workflow.CreateInput{Key: "v", Name: "v"})
	require.ErrorIs(t, err, tenant.ErrForbidden)
	_, err = e.svc.PutTemplate(ctx, viewer, app.ID, wf.ID, act.ID, "en", workflow.Content{Text: sp("x")})
	require.ErrorIs(t, err, tenant.ErrForbidden)
	_, err = e.svc.Preview(ctx, viewer, app.ID, workflow.PreviewInput{Channel: "sms", Template: workflow.Content{Text: sp("x")}})
	require.ErrorIs(t, err, tenant.ErrForbidden)

	dev := e.member(t, a, store.RoleDeveloper)
	_, err = e.svc.PutTemplate(ctx, dev, app.ID, wf.ID, act.ID, "en", workflow.Content{Text: sp("x")})
	require.NoError(t, err, "developers edit workflows")

	// Another application of the same tenant, another tenant, unknown ids.
	app2, err := e.ten.CreateApplication(ctx, a, store.NewApplication{Name: "Two", Key: "two"})
	require.NoError(t, err)
	_, err = e.svc.Get(ctx, a, app2.ID, wf.ID)
	require.ErrorIs(t, err, store.ErrNotFound, "workflow via another app")
	_, err = e.svc.GetTemplate(ctx, a, app2.ID, wf.ID, act.ID, "en")
	require.ErrorIs(t, err, store.ErrNotFound)
	wf2, err := e.svc.Create(ctx, a, app.ID, workflow.CreateInput{Key: "k2", Name: "k2"})
	require.NoError(t, err)
	_, err = e.svc.SetActionEnabled(ctx, a, app.ID, wf2.ID, act.ID, false)
	require.ErrorIs(t, err, store.ErrNotFound, "action via another workflow")
	_, err = e.svc.List(ctx, a, uuid.Must(uuid.NewV7()), nil, store.Page{})
	require.ErrorIs(t, err, store.ErrNotFound, "unknown app")

	b, appB := e.owner(t)
	_, err = e.svc.Get(ctx, b, app.ID, wf.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = e.svc.Get(ctx, b, appB.ID, wf.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = e.svc.PutTemplate(ctx, b, appB.ID, wf.ID, act.ID, "en", workflow.Content{Text: sp("pwned")})
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = e.svc.Preview(ctx, b, appB.ID, workflow.PreviewInput{Channel: "sms", Template: workflow.Content{Text: sp("x")}, WorkflowID: &wf.ID})
	require.ErrorIs(t, err, store.ErrNotFound, "preview can't read another tenant's workflow")
}

func TestPreview(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	a, app := e.owner(t)
	_, err := e.ten.PutBrand(ctx, a, tenant.Brand{DisplayName: sp("Acme"), FooterText: sp("Acme Ltd")})
	require.NoError(t, err)
	wf, err := e.svc.Create(ctx, a, app.ID, workflow.CreateInput{Key: "order", Name: "Order",
		PayloadSchema: map[string]any{"type": "object", "additionalProperties": false, "required": []any{"id"},
			"properties": map[string]any{"id": map[string]any{"type": "string"},
				"items": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"sku": map[string]any{"type": "string"}}}}}}})
	require.NoError(t, err)

	out, err := e.svc.Preview(ctx, a, app.ID, workflow.PreviewInput{
		Channel: "email", WorkflowID: &wf.ID,
		Template: workflow.Content{Subject: sp("{{workflow.name}} {{payload.id}}"), HTML: sp("<p>{{payload.id}} {{payload.missing}} {{payload.items.0.sku}} {{payload.items.0.nope}}</p>")},
		Payload:  map[string]any{"id": json1()},
	})
	require.NoError(t, err)
	assert.Equal(t, "Order 42", *out.Subject)
	assert.Contains(t, *out.HTML, "Acme Ltd", "effective brand layout")
	assert.Contains(t, *out.Text, "42")
	msgs := []string{}
	for _, w := range out.Warnings {
		msgs = append(msgs, w.Field+": "+w.Message)
	}
	joined := strings.Join(msgs, "\n")
	assert.Contains(t, joined, "payload: sample payload doesn't match", "id must be a string")
	assert.Contains(t, joined, "payload.missing is not defined")
	assert.Contains(t, joined, "payload.items.0.nope is not defined")
	assert.NotContains(t, joined, "payload.items.0.sku is not defined")

	sms, err := e.svc.Preview(ctx, a, app.ID, workflow.PreviewInput{Channel: "sms", Template: workflow.Content{Text: sp("नमस्ते {{payload.n}}")}, Payload: map[string]any{"n": "1"}})
	require.NoError(t, err)
	require.NotNil(t, sms.SMS)
	assert.Equal(t, render.UCS2, sms.SMS.Encoding)
	assert.Equal(t, 1, sms.SMS.Segments)

	wh, err := e.svc.Preview(ctx, a, app.ID, workflow.PreviewInput{Channel: "webhook", Template: workflow.Content{Body: map[string]any{"id": "{{payload.id}}"}}, Payload: map[string]any{"id": "A1"}})
	require.NoError(t, err)
	assert.JSONEq(t, `{"id":"A1"}`, string(wh.Body))

	sl, err := e.svc.Preview(ctx, a, app.ID, workflow.PreviewInput{Channel: "slack", Template: workflow.Content{Text: sp("Hi <{{payload.who}}>")}, Payload: map[string]any{"who": "@channel"}})
	require.NoError(t, err)
	assert.Equal(t, "Hi <@channel>", *sl.Text, "template markup is kept; only values are escaped")
	_, err = e.svc.Preview(ctx, a, app.ID, workflow.PreviewInput{Channel: "slack", Template: workflow.Content{Text: sp("{{#each}}")}})
	require.ErrorIs(t, err, render.ErrInvalid)
	_, err = e.svc.Preview(ctx, a, app.ID, workflow.PreviewInput{Channel: "sms", Template: workflow.Content{Text: sp("{{payload.big}}")}, Payload: map[string]any{"big": strings.Repeat("x", 500)}})
	require.ErrorIs(t, err, render.ErrInvalid, "over 3 segments")
	_, err = e.svc.Preview(ctx, a, app.ID, workflow.PreviewInput{Channel: "push", Template: workflow.Content{Text: sp("x")}})
	require.ErrorIs(t, err, store.ErrInvalid)
}

func json1() any { return 42 }
