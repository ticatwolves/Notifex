package httptransport

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dataPlane sets up an active "order" workflow (email) and returns base
// admin path, a prod API key and a dev API key.
func (e consoleEnv) dataPlane(owner string, scopes []string) (string, string, string) {
	e.t.Helper()
	tid := e.createTenant(owner)
	base := "/v1/admin/tenants/" + tid
	app := e.post(owner, base+"/applications", map[string]string{"name": "Shop", "key": "shop"}, 201)
	appBase := base + "/applications/" + app["id"].(string)
	prod := e.post(owner, appBase+"/environments", map[string]string{"name": "prod"}, 201)
	dev := e.post(owner, appBase+"/environments", map[string]string{"name": "dev"}, 201)
	wf := e.post(owner, appBase+"/workflows", map[string]any{"key": "order", "name": "Order",
		"payload_schema": map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}}}, 201)
	wfBase := appBase + "/workflows/" + wf["id"].(string)
	act := e.post(owner, wfBase+"/actions", map[string]any{"channel": "email"}, 201)
	rec := e.do(call{method: "PUT", path: wfBase + "/actions/" + act["id"].(string) + "/templates/en", token: owner,
		body: map[string]string{"subject": "Order {{payload.id}}", "text": "Hi"}})
	require.Equal(e.t, 200, rec.Code, rec.Body.String())
	rec = e.do(call{method: "PATCH", path: wfBase, token: owner, body: map[string]string{"status": "active"}})
	require.Equal(e.t, 200, rec.Code, rec.Body.String())
	key := func(env map[string]any) string {
		body := map[string]any{"environment_id": env["id"], "name": "backend"}
		if scopes != nil {
			body["scopes"] = scopes
		}
		return e.post(owner, appBase+"/api-keys", body, 201)["key"].(string)
	}
	return base, key(prod), key(dev)
}

type triggerCall struct {
	key, idem string
	body      any
}

