package httptransport

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pb33f/libopenapi"
	validator "github.com/pb33f/libopenapi-validator"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/ticatwolves/notifex/api"
	"github.com/ticatwolves/notifex/internal/auth"
	"github.com/ticatwolves/notifex/internal/crypto"
	"github.com/ticatwolves/notifex/internal/devseed"
	"github.com/ticatwolves/notifex/internal/health"
	"github.com/ticatwolves/notifex/internal/store"
	"github.com/ticatwolves/notifex/internal/store/entstore"
	"github.com/ticatwolves/notifex/internal/store/storetest"
)

func specValidator(t *testing.T) validator.Validator {
	t.Helper()
	doc, err := libopenapi.NewDocument(api.OpenAPI)
	require.NoError(t, err)
	v, errs := validator.NewValidator(doc)
	require.Empty(t, errs)
	ok, verrs := v.ValidateDocument()
	for _, e := range verrs {
		t.Errorf("spec: %s", e.Error())
	}
	require.True(t, ok, "api/openapi.yaml must be a valid OpenAPI 3.1 document")
	return v
}

// assertConforms validates a real response against the spec operation.
func assertConforms(t *testing.T, v validator.Validator, req *http.Request, rec *httptest.ResponseRecorder) {
	t.Helper()
	resp := rec.Result()
	t.Cleanup(func() { _ = resp.Body.Close() })
	ok, errs := v.ValidateHttpResponse(req, resp)
	for _, e := range errs {
		t.Errorf("%s %s -> %d: %s", req.Method, req.URL.Path, rec.Code, e.Error())
	}
	assert.True(t, ok)
}

// TestImplementedOperationsConformToSpec sends real requests through the real
// router (real auth, real Postgres) and validates every response against
// api/openapi.yaml.
func TestImplementedOperationsConformToSpec(t *testing.T) {
	t.Parallel()
	v := specValidator(t)
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
		TenantSlug: "c-" + uuid.NewString()[:8], OwnerEmail: uuid.NewString()[:8] + "@example.com", AppKey: "shop", Env: store.EnvStaging,
	})
	require.NoError(t, err)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hr := health.NewRegistry(time.Second)
	router := NewRouter(Deps{Service: "api", Logger: log, Metrics: prometheus.NewRegistry(), Health: hr,
		APIKeys: auth.NewAPIKeyAuthenticator(st, hasher, log, nil)})
	unavailable := NewRouter(Deps{Service: "api", Logger: log, Metrics: prometheus.NewRegistry(), Health: hr,
		APIKeys: unavailableAuth{}})

	tests := []struct {
		name   string
		h      http.Handler
		path   string
		authz  string
		status int
	}{
		{"healthz 200", router, "/healthz", "", 200},
		{"readyz 200", router, "/readyz", "", 200},
		{"whoami 200", router, "/v1/whoami", "Bearer " + seed.APIKey.Plaintext, 200},
		{"whoami 401", router, "/v1/whoami", "Bearer nope", 401},
		{"whoami 503", unavailable, "/v1/whoami", "Bearer x", 503},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://localhost:8080"+tt.path, nil)
			if tt.authz != "" {
				req.Header.Set("Authorization", tt.authz)
			}
			rec := httptest.NewRecorder()
			tt.h.ServeHTTP(rec, req)
			require.Equal(t, tt.status, rec.Code, rec.Body.String())
			assertConforms(t, v, req, rec)
		})
	}

	t.Run("readyz 503 while draining", func(t *testing.T) {
		draining := health.NewRegistry(time.Second)
		draining.SetDraining()
		h := NewRouter(Deps{Service: "api", Logger: log, Metrics: prometheus.NewRegistry(), Health: draining})
		req := httptest.NewRequest(http.MethodGet, "http://localhost:8080/readyz", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		require.Equal(t, 503, rec.Code)
		assertConforms(t, v, req, rec)
	})
}

