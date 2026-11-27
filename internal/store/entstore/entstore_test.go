package entstore

import (
	"context"
	"crypto/rand"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/ticatwolves/notifex/internal/crypto"
	"github.com/ticatwolves/notifex/internal/store"
	"github.com/ticatwolves/notifex/internal/store/storetest"
)

// fixture is a store plus a key provider for provisioning tenants.
type fixture struct {
	s  *Store
	kp *crypto.Local
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	key := make([]byte, crypto.DEKSize)
	_, _ = rand.Read(key)
	kp, err := crypto.NewLocal(key)
	require.NoError(t, err)
	return fixture{s: New(storetest.DB(t)), kp: kp}
}

// tenant provisions a tenant with a real wrapped DEK bound to its id, the
// way signup will.
func (f fixture) tenant(t *testing.T) store.Tenant {
	t.Helper()
	ctx := context.Background()
	id := store.NewTenantID()
	dk, err := f.kp.GenerateDataKey(ctx, crypto.TenantContext(id.String()))
	require.NoError(t, err)
	dk.Zero()
	slug := "t-" + uuid.NewString()[:8]
	ten, err := f.s.Tenants().Create(ctx, store.NewTenant{
		ID: id, Name: "Tenant " + slug, Slug: slug,
		DEK: crypto.WrappedDEK{Wrapped: dk.Wrapped, KeyID: dk.KeyID, Version: 1},
	})
	require.NoError(t, err)
	return ten
}

// graph is one tenant's application, environment and API key.
type graph struct {
	tenant store.Tenant
	app    store.Application
	env    store.Environment
	key    store.APIKey
	user   store.User
	member store.Membership
}

func (f fixture) graph(t *testing.T) graph {
	t.Helper()
	ctx := context.Background()
	g := graph{tenant: f.tenant(t)}
	var err error
	g.app, err = f.s.Applications().Create(ctx, g.tenant.ID, store.NewApplication{Name: "Shop", Key: "shop"})
	require.NoError(t, err)
	g.env, err = f.s.Environments().Create(ctx, g.tenant.ID, g.app.ID, store.EnvProd)
	require.NoError(t, err)
	g.key, err = f.s.APIKeys().Create(ctx, g.tenant.ID, store.NewAPIKey{
		ApplicationID: g.app.ID, EnvironmentID: g.env.ID, Name: "ci",
		Prefix: "p" + uuid.NewString()[:12], Hash: []byte("hash-" + uuid.NewString()),
	})
	require.NoError(t, err)
	g.user, err = f.s.Users().Create(ctx, store.NewUser{Email: fmt.Sprintf("u-%s@example.com", uuid.NewString()[:8])})
	require.NoError(t, err)
	g.member, err = f.s.Memberships().Create(ctx, g.tenant.ID, g.user.ID, store.RoleAdmin)
	require.NoError(t, err)
	return g
}
