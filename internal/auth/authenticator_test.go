package auth_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/google/uuid"

	"github.com/ticatwolves/notifex/internal/audit"
	"github.com/ticatwolves/notifex/internal/auth"
	"github.com/ticatwolves/notifex/internal/crypto"
	"github.com/ticatwolves/notifex/internal/devseed"
	"github.com/ticatwolves/notifex/internal/store"
	"github.com/ticatwolves/notifex/internal/store/entstore"
	"github.com/ticatwolves/notifex/internal/store/storetest"
)

type env struct {
	st     *entstore.Store
	hasher *auth.Hasher
	kp     *crypto.Local
	seed   devseed.Result
	logs   *bytes.Buffer
	reg    *prometheus.Registry
	authn  *auth.APIKeyAuthenticator
}

func setup(t *testing.T) env {
	t.Helper()
	e := env{st: entstore.New(storetest.DB(t)), logs: &bytes.Buffer{}, reg: prometheus.NewRegistry()}
	pepper := make([]byte, 32)
	_, _ = rand.Read(pepper)
	var err error
	e.hasher, err = auth.NewHasher(base64.StdEncoding.EncodeToString(pepper))
	require.NoError(t, err)
	mk := make([]byte, 32)
	_, _ = rand.Read(mk)
	e.kp, err = crypto.NewLocal(mk)
	require.NoError(t, err)
	e.seed, err = devseed.Run(context.Background(), e.st, e.kp, e.hasher, devseed.Options{
		TenantSlug: "t-" + uuid.NewString()[:8], OwnerEmail: uuid.NewString()[:8] + "@example.com",
		AppKey: "shop", Env: store.EnvProd,
	})
	require.NoError(t, err)
	e.authn = auth.NewAPIKeyAuthenticator(e.st, e.hasher, slog.New(slog.NewJSONHandler(e.logs, nil)), e.reg)
	return e
}

func (e env) issue(t *testing.T, in auth.IssueInput) auth.Issued {
	t.Helper()
	if in.ApplicationID == uuid.Nil {
		in.ApplicationID, in.EnvironmentID = e.seed.Application.ID, e.seed.Environment.ID
	}
	if in.Name == "" {
		in.Name = "test"
	}
	k, err := auth.IssueAPIKey(context.Background(), e.st, e.hasher, audit.System, e.seed.Tenant.ID, in)
	require.NoError(t, err)
	return k
}

func TestAuthenticateValidKey(t *testing.T) {
	t.Parallel()
	e := setup(t)
	ctx := context.Background()
	p, err := e.authn.Authenticate(ctx, e.seed.APIKey.Plaintext)
	require.NoError(t, err)
	assert.Equal(t, e.seed.Tenant.ID, p.TenantID)
	assert.Equal(t, e.seed.Application.ID, p.ApplicationID)
	assert.Equal(t, e.seed.Environment.ID, p.EnvironmentID)
	assert.Equal(t, store.EnvProd, p.Environment)
	assert.Equal(t, e.seed.APIKey.Key.ID, p.APIKeyID)

	k, err := e.st.APIKeys().GetByPrefix(ctx, e.seed.APIKey.Key.Prefix)
	require.NoError(t, err)
	assert.NotNil(t, k.LastUsedAt, "last_used_at recorded")
	assert.InDelta(t, 1, counter(t, e.reg, "ok"), 0)
}

func TestAuthenticateRejections(t *testing.T) {
	t.Parallel()
	e := setup(t)
	ctx := context.Background()
	good := e.seed.APIKey.Plaintext
	parts := strings.Split(good, "_")

	// Valid format and checksum, but a secret that was never issued.
	forged, _, err := auth.Generate("prod")
	require.NoError(t, err)
	fp := strings.Split(forged, "_")
	wrongSecret := rebuild("prod", parts[2], fp[3][:43])

	// Environment in the key text doesn't match the key's environment record.
	envMismatch := rebuild("dev", parts[2], parts[3][:43])

	revoked := e.issue(t, auth.IssueInput{})
	require.NoError(t, e.st.APIKeys().Revoke(ctx, e.seed.Tenant.ID, revoked.Key.ID, time.Now().Add(-time.Second)))
	past := time.Now().Add(-time.Minute)
	expired := e.issue(t, auth.IssueInput{ExpiresAt: &past})

	tests := []struct {
		name, key, reason string
	}{
		{"empty", "", "malformed"},
		{"garbage", "hello", "malformed"},
		{"typo (checksum)", good[:len(good)-1] + flip(good[len(good)-1]), "malformed"},
		{"unknown prefix", forged, "unknown_prefix"},
		{"wrong secret", wrongSecret, "bad_secret"},
		// env isn't hashed, so this passes the secret check and is caught by
		// comparing against the environment record.
		{"environment mismatch", envMismatch, "environment_mismatch"},
		{"revoked", revoked.Plaintext, "revoked"},
		{"expired", expired.Plaintext, "expired"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := e.authn.Authenticate(ctx, tt.key)
			require.ErrorIs(t, err, auth.ErrUnauthenticated)
			assert.NotErrorIs(t, err, auth.ErrUnavailable)
		})
	}
	for _, tt := range tests {
		assert.Positive(t, counter(t, e.reg, tt.reason), tt.reason)
	}
	assert.NotContains(t, e.logs.String(), parts[3], "secrets never logged")
}

