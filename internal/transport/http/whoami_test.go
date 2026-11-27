package httptransport

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sbshobhit/notifex/internal/auth"
	"github.com/sbshobhit/notifex/internal/config"
	"github.com/sbshobhit/notifex/internal/crypto"
	"github.com/sbshobhit/notifex/internal/devseed"
	"github.com/sbshobhit/notifex/internal/health"
	"github.com/sbshobhit/notifex/internal/observability"
	"github.com/sbshobhit/notifex/internal/store"
	"github.com/sbshobhit/notifex/internal/store/entstore"
	"github.com/sbshobhit/notifex/internal/store/storetest"
)

func TestWhoAmIEndToEnd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := entstore.New(storetest.DB(t))
	pepper, mk := make([]byte, 32), make([]byte, 32)
	_, _ = rand.Read(pepper)
	_, _ = rand.Read(mk)
	hasher, err := auth.NewHasher(base64.StdEncoding.EncodeToString(pepper))
	require.NoError(t, err)
	kp, err := crypto.NewLocal(mk)
	require.NoError(t, err)
	seed, err := devseed.Run(ctx, st, kp, hasher, devseed.Options{
		TenantSlug: "w-" + uuid.NewString()[:8], OwnerEmail: uuid.NewString()[:8] + "@example.com", AppKey: "shop", Env: store.EnvDev,
	})
	require.NoError(t, err)

	var logs bytes.Buffer
	// The production logger: it adds ctx correlation attrs and redacts keys.
	log := observability.NewLogger(config.Config{Service: "api", Env: "local", LogFormat: "json", LogLevel: slog.LevelDebug}, &logs)
	h := NewRouter(Deps{
		Service: "api", Logger: log, Metrics: prometheus.NewRegistry(), Health: health.NewRegistry(time.Second),
		APIKeys: auth.NewAPIKeyAuthenticator(st, hasher, log, nil),
	})
	call := func(authz string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/whoami", nil)
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	key := seed.APIKey.Plaintext

	t.Run("valid key", func(t *testing.T) {
		rec := call("Bearer " + key)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var body whoamiResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		assert.Equal(t, seed.Tenant.ID.String(), body.TenantID)
		assert.Equal(t, seed.Application.ID.String(), body.ApplicationID)
		assert.Equal(t, seed.Environment.ID.String(), body.EnvironmentID)
		assert.Equal(t, "dev", body.Environment)
		assert.Equal(t, seed.APIKey.Key.Prefix, body.APIKeyPrefix)
		assert.NotNil(t, body.Scopes)
		assert.NotContains(t, rec.Body.String(), key, "response never echoes the key")
	})
	t.Run("scheme is case-insensitive", func(t *testing.T) {
		assert.Equal(t, http.StatusOK, call("bearer "+key).Code)
	})

	for name, authz := range map[string]string{
		"missing header": "",
		"basic scheme":   "Basic " + base64.StdEncoding.EncodeToString([]byte("u:"+key)),
		"empty bearer":   "Bearer ",
		"malformed key":  "Bearer nope",
		"truncated key":  "Bearer " + key[:len(key)-1],
	} {
		t.Run("401 "+name, func(t *testing.T) {
			rec := call(authz)
			require.Equal(t, http.StatusUnauthorized, rec.Code)
			assert.Equal(t, `Bearer realm="notifex"`, rec.Header().Get("WWW-Authenticate"))
			var body errorBody
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Equal(t, "unauthenticated", body.Error.Code)
		})
	}

	t.Run("revoked key is 401 immediately", func(t *testing.T) {
		require.NoError(t, st.APIKeys().Revoke(ctx, seed.Tenant.ID, seed.APIKey.Key.ID, time.Now().Add(-time.Second)))
		assert.Equal(t, http.StatusUnauthorized, call("Bearer "+key).Code)
	})

	secret := key[strings.LastIndexByte(key, '_')+1:]
	assert.NotContains(t, logs.String(), secret, "the key's secret never reaches logs")
	assert.Contains(t, logs.String(), seed.Tenant.ID.String(), "authenticated requests log tenant_id")
}

type unavailableAuth struct{}

func (unavailableAuth) Authenticate(context.Context, string) (auth.Principal, error) {
	return auth.Principal{}, errors.Join(auth.ErrUnavailable, errors.New("db down"))
}

func TestAuthUnavailableIs503(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	h := NewRouter(Deps{Service: "api", Logger: slog.New(slog.NewTextHandler(f.logs, nil)),
		Metrics: prometheus.NewRegistry(), Health: f.health, APIKeys: unavailableAuth{}})
	req := httptest.NewRequest(http.MethodGet, "/v1/whoami", nil)
	req.Header.Set("Authorization", "Bearer x")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "1", rec.Header().Get("Retry-After"))
	assert.NotContains(t, rec.Body.String(), "db down")
}
