package tenant_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ticatwolves/notifex/internal/auth"
	"github.com/ticatwolves/notifex/internal/store"
	"github.com/ticatwolves/notifex/internal/tenant"
)

// appFixture is a tenant with one member per role and an app with dev and
// prod environments.
type appFixture struct {
	e                         *env
	ten                       store.Tenant
	viewer, dev, admin, owner tenant.Actor
	app                       store.Application
	devEnv, prodEnv           store.Environment
}

func newAppFixture(t *testing.T) appFixture {
	t.Helper()
	e := newEnv(t)
	ownerU := e.user(t, true)
	f := appFixture{e: e, ten: e.tenantFor(t, ownerU)}
	f.owner = e.access(t, ownerU, f.ten)
	f.viewer, f.dev, f.admin = e.member(t, f.ten, store.RoleViewer), e.member(t, f.ten, store.RoleDeveloper), e.member(t, f.ten, store.RoleAdmin)
	ctx := context.Background()
	var err error
	f.app, err = e.svc.CreateApplication(ctx, f.dev, store.NewApplication{Name: "Shop", Key: "shop"})
	require.NoError(t, err)
	f.devEnv, err = e.svc.CreateEnvironment(ctx, f.dev, f.app.ID, store.EnvDev)
	require.NoError(t, err)
	f.prodEnv, err = e.svc.CreateEnvironment(ctx, f.dev, f.app.ID, store.EnvProd)
	require.NoError(t, err)
	return f
}

