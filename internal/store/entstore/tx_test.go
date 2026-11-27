package entstore

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ticatwolves/notifex/internal/store"
)

func TestWithTx(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	ten := f.tenant(t)

	createWithAudit := func(ctx context.Context, tx store.Store, key string) error {
		app, err := tx.Applications().Create(ctx, ten.ID, store.NewApplication{Name: key, Key: key})
		if err != nil {
			return err
		}
		_, err = tx.Audit().Append(ctx, store.AuditEvent{
			TenantID: &ten.ID, ActorType: store.ActorUser, ActorID: "u",
			Action: "application.create", EntityType: "application", EntityID: app.ID.String(),
		})
		return err
	}
	countApps := func() int {
		l, err := f.s.Applications().List(ctx, ten.ID, store.Page{Limit: store.MaxPageSize})
		require.NoError(t, err)
		return len(l)
	}
	countAudit := func() int {
		l, err := f.s.Audit().List(ctx, ten.ID, store.Page{Limit: store.MaxPageSize})
		require.NoError(t, err)
		return len(l)
	}

	t.Run("commit persists mutation and audit together", func(t *testing.T) {
		require.NoError(t, f.s.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
			return createWithAudit(ctx, tx, "committed")
		}))
		assert.Equal(t, 1, countApps())
		assert.Equal(t, 1, countAudit())
	})
	t.Run("error rolls back both", func(t *testing.T) {
		boom := errors.New("boom")
		err := f.s.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
			if err := createWithAudit(ctx, tx, "rolled-back"); err != nil {
				return err
			}
			return boom
		})
		require.ErrorIs(t, err, boom)
		assert.Equal(t, 1, countApps())
		assert.Equal(t, 1, countAudit())
	})
	t.Run("constraint error inside tx rolls back earlier writes", func(t *testing.T) {
		err := f.s.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
			if err := createWithAudit(ctx, tx, "first"); err != nil {
				return err
			}
			return createWithAudit(ctx, tx, "committed") // duplicate key
		})
		require.ErrorIs(t, err, store.ErrConflict)
		assert.Equal(t, 1, countApps())
	})
	t.Run("panic rolls back and re-panics", func(t *testing.T) {
		assert.Panics(t, func() {
			_ = f.s.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
				_ = createWithAudit(ctx, tx, "panicked")
				panic("kaboom")
			})
		})
		assert.Equal(t, 1, countApps())
	})
	t.Run("nested WithTx joins the outer transaction", func(t *testing.T) {
		err := f.s.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
			if err := tx.WithTx(ctx, func(ctx context.Context, inner store.Store) error {
				return createWithAudit(ctx, inner, "nested")
			}); err != nil {
				return err
			}
			return errors.New("outer fails")
		})
		require.Error(t, err)
		assert.Equal(t, 1, countApps(), "inner write rolled back with the outer tx")
	})
}
