package httptransport

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	validator "github.com/pb33f/libopenapi-validator"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ticatwolves/notifex/internal/auth"
	"github.com/ticatwolves/notifex/internal/config"
	"github.com/ticatwolves/notifex/internal/crypto"
	"github.com/ticatwolves/notifex/internal/health"
	"github.com/ticatwolves/notifex/internal/mail"
	"github.com/ticatwolves/notifex/internal/observability"
	"github.com/ticatwolves/notifex/internal/operator"
	"github.com/ticatwolves/notifex/internal/ratelimit"
	"github.com/ticatwolves/notifex/internal/store/entstore"
	"github.com/ticatwolves/notifex/internal/store/storetest"
	"github.com/ticatwolves/notifex/internal/tenant"
)

const (
	strongPW = "plum-orbit-candle-91"
	resetPW  = "glacier-tuba-mosaic-44" // test-only value
)

type consoleEnv struct {
	st   *entstore.Store
	ops  *operator.Service
	h    http.Handler
	mail *mail.Recorder
	logs *bytes.Buffer
	v    validator.Validator
	t    *testing.T
}

func newConsoleEnv(t *testing.T, limits Limits, trusted []string) consoleEnv {
	t.Helper()
	return newConsoleEnvWith(t, limits, trusted, nil)
}

// newConsoleEnvWith optionally enables OAuth via mk, which receives the
// account service the router uses.
func newConsoleEnvWith(t *testing.T, limits Limits, trusted []string, mk func(*auth.Accounts) ConsoleOAuth) consoleEnv {
	t.Helper()
	st := entstore.New(storetest.DB(t))
	var logs bytes.Buffer
	log := observability.NewLogger(config.Config{Service: "api", Env: "local", LogFormat: "json", LogLevel: slog.LevelDebug}, &logs)
	rec := &mail.Recorder{}
	accounts := auth.NewAccounts(st, auth.NewPasswordHasher(4), rec, log, "https://console.test", auth.DefaultSessionPolicy)
	deps := Deps{
		Service: "api", Logger: log, Metrics: prometheus.NewRegistry(), Health: health.NewRegistry(time.Second),
		APIKeys: rejectAllAuth{}, Accounts: accounts, Sessions: auth.NewSessionAuthenticator(st, log, auth.DefaultSessionPolicy),
		Limits: limits, TrustedProxies: trusted,
	}
	if mk != nil {
		deps.OAuth = mk(accounts)
	}
	mk2 := make([]byte, 32)
	_, _ = rand.Read(mk2)
	kp, err := crypto.NewLocal(mk2)
	require.NoError(t, err)
	ts := tenant.NewService(st, kp, rec, log, "https://console.test", tenant.DefaultPolicy)
	pepper := make([]byte, 32)
	_, _ = rand.Read(pepper)
	ts.KeyHasher, err = auth.NewHasher(base64.StdEncoding.EncodeToString(pepper))
	require.NoError(t, err)
	deps.Tenants = ts
	ops := operator.NewService(st, kp, auth.NewPasswordHasher(4), log, operator.DefaultPolicy)
	deps.Operators = ops
	h := NewRouter(deps)
	return consoleEnv{st: st, ops: ops, h: h, mail: rec, logs: &logs, v: specValidator(t), t: t}
}

type call struct {
	method, path, token, remote, xff string
	body                             any
}

