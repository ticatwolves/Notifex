package entstore

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ticatwolves/notifex/internal/crypto"
	"github.com/ticatwolves/notifex/internal/store"
)

func ptr[T any](v T) *T { return &v }

func TestTenants(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	ten := f.tenant(t)

	got, err := f.s.Tenants().Get(ctx, ten.ID)
	require.NoError(t, err)
	assert.Equal(t, store.TenantActive, got.Status)
	assert.True(t, got.Sandbox, "new tenants default to sandbox")
	assert.Equal(t, "free", got.Plan)
	assert.False(t, got.DEKShredded)

	bySlug, err := f.s.Tenants().GetBySlug(ctx, ten.Slug)
	require.NoError(t, err)
	assert.Equal(t, ten.ID, bySlug.ID)

	upd, err := f.s.Tenants().Update(ctx, ten.ID, store.TenantPatch{
		Name: ptr("Renamed"), Status: ptr(store.TenantSuspended), PlatformSMSDailyLimit: ptr(500),
		Sandbox: ptr(false), Quotas: map[string]int64{"triggers_per_month": 100000},
	})
	require.NoError(t, err)
	assert.Equal(t, "Renamed", upd.Name)
	assert.Equal(t, store.TenantSuspended, upd.Status)
	assert.Equal(t, 500, upd.PlatformSMSDailyLimit)
	assert.False(t, upd.Sandbox)
	assert.Equal(t, int64(100000), upd.Quotas["triggers_per_month"])
	assert.True(t, upd.UpdatedAt.After(ten.UpdatedAt) || upd.UpdatedAt.Equal(ten.UpdatedAt))

	_, err = f.s.Tenants().Get(ctx, store.NewTenantID())
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = f.s.Tenants().Update(ctx, store.NewTenantID(), store.TenantPatch{Name: ptr("x")})
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestTenantCreateValidation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	existing := f.tenant(t)
	dek := crypto.WrappedDEK{Wrapped: []byte("w"), KeyID: "k", Version: 1}
	tests := []struct {
		name string
		in   store.NewTenant
		want error
	}{
		{name: "missing id", in: store.NewTenant{Name: "a", Slug: "ok-slug", DEK: dek}, want: store.ErrInvalid},
		{name: "bad slug", in: store.NewTenant{ID: store.NewTenantID(), Name: "a", Slug: "Bad_Slug", DEK: dek}, want: store.ErrInvalid},
		{name: "slug too long", in: store.NewTenant{ID: store.NewTenantID(), Name: "a", Slug: strings.Repeat("a", 64), DEK: dek}, want: store.ErrInvalid},
		{name: "missing dek", in: store.NewTenant{ID: store.NewTenantID(), Name: "a", Slug: "no-dek"}, want: store.ErrInvalid},
		{name: "empty name", in: store.NewTenant{ID: store.NewTenantID(), Slug: "no-name", DEK: dek}, want: store.ErrInvalid},
		{name: "negative sms limit", in: store.NewTenant{ID: store.NewTenantID(), Name: "a", Slug: "neg-" + uuid.NewString()[:6], PlatformSMSDailyLimit: -1, DEK: dek}, want: store.ErrInvalid},
		{name: "duplicate slug", in: store.NewTenant{ID: store.NewTenantID(), Name: "a", Slug: existing.Slug, DEK: dek}, want: store.ErrConflict},
		{name: "duplicate id", in: store.NewTenant{ID: existing.ID, Name: "a", Slug: "dup-" + uuid.NewString()[:6], DEK: dek}, want: store.ErrConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := f.s.Tenants().Create(ctx, tt.in)
			require.ErrorIs(t, err, tt.want)
		})
	}
}

