package entstore

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ticatwolves/notifex/ent/application"
	"github.com/ticatwolves/notifex/internal/store"
	"github.com/ticatwolves/notifex/internal/store/storetest"
	"github.com/ticatwolves/notifex/internal/store/tenantscope"
)

// These tests bypass the store methods and use the ent client directly, as a
// future careless store method would, to prove scoping fails closed.
func TestScopeEnforcementFailsClosed(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	a, b := f.graph(t), f.graph(t)
	c := f.s.client

	t.Run("query without scope fails", func(t *testing.T) {
		_, err := c.Application.Query().All(ctx)
		require.ErrorIs(t, err, tenantscope.ErrMissing)
		_, err = c.Application.Query().Count(ctx)
		require.ErrorIs(t, err, tenantscope.ErrMissing)
		_, err = c.APIKey.Get(ctx, a.key.ID)
		require.ErrorIs(t, err, tenantscope.ErrMissing)
	})
	t.Run("update and delete without scope fail", func(t *testing.T) {
		_, err := c.Application.Update().SetName("x").Save(ctx)
		require.ErrorIs(t, err, tenantscope.ErrMissing)
		_, err = c.Environment.Delete().Exec(ctx)
		require.ErrorIs(t, err, tenantscope.ErrMissing)
	})
	t.Run("create without scope fails", func(t *testing.T) {
		_, err := c.Application.Create().SetTenantID(a.tenant.ID.UUID()).SetName("x").SetKey("x").Save(ctx)
		require.ErrorIs(t, err, tenantscope.ErrMissing)
	})
	t.Run("scoped query without explicit predicate only sees the scoped tenant", func(t *testing.T) {
		rows, err := c.Application.Query().All(tenantscope.With(ctx, b.tenant.ID.UUID()))
		require.NoError(t, err)
		for _, r := range rows {
			assert.Equal(t, b.tenant.ID.UUID(), r.TenantID)
		}
		_, err = c.Application.Get(tenantscope.With(ctx, b.tenant.ID.UUID()), a.app.ID)
		require.Error(t, err, "A's row is invisible under B's scope")
	})
	t.Run("scoped bulk update cannot touch another tenant", func(t *testing.T) {
		n, err := c.Application.Update().Where(application.ID(a.app.ID)).SetName("pwned").
			Save(tenantscope.With(ctx, b.tenant.ID.UUID()))
		require.NoError(t, err)
		assert.Zero(t, n)
		got, err := f.s.Applications().Get(ctx, a.tenant.ID, a.app.ID)
		require.NoError(t, err)
		assert.Equal(t, a.app.Name, got.Name)
	})
	t.Run("scoped create for another tenant fails", func(t *testing.T) {
		_, err := c.Application.Create().SetTenantID(a.tenant.ID.UUID()).SetName("x").SetKey("smuggled").
			Save(tenantscope.With(ctx, b.tenant.ID.UUID()))
		require.ErrorIs(t, err, tenantscope.ErrMissing)
	})
	t.Run("scoped create fills tenant_id", func(t *testing.T) {
		row, err := c.Application.Create().SetName("x").SetKey("filled-" + uuid.NewString()[:6]).
			Save(tenantscope.With(ctx, b.tenant.ID.UUID()))
		require.NoError(t, err)
		assert.Equal(t, b.tenant.ID.UUID(), row.TenantID)
	})
	t.Run("bypass is explicit", func(t *testing.T) {
		n, err := c.Application.Query().Count(tenantscope.Bypass(ctx, tenantscope.ReasonPlatform))
		require.NoError(t, err)
		assert.GreaterOrEqual(t, n, 2)
	})
	t.Run("transactions are scoped too", func(t *testing.T) {
		err := f.s.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
			_, err := tx.(*Store).client.Application.Query().All(ctx)
			return err
		})
		require.ErrorIs(t, err, tenantscope.ErrMissing)
	})
}

// TestDatabaseRejectsCrossTenantReferences proves the composite foreign keys
// hold at the database level, independent of any Go code: raw SQL that points
// tenant B's rows at tenant A's application or environment is rejected.
func TestDatabaseRejectsCrossTenantReferences(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	db := storetest.DB(t)
	ctx := context.Background()
	a, b := f.graph(t), f.graph(t)

	_, err := db.ExecContext(ctx,
		`INSERT INTO environments (id, tenant_id, created_at, updated_at, application_id, name)
		 VALUES ($1, $2, now(), now(), $3, 'staging')`,
		uuid.New(), b.tenant.ID.UUID(), a.app.ID)
	require.ErrorContains(t, err, "environments_application_fk")

	_, err = db.ExecContext(ctx,
		`INSERT INTO api_keys (id, tenant_id, created_at, updated_at, application_id, environment_id, name, prefix, hash, scopes)
		 VALUES ($1, $2, now(), now(), $3, $4, 'x', $5, '\x00', '[]')`,
		uuid.New(), b.tenant.ID.UUID(), b.app.ID, a.env.ID, "raw-"+uuid.NewString()[:8])
	require.ErrorContains(t, err, "api_keys_environment_fk")

	_, err = db.ExecContext(ctx, `UPDATE environments SET tenant_id = $1 WHERE id = $2`, b.tenant.ID.UUID(), a.env.ID)
	require.Error(t, err, "moving a child to another tenant breaks its composite FK")
}
