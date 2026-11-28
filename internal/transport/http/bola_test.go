package httptransport

import (
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sweepBodies holds a valid request body for every tenant-scoped admin route
// (nil = none). TestBOLASweep fails when a route is missing here, so a new
// route can't ship without cross-tenant coverage.
var sweepBodies = map[string]func(ids map[string]string) any{
	"GET ":                     nil,
	"PATCH ":                   func(map[string]string) any { return map[string]string{"name": "pwned"} },
	"DELETE ":                  nil,
	"GET /members":             nil,
	"PATCH /members/:user_id":  func(map[string]string) any { return map[string]string{"role": "owner"} },
	"DELETE /members/:user_id": nil,
	"GET /invitations":         nil,
	"POST /invitations": func(map[string]string) any {
		return map[string]string{"email": "sweep-" + newEmail(), "role": "owner"}
	},
	"DELETE /invitations/:invitation_id": nil,
	"GET /audit-events":                  nil,
	"GET /brand":                         nil,
	"PUT /brand":                         func(map[string]string) any { return map[string]string{"display_name": "pwned"} },
	"GET /policy":                        nil,
	"PUT /policy":                        func(map[string]string) any { return map[string]any{"max_attempts": 1} },
	"GET /applications/:application_id/brand":     nil,
	"PUT /applications/:application_id/brand":     func(map[string]string) any { return map[string]string{"display_name": "pwned"} },
	"DELETE /applications/:application_id/brand":  nil,
	"GET /applications/:application_id/policy":    nil,
	"PUT /applications/:application_id/policy":    func(map[string]string) any { return map[string]any{"max_attempts": 1} },
	"DELETE /applications/:application_id/policy": nil,
	"GET /applications":                           nil,
	"POST /applications": func(map[string]string) any {
		return map[string]string{"name": "x", "key": "sweep"}
	},
	"GET /applications/:application_id":              nil,
	"PATCH /applications/:application_id":            func(map[string]string) any { return map[string]string{"name": "pwned"} },
	"DELETE /applications/:application_id":           nil,
	"GET /applications/:application_id/environments": nil,
	"POST /applications/:application_id/environments": func(map[string]string) any {
		return map[string]string{"name": "dev"}
	},
	"DELETE /applications/:application_id/environments/:environment_id": nil,
	"GET /applications/:application_id/api-keys":                        nil,
	"POST /applications/:application_id/api-keys": func(ids map[string]string) any {
		return map[string]any{"environment_id": ids["environment_id"], "name": "x"}
	},
	"POST /applications/:application_id/api-keys/:api_key_id/revoke": nil,
	"GET /applications/:application_id/workflows":                    nil,
	"POST /applications/:application_id/workflows": func(map[string]string) any {
		return map[string]string{"key": "sweep", "name": "x"}
	},
	"GET /applications/:application_id/workflows/:workflow_id":    nil,
	"PATCH /applications/:application_id/workflows/:workflow_id":  func(map[string]string) any { return map[string]string{"name": "pwned"} },
	"DELETE /applications/:application_id/workflows/:workflow_id": nil,
	"POST /applications/:application_id/workflows/:workflow_id/actions": func(map[string]string) any {
		return map[string]string{"channel": "slack"}
	},
	"PATCH /applications/:application_id/workflows/:workflow_id/actions/:action_id": func(map[string]string) any {
		return map[string]bool{"enabled": false}
	},
	"DELETE /applications/:application_id/workflows/:workflow_id/actions/:action_id":                   nil,
	"GET /applications/:application_id/workflows/:workflow_id/actions/:action_id/templates":            nil,
	"GET /applications/:application_id/workflows/:workflow_id/actions/:action_id/templates/:locale":    nil,
	"DELETE /applications/:application_id/workflows/:workflow_id/actions/:action_id/templates/:locale": nil,
	"PUT /applications/:application_id/workflows/:workflow_id/actions/:action_id/templates/:locale": func(map[string]string) any {
		return map[string]string{"text": "pwned"}
	},
	"POST /applications/:application_id/templates/preview": func(ids map[string]string) any {
		return map[string]any{"channel": "sms", "template": map[string]string{"text": "x"}, "workflow_id": ids["workflow_id"]}
	},
	"POST /applications/:application_id/api-keys/:api_key_id/rotate": nil,
}

const tenantPrefix = "/v1/admin/tenants/:tenant_id"

// bodyRefs are routes whose request body references other objects by id.
var bodyRefs = map[string]bool{
	"POST /applications/:application_id/api-keys":          true,
	"POST /applications/:application_id/templates/preview": true,
}

var routeParam = regexp.MustCompile(`:([a-z_]+)`)

// TestBOLASweep: the owner of tenant B calls every tenant-scoped admin route
// with tenant A's ids, and every call must be 404. For a route with n path
// ids it tries n shapes: the first k ids (k = 0…n-1) are B's own and the
// rest are A's, so a foreign id is caught at every depth (tenant, app,
// workflow, action, …). Request bodies always carry A's ids, and routes
// whose body references ids are also called with an all-B path.
//
// Afterwards tenant A must be unchanged.
func TestBOLASweep(t *testing.T) {
	t.Parallel()
	e := newConsoleEnv(t, Limits{}, nil)

	world := func() (string, map[string]string) {
		_, owner := e.account()
		tid := e.createTenant(owner)
		base := "/v1/admin/tenants/" + tid
		app := e.post(owner, base+"/applications", map[string]string{"name": "Shop", "key": "shop"}, 201)
		appPath := base + "/applications/" + app["id"].(string)
		env := e.post(owner, appPath+"/environments", map[string]string{"name": "prod"}, 201)
		key := e.post(owner, appPath+"/api-keys", map[string]any{"environment_id": env["id"], "name": "k"}, 201)
		mAddr, mTok := e.account()
		member := e.join(owner, tid, mAddr, mTok, "viewer")
		inv := e.post(owner, base+"/invitations", map[string]string{"email": newEmail(), "role": "viewer"}, 201)
		wf := e.post(owner, appPath+"/workflows", map[string]string{"key": "order", "name": "Order"}, 201)
		wfPath := appPath + "/workflows/" + wf["id"].(string)
		act := e.post(owner, wfPath+"/actions", map[string]any{"channel": "sms"}, 201)
		rec := e.do(call{method: "PUT", path: wfPath + "/actions/" + act["id"].(string) + "/templates/en", token: owner, body: map[string]string{"text": "Order shipped"}})
		require.Equal(t, 200, rec.Code, rec.Body.String())
		return owner, map[string]string{
			"tenant_id": tid, "application_id": app["id"].(string), "environment_id": env["id"].(string),
			"api_key_id": key["api_key"].(map[string]any)["id"].(string), "user_id": member, "invitation_id": inv["id"].(string),
			"workflow_id": wf["id"].(string), "action_id": act["id"].(string), "locale": "en",
		}
	}
	ownerA, a := world()
	attacker, b := world()

	var routes []gin.RouteInfo
	for _, r := range e.h.(*gin.Engine).Routes() {
		if strings.HasPrefix(r.Path, tenantPrefix) {
			routes = append(routes, r)
		}
	}
	sort.Slice(routes, func(i, j int) bool { return routes[i].Method+routes[i].Path < routes[j].Method+routes[j].Path })
	require.NotEmpty(t, routes)

	fill := func(path string, pick func(param string) string) string {
		return routeParam.ReplaceAllStringFunc(path, func(m string) string { return pick(m[1:]) })
	}
	calls := 0
	for _, r := range routes {
		key := r.Method + " " + strings.TrimPrefix(r.Path, tenantPrefix)
		mkBody, known := sweepBodies[key]
		if !assert.True(t, known, "route %q has no BOLA sweep entry: add one to sweepBodies", key) {
			continue
		}
		// Owned ids, in path order; locale is a name, not an owned object.
		var owned []string
		for _, m := range routeParam.FindAllStringSubmatch(r.Path, -1) {
			if m[1] != "locale" {
				owned = append(owned, m[1])
			}
		}
		shapes := map[string]func(string) string{}
		for k := 0; k < len(owned); k++ {
			mine := map[string]bool{}
			for _, p := range owned[:k] {
				mine[p] = true
			}
			name := "all A"
			if k > 0 {
				name = "B's " + strings.Join(owned[:k], ", ") + "; A's rest"
			}
			shapes[name] = func(p string) string {
				if mine[p] {
					return b[p]
				}
				return a[p]
			}
		}
		if bodyRefs[key] {
			shapes["all B path, A's ids in the body"] = func(p string) string { return b[p] }
		}
		for name, pick := range shapes {
			var body any
			if mkBody != nil {
				body = mkBody(a) // bodies reference A's objects too
			}
			rec := e.do(call{method: r.Method, path: fill(r.Path, pick), token: attacker, body: body})
			assert.Equal(t, http.StatusNotFound, rec.Code, "%s [%s] -> %s", key, name, rec.Body.String())
			calls++
		}
	}
	t.Logf("BOLA sweep: %d routes, %d cross-tenant calls, all 404", len(routes), calls)
	for k := range sweepBodies {
		found := false
		for _, r := range routes {
			found = found || r.Method+" "+strings.TrimPrefix(r.Path, tenantPrefix) == k
		}
		assert.True(t, found, "sweep entry %q matches no route (stale)", k)
	}

	// Tenant A is untouched.
	base := "/v1/admin/tenants/" + a["tenant_id"]
	appPath := base + "/applications/" + a["application_id"]
	rec := e.do(call{method: "GET", path: base, token: ownerA})
	require.Equal(t, 200, rec.Code)
	assert.Contains(t, rec.Body.String(), `"name":"Acme"`)
	assert.Contains(t, rec.Body.String(), `"status":"active"`)
	rec = e.do(call{method: "GET", path: appPath, token: ownerA})
	require.Equal(t, 200, rec.Code)
	assert.Contains(t, rec.Body.String(), `"name":"Shop"`)
	rec = e.do(call{method: "GET", path: appPath + "/api-keys", token: ownerA})
	var keys struct {
		Data []map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &keys))
	require.Len(t, keys.Data, 1, "no key created or rotated in A")
	assert.Nil(t, keys.Data[0]["revoked_at"], "A's key not revoked")
	rec = e.do(call{method: "GET", path: appPath + "/environments", token: ownerA})
	assert.Contains(t, rec.Body.String(), a["environment_id"])
	assert.NotContains(t, rec.Body.String(), `"name":"dev"`)
	rec = e.do(call{method: "GET", path: base + "/members", token: ownerA})
	assert.Contains(t, rec.Body.String(), a["user_id"])
	assert.NotContains(t, rec.Body.String(), b["user_id"])
	rec = e.do(call{method: "GET", path: base + "/invitations", token: ownerA})
	assert.Contains(t, rec.Body.String(), a["invitation_id"])
	assert.NotContains(t, rec.Body.String(), "sweep-")

	// A's workflow, action and template are untouched.
	wfPath := appPath + "/workflows/" + a["workflow_id"]
	rec = e.do(call{method: "GET", path: wfPath, token: ownerA})
	require.Equal(t, 200, rec.Code)
	assert.Contains(t, rec.Body.String(), `"name":"Order"`)
	assert.Contains(t, rec.Body.String(), `"enabled":true`)
	assert.NotContains(t, rec.Body.String(), `"channel":"slack"`)
	rec = e.do(call{method: "GET", path: wfPath + "/actions/" + a["action_id"] + "/templates/en", token: ownerA})
	require.Equal(t, 200, rec.Code)
	assert.Contains(t, rec.Body.String(), `"text":"Order shipped"`)

	// A's brand and policy were never written (defaults, updated_at null).
	rec = e.do(call{method: "GET", path: base + "/brand", token: ownerA})
	assert.JSONEq(t, `{"default_locale":"en","updated_at":null}`, rec.Body.String())
	rec = e.do(call{method: "GET", path: appPath + "/policy", token: ownerA})
	assert.Contains(t, rec.Body.String(), `"override":null`)
	assert.Contains(t, rec.Body.String(), `"max_attempts":5`)

	// A's audit trail shows nothing done by the attacker.
	rec = e.do(call{method: "GET", path: base + "/audit-events?limit=200", token: ownerA})
	require.Equal(t, 200, rec.Code)
	var me struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	require.NoError(t, json.Unmarshal(e.do(call{method: "GET", path: "/v1/me", token: attacker}).Body.Bytes(), &me))
	require.NotEmpty(t, me.User.ID)
	assert.NotContains(t, rec.Body.String(), me.User.ID)

	// Console session revocation is user-bound too.
	rec = e.do(call{method: "GET", path: "/v1/me/sessions", token: ownerA})
	var sessions struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &sessions))
	require.NotEmpty(t, sessions.Data)
	rec = e.do(call{method: "DELETE", path: "/v1/me/sessions/" + sessions.Data[0].ID, token: attacker})
	assert.Equal(t, http.StatusNotFound, rec.Code)
	require.Equal(t, 200, e.do(call{method: "GET", path: "/v1/me", token: ownerA}).Code, "A's session still valid")
}