func (e consoleEnv) trigger(c triggerCall) (int, map[string]any) {
	e.t.Helper()
	rec := e.doWithHeader(call{method: "POST", path: "/v1/trigger", token: c.key, body: c.body}, "Idempotency-Key", c.idem)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestTriggerOverHTTP(t *testing.T) {
	t.Parallel()
	e := newConsoleEnv(t, Limits{}, nil)
	_, owner := e.account()
	_, key, devKey := e.dataPlane(owner, nil)

	body := map[string]any{"workflow": "order", "payload": map[string]any{"id": "A1"},
		"recipients": []map[string]any{{"id": "r1", "email": "a@example.com"}, {"id": "r2"}}}
	code, res := e.trigger(triggerCall{key: key, body: body, idem: "order-A1-" + uuid.NewString()[:8]})
	require.Equal(t, 202, code, res)
	assert.Equal(t, "accepted", res["status"])
	notifs := res["notifications"].([]any)
	require.Len(t, notifs, 2)
	missing := notifs[1].(map[string]any)
	assert.Equal(t, "failed", missing["status"])
	assert.Equal(t, "recipient_missing", missing["error"].(map[string]any)["code"])
	runID := res["run_id"].(string)
	assert.True(t, strings.HasPrefix(runID, "run_"))

	// Idempotent replay and conflict.
	idem := "order-B2-" + uuid.NewString()[:8]
	_, first := e.trigger(triggerCall{key: key, body: body, idem: idem})
	_, again := e.trigger(triggerCall{key: key, body: body, idem: idem})
	assert.Equal(t, first["run_id"], again["run_id"])
	other := map[string]any{"workflow": "order", "recipients": []map[string]any{{"email": "b@example.com"}}}
	code, res = e.trigger(triggerCall{key: key, body: other, idem: idem})
	assert.Equal(t, 409, code)
	assert.Equal(t, "idempotency_conflict", res["error"].(map[string]any)["code"])

	// Request errors.
	for name, c := range map[string]struct {
		body any
		idem string
		code int
		err  string
	}{
		"unknown workflow":  {map[string]any{"workflow": "nope", "recipients": []map[string]any{{"email": "a@example.com"}}}, "", 404, "workflow_not_found"},
		"schema mismatch":   {map[string]any{"workflow": "order", "payload": map[string]any{"id": 5}, "recipients": []map[string]any{{"email": "a@example.com"}}}, "", 400, "validation_failed"},
		"unknown field":     {map[string]any{"workflow": "order", "recipients": []map[string]any{{"email": "a@example.com"}}, "priority": "high"}, "", 400, "validation_failed"},
		"no recipients":     {map[string]any{"workflow": "order", "recipients": []map[string]any{}}, "", 400, "validation_failed"},
		"bad idempotency":   {body, "short", 400, "validation_failed"},
		"far schedule":      {map[string]any{"workflow": "order", "schedule_at": "2099-01-01T00:00:00Z", "recipients": []map[string]any{{"email": "a@example.com"}}}, "", 400, "schedule_horizon_exceeded"},
		"channel not in wf": {map[string]any{"workflow": "order", "channels": []string{"sms"}, "recipients": []map[string]any{{"phone": "+919876543210"}}}, "", 400, "validation_failed"},
	} {
		code, res := e.trigger(triggerCall{key: key, body: c.body, idem: c.idem})
		assert.Equal(t, c.code, code, name)
		if res["error"] != nil {
			assert.Equal(t, c.err, res["error"].(map[string]any)["code"], name)
		}
	}

	// Reads: run and notification with timeline, scoped to the key's environment.
	rec := e.do(call{method: "GET", path: "/v1/runs/" + runID, token: key})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"workflow":"order"`)
	assert.Contains(t, rec.Body.String(), `"source":"rest"`)
	nid := notifs[0].(map[string]any)["id"].(string)
	rec = e.do(call{method: "GET", path: "/v1/notifications/" + nid, token: key})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"recipient_masked":"a***@e***.com"`)
	assert.Contains(t, rec.Body.String(), `"type":"accepted"`)
	assert.NotContains(t, rec.Body.String(), "a@example.com", "no raw recipient")
	assert.NotContains(t, rec.Body.String(), "A1", "no payload")
	assert.Equal(t, 404, e.do(call{method: "GET", path: "/v1/runs/" + runID, token: devKey}).Code, "other environment")
	assert.Equal(t, 404, e.do(call{method: "GET", path: "/v1/notifications/" + nid, token: devKey}).Code)

	// Another tenant's key sees nothing; console sessions never reach the data plane.
	_, owner2 := e.account()
	_, key2, _ := e.dataPlane(owner2, nil)
	assert.Equal(t, 404, e.do(call{method: "GET", path: "/v1/runs/" + runID, token: key2}).Code)
	assert.Equal(t, 401, e.do(call{method: "POST", path: "/v1/trigger", token: owner, body: body}).Code)

	// The server log never has the payload or raw recipients.
	assert.NotContains(t, e.logs.String(), "a@example.com")
	assert.NotContains(t, e.logs.String(), `"A1"`)
}

func TestTriggerScopes(t *testing.T) {
	t.Parallel()
	e := newConsoleEnv(t, Limits{}, nil)
	_, owner := e.account()
	_, readOnly, _ := e.dataPlane(owner, []string{"read"})
	body := map[string]any{"workflow": "order", "recipients": []map[string]any{{"email": "a@example.com"}}}
	code, res := e.trigger(triggerCall{key: readOnly, body: body})
	assert.Equal(t, 403, code, res)

	_, owner2 := e.account()
	_, triggerOnly, _ := e.dataPlane(owner2, []string{"trigger"})
	code, res = e.trigger(triggerCall{key: triggerOnly, body: body})
	require.Equal(t, 202, code, res)
	assert.Equal(t, 403, e.do(call{method: "GET", path: "/v1/runs/" + res["run_id"].(string), token: triggerOnly}).Code)
}
