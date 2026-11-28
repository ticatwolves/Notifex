// Package devseed provisions a local development tenant end to end: tenant
// (with DEK), owner user, application, environment, an active
// "order_shipped" workflow (email + SMS, platform senders) for trigger
// smoke and load tests, and a fresh API key. It is idempotent apart from
// the key, which is new on every run. Local only.
package devseed

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/ticatwolves/notifex/internal/audit"
	"github.com/ticatwolves/notifex/internal/auth"
	"github.com/ticatwolves/notifex/internal/crypto"
	"github.com/ticatwolves/notifex/internal/mail"
	"github.com/ticatwolves/notifex/internal/store"
	"github.com/ticatwolves/notifex/internal/tenant"
	"github.com/ticatwolves/notifex/internal/workflow"
)

// Options configures Run.
type Options struct {
	TenantSlug string
	OwnerEmail string
	AppKey     string
	Env        store.EnvironmentName
}

// Result is what Run created or found. APIKey is shown once.
type Result struct {
	Tenant      store.Tenant
	Application store.Application
	Environment store.Environment
	Owner       store.User
	Workflow    store.Workflow
	APIKey      auth.Issued
}

// WorkflowKey is the seeded workflow (k6/trigger-load.js triggers it).
const WorkflowKey = "order_shipped"

// SeedSMSDailyLimit is the dev tenant's platform-SMS daily limit.
const SeedSMSDailyLimit = 1_000_000

// Run provisions (or reuses) the seed graph and issues a new API key.
func Run(ctx context.Context, st store.Store, kp crypto.KeyProvider, h *auth.Hasher, o Options) (Result, error) {
	var r Result
	actor := audit.System

	t, err := st.Tenants().GetBySlug(ctx, o.TenantSlug)
	switch {
	case errors.Is(err, store.ErrNotFound):
		t, err = tenant.Provision(ctx, st, kp, actor, tenant.ProvisionInput{Name: "Acme (dev)", Slug: o.TenantSlug, Plan: "dev"})
		if err != nil {
			return r, err
		}
	case err != nil:
		return r, err
	}
	r.Tenant = t

	err = st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		var err error
		if r.Owner, err = ensureOwner(ctx, tx, actor, t.ID, o.OwnerEmail); err != nil {
			return err
		}
		if r.Application, err = ensureApp(ctx, tx, actor, t.ID, o.AppKey); err != nil {
			return err
		}
		r.Environment, err = ensureEnv(ctx, tx, actor, t.ID, r.Application.ID, o.Env)
		return err
	})
	if err != nil {
		return r, err
	}

	if r.Workflow, err = ensureWorkflow(ctx, st, kp, t.ID, r.Owner, r.Application.ID); err != nil {
		return r, err
	}

	r.APIKey, err = auth.IssueAPIKey(ctx, st, h, actor, t.ID, auth.IssueInput{
		ApplicationID: r.Application.ID, EnvironmentID: r.Environment.ID,
		Name: "seed " + time.Now().UTC().Format(time.RFC3339),
	})
	return r, err
}