func TestAuditEventsOverHTTP(t *testing.T) {
	t.Parallel()
	e := newConsoleEnv(t, Limits{}, nil)
	_, owner := e.account()
	tid := e.createTenant(owner)
	base := "/v1/admin/tenants/" + tid
	rec := e.do(call{method: "POST", path: base + "/applications", token: owner, body: map[string]string{"name": "Shop", "key": "shop"}})
	require.Equal(t, 201, rec.Code)
	reqID := rec.Header().Get("X-Request-ID")
	require.NotEmpty(t, reqID)
	var app struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &app))

	// An operator change shows as Notifex staff, never the operator's id.
	op := e.operatorLogin()
	require.Equal(t, 200, e.do(call{method: "PATCH", path: "/v1/operator/tenants/" + tid, token: op,
		body: map[string]any{"sandbox": false, "reason": "onboarding"}}).Code)

	type event struct {
		ID        string         `json:"id"`
		ActorType string         `json:"actor_type"`
		ActorID   string         `json:"actor_id"`
		Action    string         `json:"action"`
		EntityID  string         `json:"entity_id"`
		RequestID string         `json:"request_id"`
		After     map[string]any `json:"after"`
	}
	var page struct {
		Data       []event `json:"data"`
		NextCursor *string `json:"next_cursor"`
	}
	rec = e.do(call{method: "GET", path: base + "/audit-events", token: owner})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	byAction := map[string]event{}
	for _, ev := range page.Data {
		if _, seen := byAction[ev.Action]; !seen {
			byAction[ev.Action] = ev // newest first
		}
	}
	created := byAction["application.create"]
	assert.Equal(t, app.ID, created.EntityID, "entity ids are public ids")
	assert.True(t, strings.HasPrefix(created.ActorID, "usr_"))
	assert.Equal(t, reqID, created.RequestID, "events carry the request id")
	assert.True(t, strings.HasPrefix(byAction["tenant.create"].EntityID, "ten_"))
	opEv := byAction["operator.tenant.update"]
	assert.Equal(t, "operator", opEv.ActorType)
	assert.Equal(t, "notifex", opEv.ActorID)
	assert.Equal(t, "operator.tenant.update", page.Data[0].Action, "newest first")

	// Keyset pagination walks the whole trail exactly once.
	seen := map[string]bool{}
	path := base + "/audit-events?limit=1"
	for i := 0; i < 50; i++ {
		rec = e.do(call{method: "GET", path: path, token: owner})
		require.Equal(t, 200, rec.Code)
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
		for _, ev := range page.Data {
			assert.False(t, seen[ev.ID], "duplicate %s", ev.ID)
			seen[ev.ID] = true
		}
		if page.NextCursor == nil {
			break
		}
		path = base + "/audit-events?limit=1&after=" + *page.NextCursor
	}
	assert.GreaterOrEqual(t, len(seen), 3)

	// Developers and viewers can't read the trail; bad cursors are 400.
	for _, role := range []string{"developer", "viewer"} {
		addr, tok := e.account()
		e.join(owner, tid, addr, tok, role)
		assert.Equal(t, 403, e.do(call{method: "GET", path: base + "/audit-events", token: tok}).Code, role)
	}
	assert.Equal(t, 400, e.do(call{method: "GET", path: base + "/audit-events?after=app_123", token: owner}).Code)
}
