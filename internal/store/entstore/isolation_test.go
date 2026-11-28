package entstore

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ticatwolves/notifex/internal/store"
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
	for _, ss := range []store.SettingsStore{f.s.Brands(), f.s.Policies()} {
		_, err = ss.Put(ctx, a.tenant.ID, nil, map[string]any{"owner": "A"}, nil)
		require.NoError(t, err)
		_, err = ss.Put(ctx, a.tenant.ID, &a.app.ID, map[string]any{"owner": "A-app"}, nil)
		require.NoError(t, err)
	}

	wfA, err := f.s.Workflows().Create(ctx, a.tenant.ID, store.NewWorkflow{ApplicationID: a.app.ID, Key: "wf", Name: "A"})
	require.NoError(t, err)
	actA, err := f.s.Actions().Create(ctx, a.tenant.ID, wfA.ID, "email", true)
	require.NoError(t, err)
	_, err = f.s.Templates().Put(ctx, a.tenant.ID, actA.ID, "en", map[string]any{"subject": "A"})
	require.NoError(t, err)

	credA, err := f.s.Credentials().Create(ctx, a.tenant.ID, store.NewChannelCredential{
		ID: uuid.Must(uuid.NewV7()), ApplicationID: &a.app.ID, Channel: "email", Provider: "sendgrid", Mode: "byo",
		Name: "A", Config: map[string]any{}, SecretCiphertext: []byte("A-secret"),
	})
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
		{"brands.get tenant (B has none; never A's)", func() error { _, err := f.s.Brands().Get(ctx, B, nil); return err }},
		{"brands.get A's override", func() error { _, err := f.s.Brands().Get(ctx, B, &a.app.ID); return err }},
		{"brands.put on A's app", func() error {
			_, err := f.s.Brands().Put(ctx, B, &a.app.ID, map[string]any{"owner": "B"}, nil)
			return err
		}},
		{"brands.delete A's override", func() error { return f.s.Brands().Delete(ctx, B, a.app.ID) }},
		{"policies.get tenant (B has none; never A's)", func() error { _, err := f.s.Policies().Get(ctx, B, nil); return err }},
		{"policies.get A's override", func() error { _, err := f.s.Policies().Get(ctx, B, &a.app.ID); return err }},
		{"policies.put on A's app", func() error {
			_, err := f.s.Policies().Put(ctx, B, &a.app.ID, map[string]any{"owner": "B"}, nil)
			return err
		}},
		{"policies.delete A's override", func() error { return f.s.Policies().Delete(ctx, B, a.app.ID) }},
		{"workflows.create under A's app", func() error {
			_, err := f.s.Workflows().Create(ctx, B, store.NewWorkflow{ApplicationID: a.app.ID, Key: "x", Name: "x"})
			return err
		}},
		{"workflows.get", func() error { _, err := f.s.Workflows().Get(ctx, B, a.app.ID, wfA.ID); return err }},
		{"workflows.get via B's app", func() error { _, err := f.s.Workflows().Get(ctx, B, b.app.ID, wfA.ID); return err }},
		{"workflows.update", func() error {
			_, err := f.s.Workflows().Update(ctx, B, a.app.ID, wfA.ID, store.WorkflowPatch{Name: ptr("pwned")})
			return err
		}},
		{"workflows.delete", func() error { return f.s.Workflows().Delete(ctx, B, a.app.ID, wfA.ID) }},
		{"workflows.lock", func() error { return f.s.Workflows().Lock(ctx, B, a.app.ID, wfA.ID) }},
		{"actions.create on A's workflow", func() error { _, err := f.s.Actions().Create(ctx, B, wfA.ID, "sms", true); return err }},
		{"actions.get", func() error { _, err := f.s.Actions().Get(ctx, B, wfA.ID, actA.ID); return err }},
		{"actions.set_enabled", func() error { _, err := f.s.Actions().SetEnabled(ctx, B, wfA.ID, actA.ID, false); return err }},
		{"actions.delete", func() error { return f.s.Actions().Delete(ctx, B, wfA.ID, actA.ID) }},
		{"templates.get", func() error { _, err := f.s.Templates().Get(ctx, B, actA.ID, "en"); return err }},
		{"templates.put on A's action", func() error {
			_, err := f.s.Templates().Put(ctx, B, actA.ID, "en", map[string]any{"subject": "pwned"})
			return err
		}},
		{"templates.delete", func() error { return f.s.Templates().Delete(ctx, B, actA.ID, "en") }},
		{"credentials.create under A's app", func() error {
			_, err := f.s.Credentials().Create(ctx, B, store.NewChannelCredential{ID: uuid.Must(uuid.NewV7()), ApplicationID: &a.app.ID,
				Channel: "sms", Provider: "twilio", Mode: "byo", Name: "x", Config: map[string]any{}})
			return err
		}},
		{"credentials.create mixing B's app with A's env", func() error {
			_, err := f.s.Credentials().Create(ctx, B, store.NewChannelCredential{ID: uuid.Must(uuid.NewV7()), ApplicationID: &b.app.ID,
				EnvironmentID: &a.env.ID, Channel: "sms", Provider: "twilio", Mode: "byo", Name: "x", Config: map[string]any{}})
			return err
		}},
		{"credentials.get", func() error { _, err := f.s.Credentials().Get(ctx, B, credA.ID); return err }},
		{"credentials.update", func() error {
			_, err := f.s.Credentials().Update(ctx, B, credA.ID, store.CredentialPatch{Name: ptr("pwned")})
			return err
		}},
		{"credentials.rotate", func() error {
			_, err := f.s.Credentials().Rotate(ctx, B, credA.ID, []byte("B"), "B", time.Now())
			return err
		}},
		{"credentials.delete", func() error { return f.s.Credentials().Delete(ctx, B, credA.ID) }},
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
		{"workflows.list of A's app", func() (int, error) {
			l, err := f.s.Workflows().List(ctx, B, a.app.ID, nil, store.Page{})
			return len(l), err
		}},
		{"actions.list of A's workflow", func() (int, error) {
			m, err := f.s.Actions().ListForWorkflows(ctx, B, []uuid.UUID{wfA.ID})
			return len(m), err
		}},
		{"templates.list of A's action", func() (int, error) { l, err := f.s.Templates().List(ctx, B, actA.ID); return len(l), err }},
		{"templates.count of A's action", func() (int, error) { return f.s.Templates().Count(ctx, B, actA.ID) }},
		{"credentials.list", func() (int, error) {
			l, err := f.s.Credentials().List(ctx, B, nil, nil, store.Page{})
			return countTenant(l, a.tenant.ID, func(x store.ChannelCredential) store.TenantID { return x.TenantID }), err
		}},
		{"credentials.active_for_scope of A's app", func() (int, error) {
			l, err := f.s.Credentials().ActiveForScope(ctx, B, "email", a.app.ID, a.env.ID)
			return len(l), err
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
	gotCred, err := f.s.Credentials().Get(ctx, a.tenant.ID, credA.ID)
	require.NoError(t, err)
	assert.Equal(t, "A", gotCred.Name)
	assert.Equal(t, []byte("A-secret"), gotCred.SecretCiphertext)
	gotWF, err := f.s.Workflows().Get(ctx, a.tenant.ID, a.app.ID, wfA.ID)
	require.NoError(t, err)
	assert.Equal(t, "A", gotWF.Name)
	gotAct, err := f.s.Actions().Get(ctx, a.tenant.ID, wfA.ID, actA.ID)
	require.NoError(t, err)
	assert.True(t, gotAct.Enabled)
	gotTpl, err := f.s.Templates().Get(ctx, a.tenant.ID, actA.ID, "en")
	require.NoError(t, err)
	assert.Equal(t, "A", gotTpl.Content["subject"])
	for _, ss := range []store.SettingsStore{f.s.Brands(), f.s.Policies()} {
		got, err := ss.Get(ctx, a.tenant.ID, nil)
		require.NoError(t, err)
		assert.Equal(t, "A", got.Doc["owner"])
		got, err = ss.Get(ctx, a.tenant.ID, &a.app.ID)
		require.NoError(t, err)
		assert.Equal(t, "A-app", got.Doc["owner"])
	}
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
