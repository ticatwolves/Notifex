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

func TestWorkflowActionTemplateStores(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	g := f.graph(t)
	tid := g.tenant.ID

	_, err := f.s.Workflows().Create(ctx, tid, store.NewWorkflow{ApplicationID: g.app.ID, Key: "Bad Key", Name: "x"})
	require.ErrorIs(t, err, store.ErrInvalid)
	_, err = f.s.Workflows().Create(ctx, tid, store.NewWorkflow{ApplicationID: uuid.Must(uuid.NewV7()), Key: "k", Name: "x"})
	require.ErrorIs(t, err, store.ErrNotFound, "unknown application")

	schema := map[string]any{"type": "object"}
	wf, err := f.s.Workflows().Create(ctx, tid, store.NewWorkflow{ApplicationID: g.app.ID, Key: "order_shipped", Name: "Shipped", PayloadSchema: schema})
	require.NoError(t, err)
	assert.Equal(t, store.WorkflowDraft, wf.Status)
	assert.Equal(t, schema, wf.PayloadSchema)
	_, err = f.s.Workflows().Create(ctx, tid, store.NewWorkflow{ApplicationID: g.app.ID, Key: "order_shipped", Name: "dup"})
	require.ErrorIs(t, err, store.ErrConflict)

	byKey, err := f.s.Workflows().GetByKey(ctx, tid, g.app.ID, "order_shipped")
	require.NoError(t, err)
	assert.Equal(t, wf.ID, byKey.ID)
	_, err = f.s.Workflows().GetByKey(ctx, tid, g.app.ID, "nope")
	require.ErrorIs(t, err, store.ErrNotFound)

	active := store.WorkflowActive
	wf, err = f.s.Workflows().Update(ctx, tid, g.app.ID, wf.ID, store.WorkflowPatch{Name: ptr("Order shipped"), Status: &active, ClearSchema: true})
	require.NoError(t, err)
	assert.Equal(t, "Order shipped", wf.Name)
	assert.Nil(t, wf.PayloadSchema)
	list, err := f.s.Workflows().List(ctx, tid, g.app.ID, &active, store.Page{})
	require.NoError(t, err)
	require.Len(t, list, 1)

	// Another application's id path doesn't reach it.
	other, err := f.s.Applications().Create(ctx, tid, store.NewApplication{Name: "Other", Key: "other"})
	require.NoError(t, err)
	_, err = f.s.Workflows().Get(ctx, tid, other.ID, wf.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
	require.ErrorIs(t, f.s.Workflows().Lock(ctx, tid, other.ID, wf.ID), store.ErrNotFound)

	// Actions: one per channel.
	email, err := f.s.Actions().Create(ctx, tid, wf.ID, "email", true)
	require.NoError(t, err)
	_, err = f.s.Actions().Create(ctx, tid, wf.ID, "email", true)
	require.ErrorIs(t, err, store.ErrConflict)
	_, err = f.s.Actions().Create(ctx, tid, wf.ID, "fax", true)
	require.ErrorIs(t, err, store.ErrInvalid)
	sms, err := f.s.Actions().Create(ctx, tid, wf.ID, "sms", false)
	require.NoError(t, err)

	// Templates per locale (upsert).
	_, err = f.s.Templates().Put(ctx, tid, email.ID, "en", map[string]any{"subject": "Hi"})
	require.NoError(t, err)
	tpl, err := f.s.Templates().Put(ctx, tid, email.ID, "en", map[string]any{"subject": "Hello"})
	require.NoError(t, err)
	assert.Equal(t, "Hello", tpl.Content["subject"])
	_, err = f.s.Templates().Put(ctx, tid, email.ID, "hi", map[string]any{"subject": "नमस्ते"})
	require.NoError(t, err)
	n, err := f.s.Templates().Count(ctx, tid, email.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	tpls, err := f.s.Templates().List(ctx, tid, email.ID)
	require.NoError(t, err)
	require.Len(t, tpls, 2)
	assert.Equal(t, "en", tpls[0].Locale)

	byWF, err := f.s.Actions().ListForWorkflows(ctx, tid, []uuid.UUID{wf.ID})
	require.NoError(t, err)
	require.Len(t, byWF[wf.ID], 2)
	assert.Equal(t, []string{"en", "hi"}, byWF[wf.ID][0].Locales)
	assert.Equal(t, []string{}, byWF[wf.ID][1].Locales)

	got, err := f.s.Actions().SetEnabled(ctx, tid, wf.ID, sms.ID, true)
	require.NoError(t, err)
	assert.True(t, got.Enabled)
	require.NoError(t, f.s.Templates().Delete(ctx, tid, email.ID, "hi"))
	require.ErrorIs(t, f.s.Templates().Delete(ctx, tid, email.ID, "hi"), store.ErrNotFound)
	require.NoError(t, f.s.Actions().Delete(ctx, tid, wf.ID, sms.ID))
	require.ErrorIs(t, f.s.Actions().Delete(ctx, tid, wf.ID, sms.ID), store.ErrNotFound)

	// Workflows block deleting their application; deleting the workflow
	// removes its actions and templates.
	require.ErrorIs(t, f.s.Environments().Delete(ctx, tid, g.env.ID), store.ErrConflict) // still has a key (fixture)
	app2, err := f.s.Applications().Create(ctx, tid, store.NewApplication{Name: "Two", Key: "two"})
	require.NoError(t, err)
	wf2, err := f.s.Workflows().Create(ctx, tid, store.NewWorkflow{ApplicationID: app2.ID, Key: "k", Name: "k"})
	require.NoError(t, err)
	require.ErrorIs(t, f.s.Applications().Delete(ctx, tid, app2.ID), store.ErrConflict)
	require.NoError(t, f.s.Workflows().Delete(ctx, tid, app2.ID, wf2.ID))
	require.NoError(t, f.s.Applications().Delete(ctx, tid, app2.ID))

	require.NoError(t, f.s.Workflows().Delete(ctx, tid, g.app.ID, wf.ID))
	_, err = f.s.Templates().Get(ctx, tid, email.ID, "en")
	require.ErrorIs(t, err, store.ErrNotFound, "templates cascade")
	_, err = f.s.Actions().Get(ctx, tid, wf.ID, email.ID)
	require.ErrorIs(t, err, store.ErrNotFound, "actions cascade")
	require.ErrorIs(t, f.s.Workflows().Delete(ctx, tid, g.app.ID, wf.ID), store.ErrNotFound)
}

// TestWorkflowLockSerializes: a second transaction waits for the first's
// lock on the same workflow.
func TestWorkflowLockSerializes(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	g := f.graph(t)
	wf, err := f.s.Workflows().Create(ctx, g.tenant.ID, store.NewWorkflow{ApplicationID: g.app.ID, Key: "k", Name: "k"})
	require.NoError(t, err)

	locked, release, second := make(chan struct{}), make(chan struct{}), make(chan time.Time, 1)
	go func() {
		_ = f.s.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
			require.NoError(t, tx.Workflows().Lock(ctx, g.tenant.ID, g.app.ID, wf.ID))
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	go func() {
		_ = f.s.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
			err := tx.Workflows().Lock(ctx, g.tenant.ID, g.app.ID, wf.ID)
			second <- time.Now()
			return err
		})
	}()
	select {
	case <-second:
		t.Fatal("second lock acquired while the first transaction held it")
	case <-time.After(300 * time.Millisecond):
	}
	released := time.Now()
	close(release)
	assert.False(t, (<-second).Before(released))
}
