package httptransport

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCredentialsOverHTTP: the full lifecycle, and no secret ever appears in
// a response body, the server log or the audit trail.
func TestCredentialsOverHTTP(t *testing.T) {
	t.Parallel()
	e := newConsoleEnv(t, Limits{}, nil)
	_, owner := e.account()
	tid := e.createTenant(owner)
	base := "/v1/admin/tenants/" + tid
	app := e.post(owner, base+"/applications", map[string]string{"name": "Shop", "key": "shop"}, 201)
	appID := app["id"].(string)
	env := e.post(owner, base+"/applications/"+appID+"/environments", map[string]string{"name": "prod"}, 201)
	envID := env["id"].(string)

	// Built from repeats: no key-shaped literal for secret scanners to flag.
	var (
		sg1 = "SG." + strings.Repeat("f", 16) + "." + strings.Repeat("a", 24) + "S1S1"
		sg2 = "SG." + strings.Repeat("g", 16) + "." + strings.Repeat("b", 24) + "S2S2"
		tw  = strings.Repeat("fe", 16)
	)
	var bodies []string
	do := func(c call) map[string]any {
		t.Helper()
		c.token = owner
		rec := e.do(c)
		bodies = append(bodies, rec.Body.String())
		return map[string]any{"code": rec.Code, "body": rec.Body.String()}
	}

	rec := e.do(call{method: "GET", path: "/v1/admin/channel-providers", token: owner})
	require.Equal(t, 200, rec.Code)
	assert.Contains(t, rec.Body.String(), `"provider":"sendgrid"`)
	assert.Contains(t, rec.Body.String(), `"secret_generated":true`)

	created := e.post(owner, base+"/channel-credentials", map[string]any{
		"channel": "email", "provider": "sendgrid", "name": "SendGrid", "application_id": appID,
		"config": map[string]string{"from_email": "ops@acme.test"}, "secret": map[string]string{"api_key": sg1},
	}, 201)
	cred := created["credential"].(map[string]any)
	assert.Equal(t, "S1S1", cred["secret_hint"])
	assert.Equal(t, true, cred["has_secret"])
	assert.Nil(t, created["generated_secret"])
	credPath := base + "/channel-credentials/" + cred["id"].(string)

	r := do(call{method: "POST", path: base + "/channel-credentials", body: map[string]any{
		"channel": "email", "provider": "sendgrid", "name": "dup", "application_id": appID,
		"config": map[string]string{"from_email": "ops@acme.test"}, "secret": map[string]string{"api_key": sg1},
	}})
	assert.Equal(t, 409, r["code"], "one active credential per scope and channel")

	r = do(call{method: "POST", path: base + "/channel-credentials", body: map[string]any{
		"channel": "email", "provider": "sendgrid", "name": "bad", "config": map[string]string{"from_email": "x@y.test"},
		"secret": map[string]string{"api_key": "SG.short"},
	}})
	assert.Equal(t, 400, r["code"])
	assert.Contains(t, r["body"], "secret.api_key")

	hook := e.post(owner, base+"/channel-credentials", map[string]any{"channel": "webhook", "provider": "signing", "name": "Hooks", "config": map[string]any{"url": "https://hooks.acme.test/notifex"}}, 201)
	gen := hook["generated_secret"].(string)
	assert.True(t, strings.HasPrefix(gen, "whsec_"), "generated secret shown once at creation")
	rec = e.do(call{method: "GET", path: base + "/channel-credentials/" + hook["credential"].(map[string]any)["id"].(string), token: owner})
	assert.NotContains(t, rec.Body.String(), gen, "and never again")

	do(call{method: "POST", path: base + "/channel-credentials", body: map[string]any{
		"channel": "sms", "provider": "twilio", "name": "Twilio",
		"config": map[string]string{"account_sid": "AC" + strings.Repeat("cd", 16), "from": "+14155550100"},
		"secret": map[string]string{"auth_token": tw},
	}})

	// Resolve: application credential for email, platform default for SMS? No: tenant Twilio.
	rec = e.do(call{method: "GET", path: base + "/channel-credentials/resolve?channel=email&application_id=" + appID + "&environment_id=" + envID, token: owner})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"scope":"application"`)
	rec = e.do(call{method: "GET", path: base + "/channel-credentials/resolve?channel=sms&application_id=" + appID + "&environment_id=" + envID, token: owner})
	assert.Contains(t, rec.Body.String(), `"provider":"twilio"`)
	rec = e.do(call{method: "GET", path: base + "/channel-credentials/resolve?channel=slack&application_id=" + appID + "&environment_id=" + envID, token: owner})
	require.Equal(t, 200, rec.Code)
	assert.Contains(t, rec.Body.String(), `"available":false`)

	r = do(call{method: "POST", path: credPath + "/rotate", body: map[string]any{"secret": map[string]string{"api_key": sg2}}})
	require.Equal(t, 200, r["code"], r["body"])
	assert.Contains(t, r["body"], `"secret_hint":"S2S2"`)

	r = do(call{method: "PATCH", path: credPath, body: map[string]any{"enabled": false}})
	require.Equal(t, 200, r["code"])
	assert.Contains(t, r["body"], `"status":"disabled"`)
	r = do(call{method: "PATCH", path: credPath, body: map[string]any{"config": map[string]string{"from_email": "ops@acme.test", "api_key": sg2}}})
	assert.Equal(t, 400, r["code"], "secrets can't go into config")

	// Roles: developers see metadata, viewers nothing, only admins/owners write.
	addr, dev := e.account()
	e.join(owner, tid, addr, dev, "developer")
	assert.Equal(t, 200, e.do(call{method: "GET", path: base + "/channel-credentials", token: dev}).Code)
	assert.Equal(t, 403, e.do(call{method: "POST", path: credPath + "/rotate", token: dev, body: map[string]any{"secret": map[string]string{"api_key": sg1}}}).Code)
	addr, viewer := e.account()
	e.join(owner, tid, addr, viewer, "viewer")
	assert.Equal(t, 403, e.do(call{method: "GET", path: base + "/channel-credentials", token: viewer}).Code)

	// Credentials block deleting their application.
	assert.Equal(t, 409, e.do(call{method: "DELETE", path: base + "/applications/" + appID, token: owner}).Code)
	assert.Equal(t, 204, e.do(call{method: "DELETE", path: credPath, token: owner}).Code)
	assert.Equal(t, 404, e.do(call{method: "GET", path: credPath, token: owner}).Code)

	// No secret anywhere: responses, server log, audit trail.
	rec = e.do(call{method: "GET", path: base + "/audit-events?limit=200", token: owner})
	require.Equal(t, 200, rec.Code)
	assert.Contains(t, rec.Body.String(), `"action":"credential.rotate"`)
	everything := strings.Join(bodies, "\n") + rec.Body.String() + e.logs.String()
	for _, s := range []string{sg1, sg2, tw, gen[6:]} {
		assert.NotContains(t, everything, s)
	}
	list := e.do(call{method: "GET", path: base + "/channel-credentials", token: owner}).Body.String()
	for _, s := range []string{sg1, sg2, tw, gen} {
		assert.NotContains(t, list, s)
	}
}
