package httptransport

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ticatwolves/notifex/internal/operator"
)

// operatorLogin provisions an operator and returns its nfo_ token.
func (e consoleEnv) operatorLogin() string {
	e.t.Helper()
	addr := "op" + newEmail()
	_, uri, err := e.ops.Create(context.Background(), addr, "Ops", strongPW)
	require.NoError(e.t, err)
	code, err := operator.CodeFromURI(uri, time.Now())
	require.NoError(e.t, err)
	rec := e.do(call{method: "POST", path: "/v1/operator/auth/login", body: map[string]string{"email": addr, "password": strongPW, "totp": code}})
	require.Equal(e.t, 200, rec.Code, rec.Body.String())
	var s sessionCreatedResponse
	require.NoError(e.t, json.Unmarshal(rec.Body.Bytes(), &s))
	require.True(e.t, strings.HasPrefix(s.Token, "nfo_"))
	return s.Token
}

func TestOperatorJourney(t *testing.T) {
	t.Parallel()
	e := newConsoleEnv(t, Limits{}, nil)
	_, owner := e.account()
	tid := e.createTenant(owner)
	op := e.operatorLogin()

	rec := e.do(call{method: "GET", path: "/v1/operator/tenants/" + tid, token: op})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"sandbox":true`)

	rec = e.do(call{method: "GET", path: "/v1/operator/tenants?limit=200", token: op})
	require.Equal(t, 200, rec.Code)
	assert.Contains(t, rec.Body.String(), tid)

	rec = e.do(call{method: "PATCH", path: "/v1/operator/tenants/" + tid, token: op,
		body: map[string]any{"sandbox": false, "platform_sms_daily_limit": 250, "reason": "design partner"}})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"sandbox":false`)
	assert.Contains(t, rec.Body.String(), `"platform_sms_daily_limit":250`)

	// Suspension takes effect for the tenant's own console view.
	rec = e.do(call{method: "PATCH", path: "/v1/operator/tenants/" + tid, token: op,
		body: map[string]any{"status": "suspended", "reason": "abuse report"}})
	require.Equal(t, 200, rec.Code)
	rec = e.do(call{method: "GET", path: "/v1/admin/tenants/" + tid, token: owner})
	require.Equal(t, 200, rec.Code)
	assert.Contains(t, rec.Body.String(), `"status":"suspended"`)

	require.Equal(t, 204, e.do(call{method: "POST", path: "/v1/operator/auth/logout", token: op}).Code)
	require.Equal(t, 401, e.do(call{method: "GET", path: "/v1/operator/tenants", token: op}).Code)
}

func TestOperatorValidation(t *testing.T) {
	t.Parallel()
	e := newConsoleEnv(t, Limits{}, nil)
	_, owner := e.account()
	tid := e.createTenant(owner)
	op := e.operatorLogin()

	cases := []struct {
		name, method, path string
		body               any
		status             int
	}{
		{"no reason", "PATCH", "/v1/operator/tenants/" + tid, map[string]any{"sandbox": false}, 400},
		{"reason only", "PATCH", "/v1/operator/tenants/" + tid, map[string]any{"reason": "x"}, 400},
		{"delete is not an operator action", "PATCH", "/v1/operator/tenants/" + tid, map[string]any{"status": "deleted", "reason": "x"}, 400},
		{"unknown field", "PATCH", "/v1/operator/tenants/" + tid, map[string]any{"name": "x", "reason": "x"}, 400},
		{"unknown tenant", "GET", "/v1/operator/tenants/ten_" + strings.Repeat("1", 22), nil, 404},
		{"malformed id", "GET", "/v1/operator/tenants/app_123", nil, 404},
		{"bad status filter", "GET", "/v1/operator/tenants?status=gone", nil, 400},
		{"bad limit", "GET", "/v1/operator/tenants?limit=0", nil, 400},
	}
	for _, tc := range cases {
		rec := e.do(call{method: tc.method, path: tc.path, token: op, body: tc.body})
		assert.Equal(t, tc.status, rec.Code, "%s: %s", tc.name, rec.Body.String())
	}
}

// TestRealmSeparation: each route group accepts exactly one token kind.
func TestRealmSeparation(t *testing.T) {
	t.Parallel()
	e := newConsoleEnv(t, Limits{}, nil)
	_, console := e.account()
	tid := e.createTenant(console)
	op := e.operatorLogin()

	for _, path := range []string{"/v1/operator/tenants", "/v1/operator/tenants/" + tid} {
		rec := e.do(call{method: "GET", path: path, token: console})
		assert.Equal(t, 401, rec.Code, "console session on %s", path)
	}
	for _, path := range []string{"/v1/me", "/v1/admin/tenants/" + tid} {
		rec := e.do(call{method: "GET", path: path, token: op})
		assert.Equal(t, 401, rec.Code, "operator session on %s", path)
	}
	rec := e.do(call{method: "GET", path: "/v1/whoami", token: op})
	assert.Equal(t, 401, rec.Code, "operator session on the data plane")
	rec = e.do(call{method: "GET", path: "/v1/operator/tenants"})
	assert.Equal(t, 401, rec.Code, "anonymous")
}

func TestOperatorLoginFailuresAreUniform(t *testing.T) {
	t.Parallel()
	e := newConsoleEnv(t, Limits{}, nil)
	addr := "op" + newEmail()
	_, uri, err := e.ops.Create(context.Background(), addr, "Ops", strongPW)
	require.NoError(t, err)
	code, err := operator.CodeFromURI(uri, time.Now())
	require.NoError(t, err)
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}
	var bodies []string
	for _, in := range []map[string]string{
		{"email": addr, "password": strongPW, "totp": wrong},
		{"email": addr, "password": strongPW + "!", "totp": code},
		{"email": "nobody" + newEmail(), "password": strongPW, "totp": code},
	} {
		rec := e.do(call{method: "POST", path: "/v1/operator/auth/login", body: in})
		require.Equal(t, 401, rec.Code)
		var b errorBody
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &b))
		bodies = append(bodies, b.Error.Code+"|"+b.Error.Message)
	}
	assert.Equal(t, bodies[0], bodies[1])
	assert.Equal(t, bodies[0], bodies[2])
	// The code wasn't consumed by the failures.
	rec := e.do(call{method: "POST", path: "/v1/operator/auth/login", body: map[string]string{"email": addr, "password": strongPW, "totp": code}})
	require.Equal(t, 200, rec.Code)
	rec = e.do(call{method: "POST", path: "/v1/operator/auth/login", body: map[string]string{"email": addr, "password": strongPW, "totp": code}})
	require.Equal(t, 401, rec.Code, "replayed code")
}
