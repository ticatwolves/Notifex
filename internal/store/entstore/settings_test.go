package entstore

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ticatwolves/notifex/internal/store"
	"github.com/ticatwolves/notifex/internal/store/storetest"
)

func TestSettingsStores(t *testing.T) {
	t.Parallel()
	for name, st := range map[string]func(f fixture) store.SettingsStore{
		"brands":   func(f fixture) store.SettingsStore { return f.s.Brands() },
		"policies": func(f fixture) store.SettingsStore { return f.s.Policies() },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			ctx := context.Background()
			g := f.graph(t)
			s := st(f)

			_, err := s.Get(ctx, g.tenant.ID, nil)
			require.ErrorIs(t, err, store.ErrNotFound, "unset")

			first, err := s.Put(ctx, g.tenant.ID, nil, map[string]any{"a": "1"}, nil)
			require.NoError(t, err)
			assert.Nil(t, first.ApplicationID)
			assert.Equal(t, map[string]any{"a": "1"}, first.Doc)

			second, err := s.Put(ctx, g.tenant.ID, nil, map[string]any{"b": "2"}, nil)
			require.NoError(t, err)
			assert.Equal(t, map[string]any{"b": "2"}, second.Doc, "put replaces the whole document")
			assert.False(t, second.UpdatedAt.Before(first.UpdatedAt))

			// Application override is separate from the tenant row.
			ov, err := s.Put(ctx, g.tenant.ID, &g.app.ID, map[string]any{"c": "3"}, nil)
			require.NoError(t, err)
			assert.Equal(t, g.app.ID, *ov.ApplicationID)
			got, err := s.Get(ctx, g.tenant.ID, nil)
			require.NoError(t, err)
			assert.Equal(t, map[string]any{"b": "2"}, got.Doc)

			require.NoError(t, s.Delete(ctx, g.tenant.ID, g.app.ID))
			require.ErrorIs(t, s.Delete(ctx, g.tenant.ID, g.app.ID), store.ErrNotFound)
			_, err = s.Get(ctx, g.tenant.ID, &g.app.ID)
			require.ErrorIs(t, err, store.ErrNotFound)

			// Unknown application: not found (composite FK).
			unknown := store.NewTenantID().UUID()
			_, err = s.Put(ctx, g.tenant.ID, &unknown, map[string]any{}, nil)
			require.ErrorIs(t, err, store.ErrNotFound)

			// Deleting the application removes its override (CASCADE).
			app2, err := f.s.Applications().Create(ctx, g.tenant.ID, store.NewApplication{Name: "Two", Key: "two"})
			require.NoError(t, err)
			_, err = s.Put(ctx, g.tenant.ID, &app2.ID, map[string]any{"x": "y"}, nil)
			require.NoError(t, err)
			require.NoError(t, f.s.Applications().Delete(ctx, g.tenant.ID, app2.ID))
			_, err = s.Get(ctx, g.tenant.ID, &app2.ID)
			require.ErrorIs(t, err, store.ErrNotFound)
		})
	}
}

// TestSettingsConcurrentFirstPut: racing first writes end with one row and
// no error (the loser of the unique index updates instead).
func TestSettingsConcurrentFirstPut(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	g := f.graph(t)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, err := f.s.Brands().Put(ctx, g.tenant.ID, nil, map[string]any{"i": float64(i)}, nil)
			errs <- err
		}()
		go func() {
			defer wg.Done()
			_, err := f.s.Policies().Put(ctx, g.tenant.ID, &g.app.ID, map[string]any{"i": float64(i)}, nil)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	db := storetest.DB(t)
	var n int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM brand_profiles WHERE tenant_id = $1`, g.tenant.ID.UUID()).Scan(&n))
	assert.Equal(t, 1, n)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM policies WHERE tenant_id = $1`, g.tenant.ID.UUID()).Scan(&n))
	assert.Equal(t, 1, n)
}

// TestPolicyRetention: 1–7 days on tenant rows only, enforced by the store
// and, underneath it, by CHECK constraints.
func TestPolicyRetention(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	g := f.graph(t)

	p, err := f.s.Policies().Put(ctx, g.tenant.ID, nil, map[string]any{}, ptr(3))
	require.NoError(t, err)
	assert.Equal(t, 3, *p.PayloadRetentionDays)
	p, err = f.s.Policies().Put(ctx, g.tenant.ID, nil, map[string]any{}, nil)
	require.NoError(t, err)
	assert.Nil(t, p.PayloadRetentionDays, "nil clears it (the service applies the default)")

	for _, bad := range []int{0, 8, -1} {
		_, err = f.s.Policies().Put(ctx, g.tenant.ID, nil, map[string]any{}, ptr(bad))
		require.ErrorIs(t, err, store.ErrInvalid, "%d days", bad)
	}
	_, err = f.s.Policies().Put(ctx, g.tenant.ID, &g.app.ID, map[string]any{}, ptr(5))
	require.ErrorIs(t, err, store.ErrInvalid, "override with retention")
	_, err = f.s.Brands().Put(ctx, g.tenant.ID, nil, map[string]any{}, ptr(5))
	require.ErrorIs(t, err, store.ErrInvalid, "brand with retention")

	// The database refuses it too, whoever writes.
	db := storetest.DB(t)
	_, err = db.ExecContext(ctx, `UPDATE policies SET payload_retention_days = 30 WHERE tenant_id = $1`, g.tenant.ID.UUID())
	require.ErrorContains(t, err, "policies_retention_range")
	_, err = db.ExecContext(ctx, `INSERT INTO policies (id, tenant_id, application_id, doc, payload_retention_days, created_at, updated_at)
		VALUES (gen_random_uuid(), $1, $2, '{}', 3, now(), now())`, g.tenant.ID.UUID(), g.app.ID)
	require.ErrorContains(t, err, "policies_retention_tenant_only")
	_, err = db.ExecContext(ctx, `UPDATE policies SET doc = '[]' WHERE tenant_id = $1`, g.tenant.ID.UUID())
	require.ErrorContains(t, err, "policies_doc_object")
}
