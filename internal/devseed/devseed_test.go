package devseed_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ticatwolves/notifex/internal/auth"
	"github.com/ticatwolves/notifex/internal/crypto"
	"github.com/ticatwolves/notifex/internal/devseed"
	"github.com/ticatwolves/notifex/internal/store"
	"github.com/ticatwolves/notifex/internal/store/entstore"
	"github.com/ticatwolves/notifex/internal/store/storetest"
)

// TestSeedIsIdempotentAndTriggerable: two runs share one tenant and one
// active workflow with email and SMS actions; each run issues a new key.
func TestSeedIsIdempotentAndTriggerable(t *testing.T) {
	t.Parallel()
	mk := make([]byte, 32)
	_, _ = rand.Read(mk)
	kp, err := crypto.NewLocal(mk)
	require.NoError(t, err)
	pepper := make([]byte, 32)
	_, _ = rand.Read(pepper)
	h, err := auth.NewHasher(base64.StdEncoding.EncodeToString(pepper))
	require.NoError(t, err)
	st := entstore.New(storetest.DB(t))
	ctx := context.Background()
	o := devseed.Options{TenantSlug: "seed-test", OwnerEmail: "seed-owner@example.com", AppKey: "shop", Env: store.EnvDev}

	a, err := devseed.Run(ctx, st, kp, h, o)
	require.NoError(t, err)
	b, err := devseed.Run(ctx, st, kp, h, o)
	require.NoError(t, err)
	assert.Equal(t, a.Tenant.ID, b.Tenant.ID)
	assert.Equal(t, a.Workflow.ID, b.Workflow.ID, "one workflow across runs")
	assert.NotEqual(t, a.APIKey.Plaintext, b.APIKey.Plaintext)
	assert.Equal(t, devseed.WorkflowKey, b.Workflow.Key)
	assert.Equal(t, store.WorkflowActive, b.Workflow.Status)

	acts, err := st.Actions().ListForWorkflows(ctx, b.Tenant.ID, []uuid.UUID{b.Workflow.ID})
	require.NoError(t, err)
	chans := map[string]bool{}
	for _, x := range acts[b.Workflow.ID] {
		chans[x.Channel] = true
	}
	assert.Equal(t, map[string]bool{"email": true, "sms": true}, chans)
	ten, err := st.Tenants().Get(ctx, b.Tenant.ID)
	require.NoError(t, err)
	assert.Equal(t, devseed.SeedSMSDailyLimit, ten.PlatformSMSDailyLimit)
}