func (f appFixture) authn() *auth.APIKeyAuthenticator {
	a := auth.NewAPIKeyAuthenticator(f.e.st, f.e.hasher, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	a.SetClock(func() time.Time { f.e.mu.Lock(); defer f.e.mu.Unlock(); return f.e.now })
	return a
}

func TestApplicationPermissions(t *testing.T) {
	t.Parallel()
	f := newAppFixture(t)
	ctx := context.Background()
	s := f.e.svc

	_, err := s.CreateApplication(ctx, f.viewer, store.NewApplication{Name: "x", Key: "x1"})
	require.ErrorIs(t, err, tenant.ErrForbidden, "viewers can't create")
	apps, err := s.ListApplications(ctx, f.viewer, store.Page{})
	require.NoError(t, err, "viewers can list")
	require.Len(t, apps, 1)

	name := "Store"
	upd, err := s.UpdateApplication(ctx, f.dev, f.app.ID, store.ApplicationPatch{Name: &name})
	require.NoError(t, err)
	assert.Equal(t, "Store", upd.Name)

	other, err := s.CreateApplication(ctx, f.dev, store.NewApplication{Name: "Other", Key: "other"})
	require.NoError(t, err)
	require.ErrorIs(t, s.DeleteApplication(ctx, f.dev, other.ID), tenant.ErrForbidden, "developers can't delete")
	require.NoError(t, s.DeleteApplication(ctx, f.admin, other.ID))
	require.ErrorIs(t, s.DeleteApplication(ctx, f.admin, f.app.ID), store.ErrConflict, "still has environments")

	_, err = s.CreateApplication(ctx, f.dev, store.NewApplication{Name: "Dup", Key: "shop"})
	require.ErrorIs(t, err, store.ErrConflict)
}

func TestAPIKeyPermissionsByEnvironment(t *testing.T) {
	t.Parallel()
	f := newAppFixture(t)
	ctx := context.Background()
	s := f.e.svc

	_, err := s.CreateAPIKey(ctx, f.viewer, f.app.ID, tenant.KeyInput{EnvironmentID: f.devEnv.ID, Name: "k"})
	require.ErrorIs(t, err, tenant.ErrForbidden)
	devKey, err := s.CreateAPIKey(ctx, f.dev, f.app.ID, tenant.KeyInput{EnvironmentID: f.devEnv.ID, Name: "ci", Scopes: []string{"trigger"}})
	require.NoError(t, err, "developers may create non-prod keys")
	_, err = s.CreateAPIKey(ctx, f.dev, f.app.ID, tenant.KeyInput{EnvironmentID: f.prodEnv.ID, Name: "prod"})
	require.ErrorIs(t, err, tenant.ErrForbidden, "but not prod keys")
	prodKey, err := s.CreateAPIKey(ctx, f.admin, f.app.ID, tenant.KeyInput{EnvironmentID: f.prodEnv.ID, Name: "prod"})
	require.NoError(t, err)

	require.ErrorIs(t, s.RevokeAPIKey(ctx, f.dev, f.app.ID, prodKey.Key.ID), tenant.ErrForbidden, "developers can't revoke prod keys")
	_, err = s.RotateAPIKey(ctx, f.dev, f.app.ID, prodKey.Key.ID, time.Hour)
	require.ErrorIs(t, err, tenant.ErrForbidden, "or rotate them")
	require.NoError(t, s.RevokeAPIKey(ctx, f.dev, f.app.ID, devKey.Key.ID))

	_, err = s.ListAPIKeys(ctx, f.viewer, f.app.ID, store.Page{})
	require.ErrorIs(t, err, tenant.ErrForbidden, "viewers can't see keys")
	keys, err := s.ListAPIKeys(ctx, f.dev, f.app.ID, store.Page{})
	require.NoError(t, err)
	assert.Len(t, keys, 2)

	// An environment or key from another application is not found.
	app2, err := s.CreateApplication(ctx, f.dev, store.NewApplication{Name: "Two", Key: "two"})
	require.NoError(t, err)
	_, err = s.CreateAPIKey(ctx, f.admin, app2.ID, tenant.KeyInput{EnvironmentID: f.devEnv.ID, Name: "x"})
	require.ErrorIs(t, err, store.ErrNotFound)
	require.ErrorIs(t, s.RevokeAPIKey(ctx, f.admin, app2.ID, prodKey.Key.ID), store.ErrNotFound)
}

func TestAPIKeyInputValidation(t *testing.T) {
	t.Parallel()
	f := newAppFixture(t)
	ctx := context.Background()
	past, far := f.e.now.Add(-time.Minute), f.e.now.Add(3*365*24*time.Hour)
	for name, in := range map[string]tenant.KeyInput{
		"empty name":  {EnvironmentID: f.devEnv.ID, Name: " "},
		"bad scope":   {EnvironmentID: f.devEnv.ID, Name: "k", Scopes: []string{"Trigger Everything"}},
		"past expiry": {EnvironmentID: f.devEnv.ID, Name: "k", ExpiresAt: &past},
		"too far":     {EnvironmentID: f.devEnv.ID, Name: "k", ExpiresAt: &far},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := f.e.svc.CreateAPIKey(ctx, f.admin, f.app.ID, in)
			require.ErrorIs(t, err, store.ErrInvalid)
		})
	}
}

func TestAPIKeyRotationGracePeriod(t *testing.T) {
	t.Parallel()
	f := newAppFixture(t)
	ctx := context.Background()
	s, a := f.e.svc, f.authn()

	old, err := s.CreateAPIKey(ctx, f.dev, f.app.ID, tenant.KeyInput{EnvironmentID: f.devEnv.ID, Name: "ci", Scopes: []string{"trigger"}})
	require.NoError(t, err)
	r, err := s.RotateAPIKey(ctx, f.dev, f.app.ID, old.Key.ID, time.Hour)
	require.NoError(t, err)
	assert.True(t, r.OldKeyRevokedAt.Equal(f.e.now.Add(time.Hour)))
	assert.Equal(t, old.Key.Scopes, r.New.Key.Scopes, "same scopes")
	assert.Equal(t, old.Key.EnvironmentID, r.New.Key.EnvironmentID)

	for _, k := range []string{old.Plaintext, r.New.Plaintext} {
		_, err := a.Authenticate(ctx, k)
		require.NoError(t, err, "both keys work during the grace period")
	}
	f.e.advance(time.Hour)
	_, err = a.Authenticate(ctx, old.Plaintext)
	require.ErrorIs(t, err, auth.ErrUnauthenticated, "old key stops at the end of the grace period")
	_, err = a.Authenticate(ctx, r.New.Plaintext)
	require.NoError(t, err)

	_, err = s.RotateAPIKey(ctx, f.dev, f.app.ID, old.Key.ID, time.Hour)
	require.ErrorIs(t, err, tenant.ErrKeyInactive, "can't rotate a rotated key")
	_, err = s.RotateAPIKey(ctx, f.dev, f.app.ID, r.New.Key.ID, 8*24*time.Hour)
	require.ErrorIs(t, err, store.ErrInvalid, "grace over 7 days")
}