func TestAuthenticateInactiveTenant(t *testing.T) {
	t.Parallel()
	e := setup(t)
	ctx := context.Background()
	s := store.TenantSuspended
	_, err := e.st.Tenants().Update(ctx, e.seed.Tenant.ID, store.TenantPatch{Status: &s})
	require.NoError(t, err)
	_, err = e.authn.Authenticate(ctx, e.seed.APIKey.Plaintext)
	require.ErrorIs(t, err, auth.ErrUnauthenticated)
	assert.InDelta(t, 1, counter(t, e.reg, "tenant_inactive"), 0)
}

// failingStore makes every lookup fail as if the database were down.
type failingStore struct{ store.Store }

func (failingStore) APIKeys() store.APIKeyStore { return failingKeys{} }

type failingKeys struct{ store.APIKeyStore }

func (failingKeys) GetByPrefix(context.Context, string) (store.APIKey, error) {
	return store.APIKey{}, errors.New("connection refused")
}

func TestAuthenticateUnavailableIsNot401(t *testing.T) {
	t.Parallel()
	e := setup(t)
	a := auth.NewAPIKeyAuthenticator(failingStore{e.st}, e.hasher, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), nil)
	_, err := a.Authenticate(context.Background(), e.seed.APIKey.Plaintext)
	require.ErrorIs(t, err, auth.ErrUnavailable)
	assert.NotErrorIs(t, err, auth.ErrUnauthenticated)
}

func TestIssueAPIKey(t *testing.T) {
	t.Parallel()
	e := setup(t)
	ctx := context.Background()

	// Environment belongs to a different application: refused.
	other, err := e.st.Applications().Create(ctx, e.seed.Tenant.ID, store.NewApplication{Name: "Other", Key: "other"})
	require.NoError(t, err)
	_, err = auth.IssueAPIKey(ctx, e.st, e.hasher, audit.System, e.seed.Tenant.ID, auth.IssueInput{
		ApplicationID: other.ID, EnvironmentID: e.seed.Environment.ID, Name: "x",
	})
	require.ErrorIs(t, err, store.ErrNotFound)

	// Another tenant's environment: refused.
	f2 := setup(t)
	_, err = auth.IssueAPIKey(ctx, e.st, e.hasher, audit.System, f2.seed.Tenant.ID, auth.IssueInput{
		ApplicationID: e.seed.Application.ID, EnvironmentID: e.seed.Environment.ID, Name: "x",
	})
	require.ErrorIs(t, err, store.ErrNotFound)

	k := e.issue(t, auth.IssueInput{Name: "ci", Scopes: []string{"trigger"}})
	assert.True(t, strings.HasPrefix(k.Plaintext, "nfx_prod_"+k.Key.Prefix+"_"))
	assert.NotEqual(t, k.Plaintext, string(k.Key.Hash), "only the hash is stored")

	events, err := e.st.Audit().List(ctx, e.seed.Tenant.ID, store.Page{Limit: store.MaxPageSize})
	require.NoError(t, err)
	var actions []string
	for _, ev := range events {
		actions = append(actions, ev.Action)
		assert.NotContains(t, ev.After, "hash")
		for _, v := range ev.After {
			if s, ok := v.(string); ok {
				assert.NotContains(t, s, "nfx_", "plaintext key never in audit")
			}
		}
	}
	for _, want := range []string{"tenant.create", "membership.create", "application.create", "environment.create", "api_key.create"} {
		assert.Contains(t, actions, want)
	}
}

func TestSeedIsIdempotent(t *testing.T) {
	t.Parallel()
	e := setup(t)
	ctx := context.Background()
	again, err := devseed.Run(ctx, e.st, e.kp, e.hasher, devseed.Options{
		TenantSlug: e.seed.Tenant.Slug, OwnerEmail: e.seed.Owner.Email, AppKey: "shop", Env: store.EnvProd,
	})
	require.NoError(t, err)
	assert.Equal(t, e.seed.Tenant.ID, again.Tenant.ID)
	assert.Equal(t, e.seed.Application.ID, again.Application.ID)
	assert.Equal(t, e.seed.Environment.ID, again.Environment.ID)
	assert.Equal(t, e.seed.Owner.ID, again.Owner.ID)
	assert.NotEqual(t, e.seed.APIKey.Key.ID, again.APIKey.Key.ID, "each run issues a new key")
	for _, k := range []string{e.seed.APIKey.Plaintext, again.APIKey.Plaintext} {
		_, err := e.authn.Authenticate(ctx, k)
		require.NoError(t, err)
	}
	// The seeded tenant's DEK works with the cipher (bound to the tenant id).
	c := crypto.NewCipher(e.kp, e.st.Tenants(), crypto.CipherOptions{})
	_, err = c.Encrypt(ctx, crypto.AAD{TenantID: e.seed.Tenant.ID.UUID(), RowID: uuid.New(), Column: "payload"}, []byte("x"))
	require.NoError(t, err)
}

func rebuild(env, prefix, secret string) string { return auth.RebuildForTest(env, prefix, secret) }

func flip(b byte) string {
	if b == 'a' {
		return "b"
	}
	return "a"
}

func counter(t *testing.T, reg *prometheus.Registry, result string) float64 {
	t.Helper()
	fams, err := reg.Gather()
	require.NoError(t, err)
	for _, f := range fams {
		if f.GetName() != "notifex_auth_api_key_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "result" && l.GetValue() == result {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}