func TestTenantDEKAndCryptoShred(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	victim, bystander := f.tenant(t), f.tenant(t)

	// The store is the cipher's DEKSource, end to end.
	cipher := crypto.NewCipher(f.kp, f.s.Tenants(), crypto.CipherOptions{})
	aad := crypto.AAD{TenantID: victim.ID.UUID(), RowID: uuid.New(), Column: "payload"}
	ct, err := cipher.Encrypt(ctx, aad, []byte(`{"email":"a@b.com"}`))
	require.NoError(t, err)
	bAAD := crypto.AAD{TenantID: bystander.ID.UUID(), RowID: uuid.New(), Column: "payload"}
	bCT, err := cipher.Encrypt(ctx, bAAD, []byte("other"))
	require.NoError(t, err)

	require.NoError(t, f.s.Tenants().ShredDEK(ctx, victim.ID))
	cipher.Forget(victim.ID.UUID())

	_, err = f.s.Tenants().WrappedDEK(ctx, victim.ID.UUID())
	require.ErrorIs(t, err, crypto.ErrDataKeyDestroyed)
	got, err := f.s.Tenants().Get(ctx, victim.ID)
	require.NoError(t, err)
	assert.True(t, got.DEKShredded)

	_, err = cipher.Decrypt(ctx, aad, ct)
	require.ErrorIs(t, err, crypto.ErrDataKeyDestroyed, "shredded tenant's data is unreadable")
	pt, err := cipher.Decrypt(ctx, bAAD, bCT)
	require.NoError(t, err, "other tenants are unaffected")
	assert.Equal(t, []byte("other"), pt)

	// A fresh cipher (cold cache, e.g. another instance after TTL) also fails.
	_, err = crypto.NewCipher(f.kp, f.s.Tenants(), crypto.CipherOptions{}).Decrypt(ctx, aad, ct)
	require.ErrorIs(t, err, crypto.ErrDataKeyDestroyed)

	require.ErrorIs(t, f.s.Tenants().ShredDEK(ctx, store.NewTenantID()), store.ErrNotFound)
}

