// Package devseed provisions a local development tenant end to end: tenant
// (with DEK), owner user, application, environment and a fresh API key. It is
// idempotent apart from the key, which is new on every run. Local only.
package devseed

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/ticatwolves/notifex/internal/audit"
	"github.com/ticatwolves/notifex/internal/auth"
	"github.com/ticatwolves/notifex/internal/crypto"
	"github.com/ticatwolves/notifex/internal/store"
	"github.com/ticatwolves/notifex/internal/tenant"
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
	APIKey      auth.Issued
}

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

	r.APIKey, err = auth.IssueAPIKey(ctx, st, h, actor, t.ID, auth.IssueInput{
		ApplicationID: r.Application.ID, EnvironmentID: r.Environment.ID,
		Name: "seed " + time.Now().UTC().Format(time.RFC3339),
	})
	return r, err
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
