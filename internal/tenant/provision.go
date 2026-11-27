// Package tenant holds tenant lifecycle services.
package tenant

import (
	"context"
	"fmt"

	"github.com/ticatwolves/notifex/internal/audit"
	"github.com/ticatwolves/notifex/internal/crypto"
	"github.com/ticatwolves/notifex/internal/store"
)

// ProvisionInput describes a new tenant.
type ProvisionInput struct {
	Name string
	Slug string
	Plan string
}

// Provision creates a tenant with a fresh DEK bound to its id (ADR-006),
// sandboxed by default, and records the audit event in the same transaction.
// The plaintext DEK never leaves this function.
func Provision(ctx context.Context, st store.Store, kp crypto.KeyProvider, actor audit.Actor, in ProvisionInput) (store.Tenant, error) {
	id := store.NewTenantID()
	dk, err := kp.GenerateDataKey(ctx, crypto.TenantContext(id.String()))
	if err != nil {
		return store.Tenant{}, fmt.Errorf("provision tenant: %w", err)
	}
	dk.Zero()

	var t store.Tenant
	err = st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		var err error
		t, err = tx.Tenants().Create(ctx, store.NewTenant{
			ID: id, Name: in.Name, Slug: in.Slug, Plan: in.Plan,
			DEK: crypto.WrappedDEK{Wrapped: dk.Wrapped, KeyID: dk.KeyID, Version: 1},
		})
		if err != nil {
			return fmt.Errorf("provision tenant: %w", err)
		}
		return audit.Record(ctx, tx, actor, audit.Event{
			TenantID: &t.ID, Action: "tenant.create", EntityType: "tenant", EntityID: t.ID.String(),
			After: map[string]any{"name": t.Name, "slug": t.Slug, "plan": t.Plan, "sandbox": t.Sandbox},
		})
	})
	if err != nil {
		return store.Tenant{}, err
	}
	return t, nil
}