// ensureWorkflow creates the active order_shipped workflow through the
// workflow service (validation, audit) and lifts the tenant's platform-SMS
// limit, which new tenants start at 0.
func ensureWorkflow(ctx context.Context, st store.Store, kp crypto.KeyProvider, tid store.TenantID, owner store.User, appID uuid.UUID) (store.Workflow, error) {
	if w, err := st.Workflows().GetByKey(ctx, tid, appID, WorkflowKey); err == nil {
		return w, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return store.Workflow{}, err
	}
	limit := SeedSMSDailyLimit
	if _, err := st.Tenants().Update(ctx, tid, store.TenantPatch{PlatformSMSDailyLimit: &limit}); err != nil {
		return store.Workflow{}, fmt.Errorf("sms limit: %w", err)
	}
	log := slog.New(slog.DiscardHandler)
	ts := tenant.NewService(st, kp, &mail.Recorder{}, log, "http://localhost:3000", tenant.DefaultPolicy)
	ws := workflow.NewService(st, ts, log)
	a := tenant.Actor{User: tenant.User{ID: owner.ID, Email: owner.Email, EmailVerified: true, ReauthenticatedAt: time.Now()},
		TenantID: tid, Role: store.RoleOwner}
	sp := func(s string) *string { return &s }
	w, err := ws.Create(ctx, a, appID, workflow.CreateInput{Key: WorkflowKey, Name: "Order shipped",
		PayloadSchema: map[string]any{"type": "object", "properties": map[string]any{
			"order_id": map[string]any{"type": "string"}, "tracking_url": map[string]any{"type": "string"}}}})
	if err != nil {
		return store.Workflow{}, fmt.Errorf("workflow: %w", err)
	}
	for ch, c := range map[string]workflow.Content{
		"email": {Subject: sp("Order {{payload.order_id}} shipped"), Text: sp("Track it: {{payload.tracking_url}}")},
		"sms":   {Text: sp("Order {{payload.order_id}} shipped: {{payload.tracking_url}}")},
	} {
		act, err := ws.CreateAction(ctx, a, appID, w.ID, ch, true)
		if err != nil {
			return store.Workflow{}, fmt.Errorf("%s action: %w", ch, err)
		}
		if _, err := ws.PutTemplate(ctx, a, appID, w.ID, act.ID, "en", c); err != nil {
			return store.Workflow{}, fmt.Errorf("%s template: %w", ch, err)
		}
	}
	active := store.WorkflowActive
	if _, err := ws.Update(ctx, a, appID, w.ID, workflow.Patch{Status: &active}); err != nil {
		return store.Workflow{}, fmt.Errorf("activate: %w", err)
	}
	return st.Workflows().GetByKey(ctx, tid, appID, WorkflowKey)
}

func ensureOwner(ctx context.Context, tx store.Store, actor audit.Actor, tid store.TenantID, email string) (store.User, error) {
	u, err := tx.Users().GetByEmail(ctx, email)
	if errors.Is(err, store.ErrNotFound) {
		if u, err = tx.Users().Create(ctx, store.NewUser{Email: email, Name: "Dev Owner"}); err != nil {
			return u, err
		}
		if err := audit.Record(ctx, tx, actor, audit.Event{Action: "user.create", EntityType: "user", EntityID: u.ID.String()}); err != nil {
			return u, err
		}
	} else if err != nil {
		return u, err
	}
	if _, err := tx.Memberships().Get(ctx, tid, u.ID); errors.Is(err, store.ErrNotFound) {
		m, err := tx.Memberships().Create(ctx, tid, u.ID, store.RoleOwner)
		if err != nil {
			return u, err
		}
		return u, audit.Record(ctx, tx, actor, audit.Event{
			TenantID: &tid, Action: "membership.create", EntityType: "membership", EntityID: m.ID.String(),
			After: map[string]any{"user_id": u.ID.String(), "role": string(m.Role)},
		})
	} else if err != nil {
		return u, err
	}
	return u, nil
}

func ensureApp(ctx context.Context, tx store.Store, actor audit.Actor, tid store.TenantID, key string) (store.Application, error) {
	apps, err := tx.Applications().List(ctx, tid, store.Page{Limit: store.MaxPageSize})
	if err != nil {
		return store.Application{}, err
	}
	for _, a := range apps {
		if a.Key == key {
			return a, nil
		}
	}
	a, err := tx.Applications().Create(ctx, tid, store.NewApplication{Name: "Shop", Key: key, Description: "Seeded for local development"})
	if err != nil {
		return a, err
	}
	return a, audit.Record(ctx, tx, actor, audit.Event{
		TenantID: &tid, Action: "application.create", EntityType: "application", EntityID: a.ID.String(),
		After: map[string]any{"name": a.Name, "key": a.Key},
	})
}

func ensureEnv(ctx context.Context, tx store.Store, actor audit.Actor, tid store.TenantID, appID uuid.UUID, name store.EnvironmentName) (store.Environment, error) {
	envs, err := tx.Environments().List(ctx, tid, appID)
	if err != nil {
		return store.Environment{}, err
	}
	for _, e := range envs {
		if e.Name == name {
			return e, nil
		}
	}
	e, err := tx.Environments().Create(ctx, tid, appID, name)
	if err != nil {
		return e, fmt.Errorf("create environment: %w", err)
	}
	return e, audit.Record(ctx, tx, actor, audit.Event{
		TenantID: &tid, Action: "environment.create", EntityType: "environment", EntityID: e.ID.String(),
		After: map[string]any{"application_id": e.ApplicationID.String(), "name": string(e.Name)},
	})
}