// TestValidatorRejectsNonConformingResponses proves the contract test has
// teeth: a whoami body with a raw UUID or an extra field must fail.
func TestValidatorRejectsNonConformingResponses(t *testing.T) {
	t.Parallel()
	v := specValidator(t)
	bad := map[string]string{
		"raw uuid id":   `{"tenant_id":"` + uuid.NewString() + `","application_id":"app_0000000000000000000001","environment_id":"env_0000000000000000000001","environment":"dev","api_key_id":"key_0000000000000000000001","api_key_prefix":"abcdefghijkl","scopes":[]}`,
		"extra field":   `{"tenant_id":"ten_0000000000000000000001","application_id":"app_0000000000000000000001","environment_id":"env_0000000000000000000001","environment":"dev","api_key_id":"key_0000000000000000000001","api_key_prefix":"abcdefghijkl","scopes":[],"secret":"x"}`,
		"missing field": `{"tenant_id":"ten_0000000000000000000001"}`,
	}
	for name, body := range bad {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://localhost:8080/v1/whoami", nil)
			req.Header.Set("Authorization", "Bearer x")
			rec := httptest.NewRecorder()
			rec.Header().Set("Content-Type", "application/json")
			rec.WriteHeader(200)
			_, _ = rec.WriteString(body)
			resp := rec.Result()
			defer func() { _ = resp.Body.Close() }()
			ok, _ := v.ValidateHttpResponse(req, resp)
			assert.False(t, ok, "validator accepted a non-conforming body")
		})
	}
}

// TestSpecAndRouterDoNotDrift: every served route must be in the spec as
// `implemented`, and every `implemented` operation must be served.
func TestSpecAndRouterDoNotDrift(t *testing.T) {
	t.Parallel()
	var spec struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	require.NoError(t, yaml.Unmarshal(api.OpenAPI, &spec))

	httpMethods := map[string]bool{"get": true, "put": true, "post": true, "delete": true, "options": true, "head": true, "patch": true, "trace": true}
	implemented, all := map[string]bool{}, map[string]string{}
	for path, ops := range spec.Paths {
		for method, node := range ops {
			if !httpMethods[method] {
				continue // path-level keys such as `parameters`
			}
			var op struct {
				Status string `yaml:"x-notifex-status"`
			}
			require.NoError(t, node.Decode(&op))
			key := strings.ToUpper(method) + " " + path
			all[key] = op.Status
			require.Contains(t, []string{"implemented", "draft"}, op.Status, "%s needs x-notifex-status", key)
			if op.Status == "implemented" {
				implemented[key] = true
			}
		}
	}

	// Every optional route group enabled, so the router serves everything it can.
	r := NewRouter(Deps{Service: "api", Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics: prometheus.NewRegistry(), Health: health.NewRegistry(time.Second), APIKeys: unavailableAuth{},
		Accounts: stubAccounts{}, Sessions: stubSessions{}, OAuth: stubOAuth{}, Tenants: stubTenants{},
		Operators: stubOperators{}})
	param := regexp.MustCompile(`:([a-z_]+)`)
	served := map[string]bool{}
	for _, rt := range r.(*gin.Engine).Routes() {
		served[rt.Method+" "+param.ReplaceAllString(rt.Path, "{$1}")] = true
	}

	var missing, unserved []string
	for k := range served {
		if all[k] != "implemented" {
			missing = append(missing, k+" (status: "+all[k]+")")
		}
	}
	for k := range implemented {
		if !served[k] {
			unserved = append(unserved, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(unserved)
	assert.Empty(t, missing, "served routes missing from the spec or not marked implemented")
	assert.Empty(t, unserved, "spec marks these implemented but the router does not serve them")
}

// stubAccounts and stubSessions satisfy the console interfaces for route
// registration; drift tests never call them.
type stubAccounts struct{ ConsoleAccounts }

type stubSessions struct{ SessionAuthenticator }

type stubOAuth struct{ ConsoleOAuth }

type stubTenants struct{ TenantAdmin }

type stubOperators struct{ OperatorRealm }
