package httptransport

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func (e consoleEnv) post(token, path string, body any, want int) map[string]any {
	e.t.Helper()
	rec := e.do(call{method: "POST", path: path, token: token, body: body})
	require.Equal(e.t, want, rec.Code, "%s -> %s", path, rec.Body.String())
	out := map[string]any{}
	if rec.Body.Len() > 0 {
		require.NoError(e.t, json.Unmarshal(rec.Body.Bytes(), &out))
	}
	return out
}

func TestAppsEnvironmentsKeysJourney(t *testing.T) {
	t.Parallel()
	e := newConsoleEnv(t, Limits{}, nil)
	_, owner := e.account()
	tid := e.createTenant(owner)
	base := "/v1/admin/tenants/" + tid

	app := e.post(owner, base+"/applications", map[string]string{"name": "Shop", "key": "shop"}, 201)
	appPath := base + "/applications/" + app["id"].(string)
	env := e.post(owner, appPath+"/environments", map[string]string{"name": "prod"}, 201)

	created := e.post(owner, appPath+"/api-keys", map[string]any{"environment_id": env["id"], "name": "backend", "scopes": []string{"trigger"}}, 201)
	secret := created["key"].(string)
	assert.True(t, strings.HasPrefix(secret, "nfx_prod_"))

	rec := e.do(call{method: "GET", path: appPath + "/api-keys", token: owner})
	require.Equal(t, 200, rec.Code)
	assert.NotContains(t, rec.Body.String(), secret[len(secret)-20:], "listing never returns secrets")

	keyID := created["api_key"].(map[string]any)["id"].(string)
	rot := e.post(owner, appPath+"/api-keys/"+keyID+"/rotate", map[string]any{"grace_period_seconds": 3600}, 201)
	assert.NotEqual(t, secret, rot["key"])
	assert.NotEmpty(t, rot["old_key_revoked_at"])
	e.post(owner, appPath+"/api-keys/"+keyID+"/rotate", nil, 409)
	e.post(owner, appPath+"/api-keys/"+keyID+"/revoke", nil, 204)

	// Environment with an active (rotated-in) key can't be deleted.
	rec = e.do(call{method: "DELETE", path: appPath + "/environments/" + env["id"].(string), token: owner})
	require.Equal(t, 409, rec.Code)
	newID := rot["api_key"].(map[string]any)["id"].(string)
	e.post(owner, appPath+"/api-keys/"+newID+"/revoke", nil, 204)
	require.Equal(t, 204, e.do(call{method: "DELETE", path: appPath + "/environments/" + env["id"].(string), token: owner}).Code)
	require.Equal(t, 204, e.do(call{method: "DELETE", path: appPath, token: owner}).Code)
	require.Equal(t, 404, e.do(call{method: "GET", path: appPath, token: owner}).Code)

	assert.NotContains(t, e.logs.String(), secret, "secrets never logged")
}

func TestAppsDeveloperLimitsOverHTTP(t *testing.T) {
	t.Parallel()
	e := newConsoleEnv(t, Limits{}, nil)
	_, owner := e.account()
	tid := e.createTenant(owner)
	base := "/v1/admin/tenants/" + tid
	devAddr, dev := e.account()
	e.join(owner, tid, devAddr, dev, "developer")
	vAddr, viewer := e.account()
	e.join(owner, tid, vAddr, viewer, "viewer")

	app := e.post(dev, base+"/applications", map[string]string{"name": "Shop", "key": "shop"}, 201)
	appPath := base + "/applications/" + app["id"].(string)
	prod := e.post(dev, appPath+"/environments", map[string]string{"name": "prod"}, 201)
	dev1 := e.post(dev, appPath+"/environments", map[string]string{"name": "dev"}, 201)

	e.post(dev, appPath+"/api-keys", map[string]any{"environment_id": dev1["id"], "name": "ci"}, 201)
	rec := e.do(call{method: "POST", path: appPath + "/api-keys", token: dev, body: map[string]any{"environment_id": prod["id"], "name": "p"}})
	require.Equal(t, 403, rec.Code)
	assert.Equal(t, "forbidden", errCode(t, rec))
	require.Equal(t, 403, e.do(call{method: "GET", path: appPath + "/api-keys", token: viewer}).Code)
	require.Equal(t, 200, e.do(call{method: "GET", path: appPath + "/environments", token: viewer}).Code)
	require.Equal(t, 403, e.do(call{method: "DELETE", path: appPath, token: dev}).Code)
	rec = e.do(call{method: "POST", path: appPath + "/api-keys", token: dev, body: map[string]any{"environment_id": "env_nope", "name": "x"}})
	require.Equal(t, 400, rec.Code)
	rec = e.do(call{method: "POST", path: appPath + "/api-keys/key_0000000000000000000001/rotate", token: dev, body: map[string]any{"grace_period_seconds": 999999999}})
	require.Equal(t, 400, rec.Code)
}