func TestUsers(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	local := "Mixed.Case+" + uuid.NewString()[:6]
	hash := "$argon2id$v=19$m=65536,t=3,p=4$c2FsdA$aGFzaA"

	u, err := f.s.Users().Create(ctx, store.NewUser{Email: "  " + local + "@Example.COM ", Name: "Ada", PasswordHash: &hash})
	require.NoError(t, err)
	assert.Equal(t, strings.ToLower(local)+"@example.com", u.Email, "emails are normalised")
	assert.Equal(t, store.UserActive, u.Status)

	got, err := f.s.Users().GetByEmail(ctx, strings.ToUpper(local)+"@EXAMPLE.com")
	require.NoError(t, err, "lookup is case-insensitive")
	assert.Equal(t, u.ID, got.ID)
	require.NotNil(t, got.PasswordHash)

	_, err = f.s.Users().Create(ctx, store.NewUser{Email: strings.ToUpper(u.Email)})
	require.ErrorIs(t, err, store.ErrConflict, "duplicate email in another case")

	for _, bad := range []string{"", "no-at-sign", "@example.com", "user@"} {
		_, err = f.s.Users().Create(ctx, store.NewUser{Email: bad})
		require.ErrorIs(t, err, store.ErrInvalid, "email %q", bad)
	}
	_, err = f.s.Users().Get(ctx, uuid.New())
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestMemberships(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	g := f.graph(t)
	other := f.tenant(t)

	_, err := f.s.Memberships().Create(ctx, g.tenant.ID, g.user.ID, store.RoleViewer)
	require.ErrorIs(t, err, store.ErrConflict, "one membership per user per tenant")
	_, err = f.s.Memberships().Create(ctx, g.tenant.ID, uuid.New(), store.RoleViewer)
	require.ErrorIs(t, err, store.ErrNotFound, "unknown user")
	_, err = f.s.Memberships().Create(ctx, g.tenant.ID, g.user.ID, store.Role("superuser"))
	require.ErrorIs(t, err, store.ErrInvalid)

	m2, err := f.s.Memberships().Create(ctx, other.ID, g.user.ID, store.RoleOwner)
	require.NoError(t, err)

	upd, err := f.s.Memberships().UpdateRole(ctx, g.tenant.ID, g.user.ID, store.RoleDeveloper)
	require.NoError(t, err)
	assert.Equal(t, store.RoleDeveloper, upd.Role)

	list, err := f.s.Memberships().List(ctx, g.tenant.ID, store.Page{})
	require.NoError(t, err)
	require.Len(t, list, 1)

	mine, err := f.s.Memberships().ListForUser(ctx, g.user.ID)
	require.NoError(t, err)
	assert.ElementsMatch(t, []uuid.UUID{g.member.ID, m2.ID}, []uuid.UUID{mine[0].ID, mine[1].ID})

	require.NoError(t, f.s.Memberships().Delete(ctx, g.tenant.ID, g.user.ID))
	_, err = f.s.Memberships().Get(ctx, g.tenant.ID, g.user.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
	require.ErrorIs(t, f.s.Memberships().Delete(ctx, g.tenant.ID, g.user.ID), store.ErrNotFound)
}

func TestApplications(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	ten, other := f.tenant(t), f.tenant(t)

	app, err := f.s.Applications().Create(ctx, ten.ID, store.NewApplication{Name: "Shop", Key: "shop", Description: "d"})
	require.NoError(t, err)
	_, err = f.s.Applications().Create(ctx, ten.ID, store.NewApplication{Name: "Shop 2", Key: "shop"})
	require.ErrorIs(t, err, store.ErrConflict, "key unique within a tenant")
	_, err = f.s.Applications().Create(ctx, other.ID, store.NewApplication{Name: "Shop", Key: "shop"})
	require.NoError(t, err, "same key is fine in another tenant")
	_, err = f.s.Applications().Create(ctx, ten.ID, store.NewApplication{Name: "x", Key: "Not A Slug"})
	require.ErrorIs(t, err, store.ErrInvalid)

	upd, err := f.s.Applications().Update(ctx, ten.ID, app.ID, store.ApplicationPatch{Name: ptr("Store")})
	require.NoError(t, err)
	assert.Equal(t, "Store", upd.Name)
	assert.Equal(t, "d", upd.Description, "unset fields unchanged")

	// Keyset pagination in creation order.
	for i := range 4 {
		_, err := f.s.Applications().Create(ctx, ten.ID, store.NewApplication{Name: "a", Key: "app-" + string(rune('a'+i))})
		require.NoError(t, err)
	}
	p1, err := f.s.Applications().List(ctx, ten.ID, store.Page{Limit: 2})
	require.NoError(t, err)
	require.Len(t, p1, 2)
	assert.Equal(t, app.ID, p1[0].ID)
	p2, err := f.s.Applications().List(ctx, ten.ID, store.Page{Limit: 2, After: p1[1].ID})
	require.NoError(t, err)
	p3, err := f.s.Applications().List(ctx, ten.ID, store.Page{Limit: 2, After: p2[1].ID})
	require.NoError(t, err)
	assert.Len(t, p2, 2)
	assert.Len(t, p3, 1)
	seen := map[uuid.UUID]bool{}
	for _, a := range append(append(p1, p2...), p3...) {
		assert.False(t, seen[a.ID], "no duplicates across pages")
		seen[a.ID] = true
		assert.Equal(t, ten.ID, a.TenantID)
	}

	// Delete is restricted while environments reference the app.
	_, err = f.s.Environments().Create(ctx, ten.ID, app.ID, store.EnvDev)
	require.NoError(t, err)
	require.ErrorIs(t, f.s.Applications().Delete(ctx, ten.ID, app.ID), store.ErrConflict)
}

func TestPageNormalize(t *testing.T) {
	assert.Equal(t, store.DefaultPageSize, store.Page{}.Normalize().Limit)
	assert.Equal(t, store.MaxPageSize, store.Page{Limit: 10_000}.Normalize().Limit)
	assert.Equal(t, 7, store.Page{Limit: 7}.Normalize().Limit)
}

func TestEnvironments(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	g := f.graph(t)

	_, err := f.s.Environments().Create(ctx, g.tenant.ID, g.app.ID, store.EnvProd)
	require.ErrorIs(t, err, store.ErrConflict, "one env per name per app")
	_, err = f.s.Environments().Create(ctx, g.tenant.ID, uuid.New(), store.EnvDev)
	require.ErrorIs(t, err, store.ErrNotFound, "unknown app")
	_, err = f.s.Environments().Create(ctx, g.tenant.ID, g.app.ID, store.EnvironmentName("qa"))
	require.ErrorIs(t, err, store.ErrInvalid)

	staging, err := f.s.Environments().Create(ctx, g.tenant.ID, g.app.ID, store.EnvStaging)
	require.NoError(t, err)
	list, err := f.s.Environments().List(ctx, g.tenant.ID, g.app.ID)
	require.NoError(t, err)
	assert.Len(t, list, 2)

	require.ErrorIs(t, f.s.Environments().Delete(ctx, g.tenant.ID, g.env.ID), store.ErrConflict, "API key references it")
	require.NoError(t, f.s.Environments().Delete(ctx, g.tenant.ID, staging.ID))
	_, err = f.s.Environments().Get(ctx, g.tenant.ID, staging.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestAPIKeys(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	g := f.graph(t)

	got, err := f.s.APIKeys().GetByPrefix(ctx, g.key.Prefix)
	require.NoError(t, err)
	assert.Equal(t, g.tenant.ID, got.TenantID, "prefix lookup resolves the tenant")
	assert.Equal(t, g.key.Hash, got.Hash)
	assert.Empty(t, got.Scopes)
	_, err = f.s.APIKeys().GetByPrefix(ctx, "nope")
	require.ErrorIs(t, err, store.ErrNotFound)

	// Environment of a different application in the same tenant: rejected by
	// the (tenant, app, env) composite FK.
	app2, err := f.s.Applications().Create(ctx, g.tenant.ID, store.NewApplication{Name: "Other", Key: "other"})
	require.NoError(t, err)
	_, err = f.s.APIKeys().Create(ctx, g.tenant.ID, store.NewAPIKey{
		ApplicationID: app2.ID, EnvironmentID: g.env.ID, Name: "x", Prefix: "px" + uuid.NewString()[:8], Hash: []byte("h"),
	})
	require.ErrorIs(t, err, store.ErrNotFound)

	_, err = f.s.APIKeys().Create(ctx, g.tenant.ID, store.NewAPIKey{
		ApplicationID: g.app.ID, EnvironmentID: g.env.ID, Name: "dup", Prefix: g.key.Prefix, Hash: []byte("h"),
	})
	require.ErrorIs(t, err, store.ErrConflict, "prefixes are globally unique")
	_, err = f.s.APIKeys().Create(ctx, g.tenant.ID, store.NewAPIKey{ApplicationID: g.app.ID, EnvironmentID: g.env.ID, Name: "x"})
	require.ErrorIs(t, err, store.ErrInvalid)

	list, err := f.s.APIKeys().List(ctx, g.tenant.ID, g.app.ID, store.Page{})
	require.NoError(t, err)
	require.Len(t, list, 1)

	// Revoke is idempotent and keeps the first timestamp.
	t1 := time.Now().UTC().Truncate(time.Microsecond)
	require.NoError(t, f.s.APIKeys().Revoke(ctx, g.tenant.ID, g.key.ID, t1))
	require.NoError(t, f.s.APIKeys().Revoke(ctx, g.tenant.ID, g.key.ID, t1.Add(time.Hour)))
	got, err = f.s.APIKeys().GetByPrefix(ctx, g.key.Prefix)
	require.NoError(t, err)
	require.NotNil(t, got.RevokedAt)
	assert.True(t, got.RevokedAt.Equal(t1))
	require.ErrorIs(t, f.s.APIKeys().Revoke(ctx, g.tenant.ID, uuid.New(), t1), store.ErrNotFound)
}

func TestAPIKeyTouchLastUsedIsThrottled(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	g := f.graph(t)
	t0 := time.Now().UTC().Truncate(time.Microsecond)

	lastUsed := func() time.Time {
		k, err := f.s.APIKeys().GetByPrefix(ctx, g.key.Prefix)
		require.NoError(t, err)
		require.NotNil(t, k.LastUsedAt)
		return *k.LastUsedAt
	}
	require.NoError(t, f.s.APIKeys().TouchLastUsed(ctx, g.tenant.ID, g.key.ID, t0))
	assert.True(t, lastUsed().Equal(t0))
	require.NoError(t, f.s.APIKeys().TouchLastUsed(ctx, g.tenant.ID, g.key.ID, t0.Add(30*time.Second)))
	assert.True(t, lastUsed().Equal(t0), "within the granularity window: no write")
	require.NoError(t, f.s.APIKeys().TouchLastUsed(ctx, g.tenant.ID, g.key.ID, t0.Add(2*time.Minute)))
	assert.True(t, lastUsed().Equal(t0.Add(2*time.Minute)))
}

func TestAudit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	ten, other := f.tenant(t), f.tenant(t)

	var ids []uuid.UUID
	for i := range 3 {
		e, err := f.s.Audit().Append(ctx, store.AuditEvent{
			TenantID: &ten.ID, ActorType: store.ActorUser, ActorID: "u1",
			Action: "application.update", EntityType: "application", EntityID: "a1",
			Before: map[string]any{"name": i}, After: map[string]any{"name": i + 1}, RequestID: "req",
		})
		require.NoError(t, err)
		ids = append(ids, e.ID)
	}
	_, err := f.s.Audit().Append(ctx, store.AuditEvent{TenantID: &other.ID, ActorType: store.ActorUser, ActorID: "u2", Action: "x.y", EntityType: "x"})
	require.NoError(t, err)
	_, err = f.s.Audit().Append(ctx, store.AuditEvent{ActorType: store.ActorOperator, ActorID: "op", Action: "tenant.suspend", EntityType: "tenant"})
	require.NoError(t, err, "platform events have no tenant")
	_, err = f.s.Audit().Append(ctx, store.AuditEvent{ActorType: store.ActorUser})
	require.ErrorIs(t, err, store.ErrInvalid)

	page1, err := f.s.Audit().List(ctx, ten.ID, store.Page{Limit: 2})
	require.NoError(t, err)
	require.Len(t, page1, 2)
	assert.Equal(t, ids[2], page1[0].ID, "newest first")
	assert.Equal(t, ids[1], page1[1].ID)
	page2, err := f.s.Audit().List(ctx, ten.ID, store.Page{Limit: 2, After: page1[1].ID})
	require.NoError(t, err)
	require.Len(t, page2, 1)
	assert.Equal(t, ids[0], page2[0].ID)
	assert.InDelta(t, 1.0, page2[0].After["name"], 0)
}