func TestRevokeCutsGracePeriodShort(t *testing.T) {
	t.Parallel()
	f := newAppFixture(t)
	ctx := context.Background()
	s, a := f.e.svc, f.authn()

	old, err := s.CreateAPIKey(ctx, f.admin, f.app.ID, tenant.KeyInput{EnvironmentID: f.prodEnv.ID, Name: "leaked"})
	require.NoError(t, err)
	_, err = s.RotateAPIKey(ctx, f.admin, f.app.ID, old.Key.ID, 7*24*time.Hour)
	require.NoError(t, err)
	// The old key leaked: revoke it now instead of waiting a week.
	require.NoError(t, s.RevokeAPIKey(ctx, f.admin, f.app.ID, old.Key.ID))
	_, err = a.Authenticate(ctx, old.Plaintext)
	require.ErrorIs(t, err, auth.ErrUnauthenticated)

	zero, err := s.CreateAPIKey(ctx, f.admin, f.app.ID, tenant.KeyInput{EnvironmentID: f.prodEnv.ID, Name: "z"})
	require.NoError(t, err)
	_, err = s.RotateAPIKey(ctx, f.admin, f.app.ID, zero.Key.ID, 0)
	require.NoError(t, err)
	_, err = a.Authenticate(ctx, zero.Plaintext)
	require.ErrorIs(t, err, auth.ErrUnauthenticated, "grace 0 revokes immediately")
}

func TestDeleteEnvironment(t *testing.T) {
	t.Parallel()
	f := newAppFixture(t)
	ctx := context.Background()
	s := f.e.svc

	k, err := s.CreateAPIKey(ctx, f.admin, f.app.ID, tenant.KeyInput{EnvironmentID: f.devEnv.ID, Name: "k"})
	require.NoError(t, err)
	require.ErrorIs(t, s.DeleteEnvironment(ctx, f.dev, f.app.ID, f.devEnv.ID), tenant.ErrForbidden)
	require.ErrorIs(t, s.DeleteEnvironment(ctx, f.admin, f.app.ID, f.devEnv.ID), store.ErrConflict, "active key")

	// A key in its rotation grace period still counts as active.
	_, err = s.RotateAPIKey(ctx, f.admin, f.app.ID, k.Key.ID, time.Hour)
	require.NoError(t, err)
	keys, _ := s.ListAPIKeys(ctx, f.admin, f.app.ID, store.Page{})
	for _, key := range keys {
		require.NoError(t, s.RevokeAPIKey(ctx, f.admin, f.app.ID, key.ID))
	}
	require.NoError(t, s.DeleteEnvironment(ctx, f.admin, f.app.ID, f.devEnv.ID), "only revoked keys left")
	_, err = f.e.st.APIKeys().GetByPrefix(ctx, k.Key.Prefix)
	require.ErrorIs(t, err, store.ErrNotFound, "revoked key rows removed with the environment")

	app2, err := s.CreateApplication(ctx, f.dev, store.NewApplication{Name: "Two", Key: "two"})
	require.NoError(t, err)
	require.ErrorIs(t, s.DeleteEnvironment(ctx, f.admin, app2.ID, f.prodEnv.ID), store.ErrNotFound, "environment of another app")
	require.ErrorIs(t, s.DeleteEnvironment(ctx, f.admin, f.app.ID, uuid.New()), store.ErrNotFound)
}