// TestAppsBOLA: another tenant's owner, holding valid ids of their own, hits
// every app/env/key endpoint of tenant A. All 404, tenant A unchanged.
func TestAppsBOLA(t *testing.T) {
	t.Parallel()
	e := newConsoleEnv(t, Limits{}, nil)
	_, ownerA := e.account()
	tidA := e.createTenant(ownerA)
	baseA := "/v1/admin/tenants/" + tidA
	appA := e.post(ownerA, baseA+"/applications", map[string]string{"name": "Shop", "key": "shop"}, 201)
	appPathA := baseA + "/applications/" + appA["id"].(string)
	envA := e.post(ownerA, appPathA+"/environments", map[string]string{"name": "prod"}, 201)
	keyA := e.post(ownerA, appPathA+"/api-keys", map[string]any{"environment_id": envA["id"], "name": "k"}, 201)
	keyIDA := keyA["api_key"].(map[string]any)["id"].(string)

	_, attacker := e.account()
	tidB := e.createTenant(attacker)
	baseB := "/v1/admin/tenants/" + tidB
	appB := e.post(attacker, baseB+"/applications", map[string]string{"name": "Mine", "key": "mine"}, 201)
	appPathB := baseB + "/applications/" + appB["id"].(string)

	calls := []call{
		// Tenant A's paths directly.
		{method: "GET", path: baseA + "/applications"},
		{method: "POST", path: baseA + "/applications", body: map[string]string{"name": "x", "key": "x"}},
		{method: "GET", path: appPathA},
		{method: "PATCH", path: appPathA, body: map[string]string{"name": "pwned"}},
		{method: "DELETE", path: appPathA},
		{method: "GET", path: appPathA + "/environments"},
		{method: "POST", path: appPathA + "/environments", body: map[string]string{"name": "dev"}},
		{method: "DELETE", path: appPathA + "/environments/" + envA["id"].(string)},
		{method: "GET", path: appPathA + "/api-keys"},
		{method: "POST", path: appPathA + "/api-keys", body: map[string]any{"environment_id": envA["id"], "name": "x"}},
		{method: "POST", path: appPathA + "/api-keys/" + keyIDA + "/revoke"},
		{method: "POST", path: appPathA + "/api-keys/" + keyIDA + "/rotate"},
		// Tenant A's ids smuggled into the attacker's own tenant paths.
		{method: "GET", path: baseB + "/applications/" + appA["id"].(string)},
		{method: "POST", path: appPathB + "/api-keys", body: map[string]any{"environment_id": envA["id"], "name": "x"}},
		{method: "POST", path: appPathB + "/api-keys/" + keyIDA + "/revoke"},
		{method: "POST", path: appPathB + "/api-keys/" + keyIDA + "/rotate"},
		{method: "DELETE", path: appPathB + "/environments/" + envA["id"].(string)},
	}
	for _, c := range calls {
		c.token = attacker
		rec := e.do(c)
		assert.Equal(t, http.StatusNotFound, rec.Code, "%s %s -> %s", c.method, c.path, rec.Body.String())
	}

	rec := e.do(call{method: "GET", path: appPathA + "/api-keys", token: ownerA})
	require.Equal(t, 200, rec.Code)
	assert.Contains(t, rec.Body.String(), `"revoked_at":null`, "tenant A's key untouched")
	rec = e.do(call{method: "GET", path: appPathA, token: ownerA})
	assert.Contains(t, rec.Body.String(), `"name":"Shop"`)
}