func (e consoleEnv) do(c call) *httptest.ResponseRecorder {
	e.t.Helper()
	var rdr *bytes.Reader
	switch b := c.body.(type) {
	case nil:
		rdr = bytes.NewReader(nil)
	case string:
		rdr = bytes.NewReader([]byte(b))
	default:
		raw, err := json.Marshal(b)
		require.NoError(e.t, err)
		rdr = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(c.method, "http://localhost:8080"+c.path, rdr)
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.remote != "" {
		req.RemoteAddr = c.remote
	}
	if c.xff != "" {
		req.Header.Set("X-Forwarded-For", c.xff)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	// Every response must match the contract, success or error.
	validateReq := httptest.NewRequest(c.method, "http://localhost:8080"+c.path, nil)
	if c.token != "" {
		validateReq.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp := rec.Result()
	defer func() { _ = resp.Body.Close() }()
	if ok, errs := e.v.ValidateHttpResponse(validateReq, resp); !ok {
		for _, er := range errs {
			e.t.Errorf("contract: %s %s -> %d: %s", c.method, c.path, rec.Code, er.Error())
		}
	}
	return rec
}

var linkToken = regexp.MustCompile(`#token=([0-9A-Za-z]+)`)

func (e consoleEnv) mailedToken(addr string) string {
	e.t.Helper()
	m, ok := e.mail.Last(addr)
	require.True(e.t, ok)
	match := linkToken.FindStringSubmatch(m.Text)
	require.Len(e.t, match, 2)
	return match[1]
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var b errorBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &b), rec.Body.String())
	return b.Error.Code
}

func newEmail() string {
	return "c" + strings.ReplaceAll(uuid.NewString()[:12], "-", "") + "@example.com"
}

func TestConsoleJourney(t *testing.T) {
	t.Parallel()
	e := newConsoleEnv(t, Limits{}, nil)
	addr := newEmail()

	require.Equal(t, 202, e.do(call{method: "POST", path: "/v1/auth/signup", body: map[string]string{"email": addr, "password": strongPW, "name": "Ada"}}).Code)
	require.Equal(t, 204, e.do(call{method: "POST", path: "/v1/auth/verify-email", body: map[string]string{"token": e.mailedToken(addr)}}).Code)

	rec := e.do(call{method: "POST", path: "/v1/auth/login", body: map[string]string{"email": addr, "password": strongPW}})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var sess sessionCreatedResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &sess))
	require.True(t, strings.HasPrefix(sess.Token, "nfs_"))

	rec = e.do(call{method: "GET", path: "/v1/me", token: sess.Token})
	require.Equal(t, 200, rec.Code)
	assert.Contains(t, rec.Body.String(), `"email_verified":true`)
	assert.Contains(t, rec.Body.String(), `"memberships":[]`)

	rec = e.do(call{method: "GET", path: "/v1/me/sessions", token: sess.Token})
	require.Equal(t, 200, rec.Code)
	assert.Contains(t, rec.Body.String(), `"current":true`)

	require.Equal(t, 204, e.do(call{method: "POST", path: "/v1/auth/reauth", token: sess.Token, body: map[string]string{"password": strongPW}}).Code)

	// A second session, revoked from the first.
	rec = e.do(call{method: "POST", path: "/v1/auth/login", body: map[string]string{"email": addr, "password": strongPW}})
	var other sessionCreatedResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &other))
	require.Equal(t, 204, e.do(call{method: "DELETE", path: "/v1/me/sessions/" + other.SessionID, token: sess.Token}).Code)
	assert.Equal(t, 401, e.do(call{method: "GET", path: "/v1/me", token: other.Token}).Code)

	require.Equal(t, 204, e.do(call{method: "POST", path: "/v1/auth/logout", token: sess.Token}).Code)
	assert.Equal(t, 401, e.do(call{method: "GET", path: "/v1/me", token: sess.Token}).Code)

	// Forgot/reset over HTTP.
	require.Equal(t, 202, e.do(call{method: "POST", path: "/v1/auth/password/forgot", body: map[string]string{"email": addr}}).Code)
	require.Equal(t, 204, e.do(call{method: "POST", path: "/v1/auth/password/reset", body: map[string]string{"token": e.mailedToken(addr), "password": resetPW}}).Code)

	// Secrets never reach logs.
	for _, secret := range []string{strongPW, resetPW, sess.Token, other.Token} {
		assert.NotContains(t, e.logs.String(), secret)
	}
	assert.NotContains(t, e.logs.String(), addr, "emails are masked in logs")
}

