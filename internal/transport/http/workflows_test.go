package httptransport

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorkflowsOverHTTP(t *testing.T) {
	t.Parallel()
	e := newConsoleEnv(t, Limits{}, nil)
	_, owner := e.account()
	tid := e.createTenant(owner)
	base := "/v1/admin/tenants/" + tid
	app := e.post(owner, base+"/applications", map[string]string{"name": "Shop", "key": "shop"}, 201)
	appBase := base + "/applications/" + app["id"].(string)

	wf := e.post(owner, appBase+"/workflows", map[string]any{
		"key": "order_shipped", "name": "Order shipped",
		"payload_schema": map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}, "n": map[string]any{"type": "integer"}}},
	}, 201)
	assert.Equal(t, "draft", wf["status"])
	wfBase := appBase + "/workflows/" + wf["id"].(string)

	email := e.post(owner, wfBase+"/actions", map[string]any{"channel": "email"}, 201)
	assert.Equal(t, true, email["enabled"])
	tplPath := wfBase + "/actions/" + email["id"].(string) + "/templates/en"

	// template_invalid with located details.
	rec := e.do(call{method: "PUT", path: tplPath, token: owner, body: map[string]string{"subject": "Hi", "html": "<p>\n  {{> footer}}</p>"}})
	require.Equal(t, 400, rec.Code, rec.Body.String())
	var bad struct {
		Error struct {
			Code    string `json:"code"`
			Details []struct {
				Field  string `json:"field"`
				Line   int    `json:"line"`
				Column int    `json:"column"`
			} `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &bad))
	assert.Equal(t, "template_invalid", bad.Error.Code)
	require.Len(t, bad.Error.Details, 1)
	assert.Equal(t, "html", bad.Error.Details[0].Field)
	assert.Equal(t, 2, bad.Error.Details[0].Line)
	assert.Equal(t, 3, bad.Error.Details[0].Column)

	rec = e.do(call{method: "PUT", path: tplPath, token: owner, body: map[string]any{"subject": "Hi", "body": map[string]any{}}})
	require.Equal(t, 400, rec.Code, "webhook field on an email template")
	assert.Equal(t, "validation_failed", errCode(t, rec))

	rec = e.do(call{method: "PUT", path: tplPath, token: owner, body: map[string]string{"subject": "Order {{payload.id}}", "html": "<p>{{payload.id}}</p>"}})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"channel":"email"`)

	rec = e.do(call{method: "PATCH", path: wfBase, token: owner, body: map[string]any{"status": "active"}})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"locales":["en"]`)

	// Rules of an active workflow are 409 with an explanation.
	rec = e.do(call{method: "PATCH", path: wfBase + "/actions/" + email["id"].(string), token: owner, body: map[string]bool{"enabled": false}})
	require.Equal(t, 409, rec.Code)
	assert.Contains(t, rec.Body.String(), "last enabled action")
	rec = e.do(call{method: "POST", path: wfBase + "/actions", token: owner, body: map[string]any{"channel": "sms"}})
	require.Equal(t, 409, rec.Code, "enabled action without a template on an active workflow")
	e.post(owner, wfBase+"/actions", map[string]any{"channel": "sms", "enabled": false}, 201)

	// payload_schema: absent leaves it, null removes it, remote $ref is refused.
	rec = e.do(call{method: "PATCH", path: wfBase, token: owner, body: map[string]any{"payload_schema": map[string]any{"$ref": "https://evil.test/x"}}})
	require.Equal(t, 400, rec.Code)
	assert.Contains(t, rec.Body.String(), "not allowed")

	// Preview: email with schema warnings, exact numbers.
	rec = e.do(call{method: "POST", path: appBase + "/templates/preview", token: owner, body: map[string]any{
		"channel": "email", "workflow_id": wf["id"],
		"template": map[string]string{"subject": "Order {{payload.id}} x{{payload.n}}", "html": "<p>{{payload.nope}}</p>"},
		"payload":  map[string]any{"id": "A1", "n": json.Number("12345678901234567")},
	}})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var pv struct {
		Subject  string `json:"subject"`
		HTML     string `json:"html"`
		Warnings []struct {
			Field   string `json:"field"`
			Message string `json:"message"`
		} `json:"warnings"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &pv))
	assert.Equal(t, "Order A1 x12345678901234567", pv.Subject, "large integers keep every digit")
	require.NotEmpty(t, pv.Warnings)
	assert.Contains(t, pv.Warnings[0].Message, "payload.nope")

	rec = e.do(call{method: "POST", path: appBase + "/templates/preview", token: owner, body: map[string]any{
		"channel": "sms", "template": map[string]string{"text": "नमस्ते {{payload.n}}"}, "payload": map[string]any{"n": 1},
	}})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"encoding":"ucs2"`)

	rec = e.do(call{method: "GET", path: appBase + "/workflows?status=active", token: owner})
	require.Equal(t, 200, rec.Code)
	assert.Contains(t, rec.Body.String(), `"key":"order_shipped"`)
	rec = e.do(call{method: "GET", path: wfBase + "/actions/" + email["id"].(string) + "/templates", token: owner})
	require.Equal(t, 200, rec.Code)
	assert.Contains(t, rec.Body.String(), `"locale":"en"`)

	// Roles: viewers read; developers edit and preview.
	addr, viewer := e.account()
	e.join(owner, tid, addr, viewer, "viewer")
	assert.Equal(t, 200, e.do(call{method: "GET", path: wfBase, token: viewer}).Code)
	assert.Equal(t, 403, e.do(call{method: "PATCH", path: wfBase, token: viewer, body: map[string]string{"name": "x"}}).Code)
	assert.Equal(t, 403, e.do(call{method: "POST", path: appBase + "/templates/preview", token: viewer,
		body: map[string]any{"channel": "sms", "template": map[string]string{"text": "x"}}}).Code)
	addr, dev := e.account()
	e.join(owner, tid, addr, dev, "developer")
	assert.Equal(t, 200, e.do(call{method: "PATCH", path: wfBase, token: dev, body: map[string]string{"name": "Renamed"}}).Code)

	// The application can't be deleted while it has workflows.
	assert.Equal(t, 409, e.do(call{method: "DELETE", path: appBase, token: owner}).Code)
	archived := e.do(call{method: "PATCH", path: wfBase, token: owner, body: map[string]any{"status": "archived", "payload_schema": nil}})
	require.Equal(t, 200, archived.Code)
	assert.Contains(t, archived.Body.String(), `"payload_schema":null`)
	assert.Equal(t, 204, e.do(call{method: "DELETE", path: wfBase, token: owner}).Code)
	assert.Equal(t, 404, e.do(call{method: "GET", path: wfBase, token: owner}).Code)
}
