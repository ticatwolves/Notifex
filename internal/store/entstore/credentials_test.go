package entstore

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ticatwolves/notifex/internal/store"
	"github.com/ticatwolves/notifex/internal/store/storetest"
)

func newCred(appID, envID *uuid.UUID, channel string) store.NewChannelCredential {
	return store.NewChannelCredential{
		ID: uuid.Must(uuid.NewV7()), ApplicationID: appID, EnvironmentID: envID, Channel: channel,
		Provider: "sendgrid", Mode: "byo", Name: "c", Config: map[string]any{"from_email": "a@b.test"},
		SecretCiphertext: []byte("ciphertext"), SecretHint: "abcd",
	}
}

func TestCredentialStore(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	g := f.graph(t)
	tid := g.tenant.ID
	cs := f.s.Credentials()

	tenantLevel, err := cs.Create(ctx, tid, newCred(nil, nil, "email"))
	require.NoError(t, err)
	assert.Equal(t, "pending", tenantLevel.Status)
	assert.Equal(t, []byte("ciphertext"), tenantLevel.SecretCiphertext)
	appLevel, err := cs.Create(ctx, tid, newCred(&g.app.ID, nil, "email"))
	require.NoError(t, err, "app scope is separate from tenant scope")
	envLevel, err := cs.Create(ctx, tid, newCred(&g.app.ID, &g.env.ID, "email"))
	require.NoError(t, err, "env scope is separate from app scope")
	_, err = cs.Create(ctx, tid, newCred(nil, nil, "sms"))
	require.NoError(t, err, "other channel")

	for name, in := range map[string]store.NewChannelCredential{
		"tenant": newCred(nil, nil, "email"), "app": newCred(&g.app.ID, nil, "email"), "env": newCred(&g.app.ID, &g.env.ID, "email"),
	} {
		_, err = cs.Create(ctx, tid, in)
		require.ErrorIs(t, err, store.ErrConflict, "second active credential at %s scope", name)
	}
	_, err = cs.Create(ctx, tid, newCred(nil, &g.env.ID, "email"))
	require.ErrorIs(t, err, store.ErrInvalid, "env without app")
	_, err = cs.Create(ctx, tid, newCred(nil, nil, "fax"))
	require.ErrorIs(t, err, store.ErrInvalid)
	unknown := uuid.Must(uuid.NewV7())
	_, err = cs.Create(ctx, tid, newCred(&unknown, nil, "email"))
	require.ErrorIs(t, err, store.ErrNotFound, "unknown application")

	// A disabled credential frees its slot; re-enabling then conflicts.
	disabled := store.CredentialDisabled
	_, err = cs.Update(ctx, tid, tenantLevel.ID, store.CredentialPatch{Status: &disabled})
	require.NoError(t, err)
	replacement, err := cs.Create(ctx, tid, newCred(nil, nil, "email"))
	require.NoError(t, err)
	pending := store.CredentialPending
	_, err = cs.Update(ctx, tid, tenantLevel.ID, store.CredentialPatch{Status: &pending})
	require.ErrorIs(t, err, store.ErrConflict)

	// Scope lookup ignores disabled rows and other applications.
	app2, err := f.s.Applications().Create(ctx, tid, store.NewApplication{Name: "Two", Key: "two"})
	require.NoError(t, err)
	_, err = cs.Create(ctx, tid, newCred(&app2.ID, nil, "email"))
	require.NoError(t, err)
	active, err := cs.ActiveForScope(ctx, tid, "email", g.app.ID, g.env.ID)
	require.NoError(t, err)
	got := map[uuid.UUID]bool{}
	for _, c := range active {
		got[c.ID] = true
	}
	assert.Equal(t, map[uuid.UUID]bool{replacement.ID: true, appLevel.ID: true, envLevel.ID: true}, got)

	now := time.Now().UTC().Truncate(time.Microsecond)
	rot, err := cs.Rotate(ctx, tid, envLevel.ID, []byte("new-ciphertext"), "wxyz", now)
	require.NoError(t, err)
	assert.Equal(t, []byte("new-ciphertext"), rot.SecretCiphertext)
	assert.Equal(t, "wxyz", rot.SecretHint)
	assert.Equal(t, now, rot.RotatedAt.UTC())

	list, err := cs.List(ctx, tid, ptr("email"), &g.app.ID, store.Page{})
	require.NoError(t, err)
	assert.Len(t, list, 2)

	// Credentials block deleting their environment and application.
	require.ErrorIs(t, f.s.Applications().Delete(ctx, tid, app2.ID), store.ErrConflict)

	require.NoError(t, cs.Delete(ctx, tid, envLevel.ID))
	require.ErrorIs(t, cs.Delete(ctx, tid, envLevel.ID), store.ErrNotFound)

	// The database enforces scope shape and size, whoever writes.
	db := storetest.DB(t)
	_, err = db.ExecContext(ctx, `UPDATE channel_credentials SET secret_ciphertext = $1 WHERE id = $2`, make([]byte, 20001), appLevel.ID)
	require.ErrorContains(t, err, "channel_credentials_secret_size")
}

// TestCredentialUniqueUnderConcurrency: racing creates for one scope and
// channel produce exactly one credential.
func TestCredentialUniqueUnderConcurrency(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	g := f.graph(t)
	var ok, conflict atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			_, err := f.s.Credentials().Create(ctx, g.tenant.ID, newCred(&g.app.ID, nil, "sms"))
			switch {
			case err == nil:
				ok.Add(1)
			case assert.ErrorIs(t, err, store.ErrConflict):
				conflict.Add(1)
			}
		})
	}
	wg.Wait()
	assert.Equal(t, int32(1), ok.Load())
	assert.Equal(t, int32(7), conflict.Load())
}