func TestConsoleResponsesDoNotRevealAccounts(t *testing.T) {
	t.Parallel()
	e := newConsoleEnv(t, Limits{}, nil)
	existing := newEmail()
	e.do(call{method: "POST", path: "/v1/auth/signup", body: map[string]string{"email": existing, "password": strongPW}})

	a := e.do(call{method: "POST", path: "/v1/auth/signup", body: map[string]string{"email": existing, "password": strongPW}})
	b := e.do(call{method: "POST", path: "/v1/auth/signup", body: map[string]string{"email": newEmail(), "password": strongPW}})
	assert.Equal(t, a.Code, b.Code)
	assert.Equal(t, a.Body.String(), b.Body.String())

	a = e.do(call{method: "POST", path: "/v1/auth/password/forgot", body: map[string]string{"email": existing}})
	b = e.do(call{method: "POST", path: "/v1/auth/password/forgot", body: map[string]string{"email": newEmail()}})
	assert.Equal(t, a.Code, b.Code)
	assert.Equal(t, a.Body.String(), b.Body.String())

	a = e.do(call{method: "POST", path: "/v1/auth/login", body: map[string]string{"email": existing, "password": "wrong-password-123"}})
	b = e.do(call{method: "POST", path: "/v1/auth/login", body: map[string]string{"email": newEmail(), "password": "wrong-password-123"}})
	assert.Equal(t, 401, a.Code)
	assert.Equal(t, a.Code, b.Code)
	assert.Equal(t, errCode(t, a), errCode(t, b))
}

func TestConsoleErrorMapping(t *testing.T) {
	t.Parallel()
	e := newConsoleEnv(t, Limits{}, nil)
	addr := newEmail()
	e.do(call{method: "POST", path: "/v1/auth/signup", body: map[string]string{"email": addr, "password": strongPW}})

	tests := []struct {
		name   string
		c      call
		status int
		code   string
	}{
		{"unknown field", call{method: "POST", path: "/v1/auth/login", body: `{"email":"a@b.co","password":"x","is_admin":true}`}, 400, "validation_failed"},
		{"not json", call{method: "POST", path: "/v1/auth/login", body: `nope`}, 400, "validation_failed"},
		{"two objects", call{method: "POST", path: "/v1/auth/login", body: `{"email":"a@b.co","password":"x"}{}`}, 400, "validation_failed"},
		{"empty body", call{method: "POST", path: "/v1/auth/signup", body: ``}, 400, "validation_failed"},
		{"weak password", call{method: "POST", path: "/v1/auth/signup", body: map[string]string{"email": newEmail(), "password": "password1234"}}, 400, "validation_failed"},
		{"bad email", call{method: "POST", path: "/v1/auth/signup", body: map[string]string{"email": "nope", "password": strongPW}}, 400, "validation_failed"},
		{"bad token", call{method: "POST", path: "/v1/auth/verify-email", body: map[string]string{"token": strings.Repeat("a", 43)}}, 400, "invalid_token"},
		{"wrong password", call{method: "POST", path: "/v1/auth/login", body: map[string]string{"email": addr, "password": "wrong-password-123"}}, 401, "invalid_credentials"},
		{"no session", call{method: "GET", path: "/v1/me"}, 401, "unauthenticated"},
		{"api key on console", call{method: "GET", path: "/v1/me", token: "nfx_dev_abcdefghijkl_" + strings.Repeat("a", 49)}, 401, "unauthenticated"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := e.do(tt.c)
			require.Equal(t, tt.status, rec.Code, rec.Body.String())
			assert.Equal(t, tt.code, errCode(t, rec))
		})
	}
}

