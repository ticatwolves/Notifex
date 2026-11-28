package httptransport

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBrandAndPolicyOverHTTP(t *testing.T) {
	t.Parallel()
	e := newConsoleEnv(t, Limits{}, nil)
	_, owner := e.account()
	tid := e.createTenant(owner)
	base := "/v1/admin/tenants/" + tid
	app := e.post(owner, base+"/applications", map[string]string{"name": "Shop", "key": "shop"}, 201)
	appBase := base + "/applications/" + app["id"].(string)

	// Defaults.
	rec := e.do(call{method: "GET", path: base + "/brand", token: owner})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{"default_locale":"en","updated_at":null}`, rec.Body.String())
	rec = e.do(call{method: "GET", path: base + "/policy", token: owner})
	require.Equal(t, 200, rec.Code)
	assert.JSONEq(t, `{"allowed_channels":[],"quiet_hours":null,"rate_limits_per_minute":{},"max_attempts":5,"payload_retention_days":7,"updated_at":null}`, rec.Body.String())

	rec = e.do(call{method: "PUT", path: base + "/brand", token: owner, body: map[string]any{
		"display_name": "Acme", "logo_url": "https://cdn.acme.test/l.png",
		"colors": map[string]string{"primary": "#112233", "background": "#ffffff"},
		"email":  map[string]string{"from_name": "Acme", "reply_to": "help@acme.test"},
	}})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"display_name":"Acme"`)

	rec = e.do(call{method: "PUT", path: base + "/policy", token: owner, body: map[string]any{
		"allowed_channels": []string{"email", "sms"}, "payload_retention_days": 3,
		"quiet_hours": map[string]any{"start": "22:00", "end": "07:00", "timezone": "Asia/Kolkata", "channels": []string{"sms"}},
	}})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"payload_retention_days":3`)

	// Application override and the merged result.
	rec = e.do(call{method: "GET", path: appBase + "/brand", token: owner})
	require.Equal(t, 200, rec.Code)
	assert.Contains(t, rec.Body.String(), `"override":null`)
	rec = e.do(call{method: "PUT", path: appBase + "/brand", token: owner, body: map[string]any{"colors": map[string]string{"primary": "#ff0000"}}})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var view struct {
		Override  map[string]any `json:"override"`
		Effective map[string]any `json:"effective"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &view))
	assert.Equal(t, map[string]any{"colors": map[string]any{"primary": "#ff0000"}}, view.Override)
	colors := view.Effective["colors"].(map[string]any)
	assert.Equal(t, "#ff0000", colors["primary"])
	assert.Equal(t, "#ffffff", colors["background"])
	assert.Equal(t, "Acme", view.Effective["display_name"])

	rec = e.do(call{method: "PUT", path: appBase + "/policy", token: owner, body: map[string]any{"max_attempts": 2}})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"payload_retention_days":3`, "effective retention is the tenant's")

	require.Equal(t, 204, e.do(call{method: "DELETE", path: appBase + "/brand", token: owner}).Code)
	require.Equal(t, 404, e.do(call{method: "DELETE", path: appBase + "/brand", token: owner}).Code)
	require.Equal(t, 204, e.do(call{method: "DELETE", path: appBase + "/policy", token: owner}).Code)

	// Validation: field-level messages, unknown fields, retention scope.
	cases := []struct {
		name, method, path string
		body               any
		msg                string
	}{
		{"http logo", "PUT", base + "/brand", map[string]any{"logo_url": "http://x.test/l.png"}, "logo_url"},
		{"bad colour", "PUT", base + "/brand", map[string]any{"colors": map[string]string{"primary": "red"}}, "colors.primary"},
		{"unknown field", "PUT", base + "/brand", map[string]any{"font": "Comic Sans"}, "unknown field"},
		{"unknown nested field", "PUT", base + "/brand", map[string]any{"colors": map[string]string{"accent": "#000000"}}, "unknown field"},
		{"retention 8", "PUT", base + "/policy", map[string]any{"payload_retention_days": 8}, "payload_retention_days"},
		{"retention on override", "PUT", appBase + "/policy", map[string]any{"payload_retention_days": 2}, "tenant policy only"},
		{"bad zone", "PUT", base + "/policy", map[string]any{"quiet_hours": map[string]string{"start": "22:00", "end": "07:00", "timezone": "Nowhere/City"}}, "quiet_hours.timezone"},
		{"two problems", "PUT", base + "/policy", map[string]any{"max_attempts": 0, "allowed_channels": []string{"fax"}}, "; "},
	}
	for _, tc := range cases {
		rec := e.do(call{method: tc.method, path: tc.path, token: owner, body: tc.body})
		require.Equal(t, 400, rec.Code, tc.name)
		assert.Equal(t, "validation_failed", errCode(t, rec), tc.name)
		assert.Contains(t, rec.Body.String(), tc.msg, tc.name)
	}

	// Roles: viewers and developers read, only admins and owners write.
	for _, role := range []string{"viewer", "developer"} {
		addr, tok := e.account()
		e.join(owner, tid, addr, tok, role)
		assert.Equal(t, 200, e.do(call{method: "GET", path: appBase + "/policy", token: tok}).Code, role)
		assert.Equal(t, 403, e.do(call{method: "PUT", path: base + "/brand", token: tok, body: map[string]any{}}).Code, role)
		assert.Equal(t, 403, e.do(call{method: "PUT", path: appBase + "/policy", token: tok, body: map[string]any{}}).Code, role)
	}

	// Audit trail records the changes.
	rec = e.do(call{method: "GET", path: base + "/audit-events?limit=200", token: owner})
	for _, action := range []string{"brand.update", "policy.update", "brand.override.update", "brand.override.delete", "policy.override.delete"} {
		assert.Contains(t, rec.Body.String(), `"action":"`+action+`"`)
	}
	assert.NotContains(t, rec.Body.String(), "help@acme.test", "reply-to masked in the trail")
}
