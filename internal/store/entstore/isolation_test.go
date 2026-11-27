package entstore

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sbshobhit/notifex/internal/store"
)

// TestTenantIsolation runs every tenant-scoped store method as tenant B
// against tenant A's ids. Each must fail with ErrNotFound (no existence leak)
// or return nothing, and tenant A's data must be unchanged afterwards.
// Add a row here for every new tenant-scoped store method.
func TestTenantIsolation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	a, b := f.graph(t), f.graph(t)
	B := b.tenant.ID
	_, err := f.s.Audit().Append(ctx, store.AuditEvent{TenantID: &a.tenant.ID, ActorType: store.ActorUser, ActorID: "u", Action: "a.b", EntityType: "a"})
	require.NoError(t, err)

	notFound := []struct {
		name string
		call func() error
	}{
		{"applications.get", func() error { _, err := f.s.Applications().Get(ctx, B, a.app.ID); return err }},
		{"applications.update", func() error {
			_, err := f.s.Applications().Update(ctx, B, a.app.ID, store.ApplicationPatch{Name: ptr("pwned")})
			return err
		}},
		{"applications.delete", func() error { return f.s.Applications().Delete(ctx, B, a.app.ID) }},
		{"environments.create under A's app", func() error {
			_, err := f.s.Environments().Create(ctx, B, a.app.ID, store.EnvStaging)
			return err
		}},
		{"environments.get", func() error { _, err := f.s.Environments().Get(ctx, B, a.env.ID); return err }},
		{"environments.delete", func() error { return f.s.Environments().Delete(ctx, B, a.env.ID) }},
		{"api_keys.create under A's app/env", func() error {
			_, err := f.s.APIKeys().Create(ctx, B, store.NewAPIKey{ApplicationID: a.app.ID, EnvironmentID: a.env.ID, Name: "x", Prefix: "iso-" + a.key.Prefix, Hash: []byte("h")})
			return err
		}},
		{"api_keys.create mixing B's app with A's env", func() error {
			_, err := f.s.APIKeys().Create(ctx, B, store.NewAPIKey{ApplicationID: b.app.ID, EnvironmentID: a.env.ID, Name: "x", Prefix: "mix-" + a.key.Prefix, Hash: []byte("h")})
			return err
		}},
		{"api_keys.revoke", func() error { return f.s.APIKeys().Revoke(ctx, B, a.key.ID, time.Now()) }},
		{"memberships.get", func() error { _, err := f.s.Memberships().Get(ctx, B, a.user.ID); return err }},
		{"memberships.update_role", func() error {
			_, err := f.s.Memberships().UpdateRole(ctx, B, a.user.ID, store.RoleOwner)
			return err
		}},
		{"memberships.delete", func() error { return f.s.Memberships().Delete(ctx, B, a.user.ID) }},
	}
	for _, tt := range notFound {
		t.Run(tt.name, func(t *testing.T) {
			require.ErrorIs(t, tt.call(), store.ErrNotFound)
		})
	}

	empty := []struct {
		name string
		call func() (int, error)
	}{
		{"applications.list", func() (int, error) {
			l, err := f.s.Applications().List(ctx, B, store.Page{})
			return countTenant(l, a.tenant.ID, func(x store.Application) store.TenantID { return x.TenantID }), err
		}},
		{"environments.list of A's app", func() (int, error) { l, err := f.s.Environments().List(ctx, B, a.app.ID); return len(l), err }},
		{"api_keys.list of A's app", func() (int, error) {
			l, err := f.s.APIKeys().List(ctx, B, a.app.ID, store.Page{})
			return len(l), err
		}},
		{"memberships.list", func() (int, error) {
			l, err := f.s.Memberships().List(ctx, B, store.Page{})
			return countTenant(l, a.tenant.ID, func(x store.Membership) store.TenantID { return x.TenantID }), err
		}},
		{"audit.list", func() (int, error) {
			l, err := f.s.Audit().List(ctx, B, store.Page{})
			n := 0
			for _, e := range l {
				if e.TenantID != nil && *e.TenantID == a.tenant.ID {
					n++
				}
			}
			return n, err
		}},
	}
	for _, tt := range empty {
		t.Run(tt.name, func(t *testing.T) {
			n, err := tt.call()
			require.NoError(t, err)
			assert.Zero(t, n, "tenant B must see none of tenant A's rows")
		})
	}

	// TouchLastUsed on another tenant's key is a silent no-op, never a write.
	require.NoError(t, f.s.APIKeys().TouchLastUsed(ctx, B, a.key.ID, time.Now()))

	// Tenant A is untouched.
	app, err := f.s.Applications().Get(ctx, a.tenant.ID, a.app.ID)
	require.NoError(t, err)
	assert.Equal(t, a.app.Name, app.Name)
	key, err := f.s.APIKeys().GetByPrefix(ctx, a.key.Prefix)
	require.NoError(t, err)
	assert.Nil(t, key.RevokedAt)
	assert.Nil(t, key.LastUsedAt)
	m, err := f.s.Memberships().Get(ctx, a.tenant.ID, a.user.ID)
	require.NoError(t, err)
	assert.Equal(t, store.RoleAdmin, m.Role)
	_, err = f.s.Environments().Get(ctx, a.tenant.ID, a.env.ID)
	require.NoError(t, err)
}

func countTenant[T any](rows []T, tid store.TenantID, get func(T) store.TenantID) int {
	n := 0
	for _, r := range rows {
		if get(r) == tid {
			n++
		}
	}
	return n
}