func TestSessionsNeverAuthorizeTheDataPlane(t *testing.T) {
	t.Parallel()
	st := entstore.New(storetest.DB(t))
	log := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	rec := &mail.Recorder{}
	accounts := auth.NewAccounts(st, auth.NewPasswordHasher(2), rec, log, "https://console.test", auth.DefaultSessionPolicy)
	ctx := context.Background()
	addr := newEmail()
	require.NoError(t, accounts.Signup(ctx, auth.SignupInput{Email: addr, Password: strongPW}))
	s, err := accounts.Login(ctx, auth.LoginInput{Email: addr, Password: strongPW})
	require.NoError(t, err)

	pepper := make([]byte, 32)
	_, _ = rand.Read(pepper)
	hasher, err := auth.NewHasher(base64.StdEncoding.EncodeToString(pepper))
	require.NoError(t, err)
	h := NewRouter(Deps{Service: "api", Logger: log, Metrics: prometheus.NewRegistry(), Health: health.NewRegistry(time.Second),
		APIKeys: auth.NewAPIKeyAuthenticator(st, hasher, log, nil)})
	req := httptest.NewRequest("GET", "/v1/whoami", nil)
	req.Header.Set("Authorization", "Bearer "+s.Token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	assert.Equal(t, 401, w.Code, "a console session is not an API key")
}

func TestLoginBackoffOverHTTP(t *testing.T) {
	t.Parallel()
	e := newConsoleEnv(t, Limits{}, nil)
	addr := newEmail()
	e.do(call{method: "POST", path: "/v1/auth/signup", body: map[string]string{"email": addr, "password": strongPW}})
	for range 5 {
		e.do(call{method: "POST", path: "/v1/auth/login", body: map[string]string{"email": addr, "password": "wrong-password-123"}})
	}
	rec := e.do(call{method: "POST", path: "/v1/auth/login", body: map[string]string{"email": addr, "password": strongPW}})
	require.Equal(t, 429, rec.Code)
	assert.Equal(t, "account_locked", errCode(t, rec))
	assert.NotEmpty(t, rec.Header().Get("Retry-After"))
}

func TestPerIPRateLimits(t *testing.T) {
	t.Parallel()
	limits := Limits{Login: ratelimit.New(2, 2, 100), Credential: ratelimit.New(1, 1, 100), APIKeyFail: ratelimit.New(3, 3, 100)}
	e := newConsoleEnv(t, limits, nil)
	login := func(remote, xff string) int {
		return e.do(call{method: "POST", path: "/v1/auth/login", remote: remote, xff: xff, body: map[string]string{"email": newEmail(), "password": "wrong-password-123"}}).Code
	}
	assert.Equal(t, 401, login("198.51.100.7:1111", ""))
	assert.Equal(t, 401, login("198.51.100.7:2222", ""))
	assert.Equal(t, 429, login("198.51.100.7:3333", ""), "third attempt from the same IP in a minute")
	assert.Equal(t, 429, login("198.51.100.7:4444", "203.0.113.9"), "a spoofed X-Forwarded-For doesn't help without a trusted proxy")
	assert.Equal(t, 401, login("198.51.100.8:1111", ""), "other IPs unaffected")

	// Failed API-key attempts are limited per IP too (threat model R1).
	whoami := func() int {
		req := httptest.NewRequest("GET", "/v1/whoami", nil)
		req.RemoteAddr = "198.51.100.20:5555"
		req.Header.Set("Authorization", "Bearer nope")
		w := httptest.NewRecorder()
		e.h.ServeHTTP(w, req)
		return w.Code
	}
	for range 3 {
		assert.Equal(t, 401, whoami())
	}
	assert.Equal(t, 429, whoami())
}

func TestTrustedProxyForwardedFor(t *testing.T) {
	t.Parallel()
	e := newConsoleEnv(t, Limits{Login: ratelimit.New(1, 1, 100), Credential: ratelimit.New(1, 1, 100), APIKeyFail: ratelimit.New(1, 1, 100)}, []string{"192.0.2.0/24"})
	login := func(xff string) int {
		return e.do(call{method: "POST", path: "/v1/auth/login", remote: "192.0.2.10:443", xff: xff, body: map[string]string{"email": newEmail(), "password": "wrong-password-123"}}).Code
	}
	assert.Equal(t, 401, login("203.0.113.1"))
	assert.Equal(t, 401, login("203.0.113.2"), "behind the trusted ALB, each client IP has its own budget")
	assert.Equal(t, 429, login("203.0.113.1"))
}

// rejectAllAuth rejects every API key, as for unknown credentials.
type rejectAllAuth struct{}

func (rejectAllAuth) Authenticate(context.Context, string) (auth.Principal, error) {
	return auth.Principal{}, auth.ErrUnauthenticated
}
