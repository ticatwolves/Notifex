package httptransport

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// account signs up, verifies and logs in a user; returns email and session.
func (e consoleEnv) account() (string, string) {
	e.t.Helper()
	addr := newEmail()
	require.Equal(e.t, 202, e.do(call{method: "POST", path: "/v1/auth/signup", body: map[string]string{"email": addr, "password": strongPW}}).Code)
	require.Equal(e.t, 204, e.do(call{method: "POST", path: "/v1/auth/verify-email", body: map[string]string{"token": e.mailedToken(addr)}}).Code)
	rec := e.do(call{method: "POST", path: "/v1/auth/login", body: map[string]string{"email": addr, "password": strongPW}})
	require.Equal(e.t, 200, rec.Code)
	var s sessionCreatedResponse
	require.NoError(e.t, json.Unmarshal(rec.Body.Bytes(), &s))
	return addr, s.Token
}

func (e consoleEnv) createTenant(token string) string {
	e.t.Helper()
	rec := e.do(call{method: "POST", path: "/v1/admin/tenants", token: token, body: map[string]string{"name": "Acme", "slug": "t-" + newEmail()[1:9]}})
	require.Equal(e.t, 201, rec.Code, rec.Body.String())
	var out struct {
		ID string `json:"id"`
	}
	require.NoError(e.t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out.ID
}

// join invites addr to tenant tid with role (as owner token) and accepts it.
func (e consoleEnv) join(ownerToken, tid, addr, token, role string) string {
	e.t.Helper()
	rec := e.do(call{method: "POST", path: "/v1/admin/tenants/" + tid + "/invitations", token: ownerToken, body: map[string]string{"email": addr, "role": role}})
	require.Equal(e.t, 201, rec.Code, rec.Body.String())
	rec = e.do(call{method: "POST", path: "/v1/invitations/accept", token: token, body: map[string]string{"token": e.mailedToken(addr)}})
	require.Equal(e.t, 200, rec.Code, rec.Body.String())
	var m struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	require.NoError(e.t, json.Unmarshal(rec.Body.Bytes(), &m))
	return m.User.ID
}

func TestTenantAdminJourney(t *testing.T) {
	t.Parallel()
	e := newConsoleEnv(t, Limits{}, nil)
	_, owner := e.account()
	tid := e.createTenant(owner)

	rec := e.do(call{method: "GET", path: "/v1/admin/tenants/" + tid, token: owner})
	require.Equal(t, 200, rec.Code)
	assert.Contains(t, rec.Body.String(), `"sandbox":true`)
	rec = e.do(call{method: "GET", path: "/v1/me", token: owner})
	assert.Contains(t, rec.Body.String(), `"role":"owner"`)

	devAddr, dev := e.account()
	devID := e.join(owner, tid, devAddr, dev, "developer")

	rec = e.do(call{method: "GET", path: "/v1/admin/tenants/" + tid + "/members?limit=1", token: dev})
	require.Equal(t, 200, rec.Code)
	var page struct {
		Data       []json.RawMessage `json:"data"`
		NextCursor *string           `json:"next_cursor"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	require.Len(t, page.Data, 1)
	require.NotNil(t, page.NextCursor)
	rec = e.do(call{method: "GET", path: "/v1/admin/tenants/" + tid + "/members?limit=1&after=" + *page.NextCursor, token: dev})
	require.Equal(t, 200, rec.Code)

	rec = e.do(call{method: "PATCH", path: "/v1/admin/tenants/" + tid + "/members/" + devID, token: owner, body: map[string]string{"role": "admin"}})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"role":"admin"`)

	rec = e.do(call{method: "PATCH", path: "/v1/admin/tenants/" + tid, token: dev, body: map[string]string{"name": "Renamed by admin"}})
	require.Equal(t, 200, rec.Code, "now an admin")

	require.Equal(t, 204, e.do(call{method: "DELETE", path: "/v1/admin/tenants/" + tid + "/members/" + devID, token: owner}).Code)
	require.Equal(t, 404, e.do(call{method: "GET", path: "/v1/admin/tenants/" + tid, token: dev}).Code, "removed member loses access at once")

	// Deleting requires recent re-auth; login counts, so age it out by
	// exercising the error path through a different owner session later.
	rec = e.do(call{method: "DELETE", path: "/v1/admin/tenants/" + tid, token: owner})
	require.Equal(t, 202, rec.Code, "fresh login counts as recent authentication")
	require.Equal(t, 404, e.do(call{method: "GET", path: "/v1/admin/tenants/" + tid, token: owner}).Code)
}

func TestTenantAdminDeniesByRole(t *testing.T) {
	t.Parallel()
	e := newConsoleEnv(t, Limits{}, nil)
	_, owner := e.account()
	tid := e.createTenant(owner)
	vAddr, viewer := e.account()
	e.join(owner, tid, vAddr, viewer, "viewer")
	aAddr, admin := e.account()
	e.join(owner, tid, aAddr, admin, "admin")

	tests := []struct {
		name, token string
		c           call
		status      int
		code        string
	}{
		{"viewer can't rename", viewer, call{method: "PATCH", path: "/v1/admin/tenants/" + tid, body: map[string]string{"name": "x"}}, 403, "forbidden"},
		{"viewer can't invite", viewer, call{method: "POST", path: "/v1/admin/tenants/" + tid + "/invitations", body: map[string]string{"email": newEmail(), "role": "viewer"}}, 403, "forbidden"},
		{"viewer can't list invitations", viewer, call{method: "GET", path: "/v1/admin/tenants/" + tid + "/invitations"}, 403, "forbidden"},
		{"admin can't invite an owner", admin, call{method: "POST", path: "/v1/admin/tenants/" + tid + "/invitations", body: map[string]string{"email": newEmail(), "role": "owner"}}, 403, "forbidden"},
		{"admin can't delete the tenant", admin, call{method: "DELETE", path: "/v1/admin/tenants/" + tid}, 403, "forbidden"},
		{"bad role value", admin, call{method: "POST", path: "/v1/admin/tenants/" + tid + "/invitations", body: map[string]string{"email": newEmail(), "role": "god"}}, 400, "validation_failed"},
		{"bad cursor", viewer, call{method: "GET", path: "/v1/admin/tenants/" + tid + "/members?after=nope"}, 400, "validation_failed"},
		{"bad limit", viewer, call{method: "GET", path: "/v1/admin/tenants/" + tid + "/members?limit=0"}, 400, "validation_failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := tt.c
			c.token = tt.token
			rec := e.do(c)
			require.Equal(t, tt.status, rec.Code, rec.Body.String())
			assert.Equal(t, tt.code, errCode(t, rec))
		})
	}

	// Unverified users can't create tenants.
	addr := newEmail()
	e.do(call{method: "POST", path: "/v1/auth/signup", body: map[string]string{"email": addr, "password": strongPW}})
	rec := e.do(call{method: "POST", path: "/v1/auth/login", body: map[string]string{"email": addr, "password": strongPW}})
	var s sessionCreatedResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &s))
	rec = e.do(call{method: "POST", path: "/v1/admin/tenants", token: s.Token, body: map[string]string{"name": "x", "slug": "unverified-" + newEmail()[1:7]}})
	require.Equal(t, 403, rec.Code)
	assert.Equal(t, "email_not_verified", errCode(t, rec))
}

// TestTenantAdminBOLA: a member of tenant B uses every tenant-A endpoint.
// Every one must be 404 (indistinguishable from a tenant that doesn't
// exist), and tenant A must be unchanged.
func TestTenantAdminBOLA(t *testing.T) {
	t.Parallel()
	e := newConsoleEnv(t, Limits{}, nil)
	_, ownerA := e.account()
	tidA := e.createTenant(ownerA)
	mAddr, memberA := e.account()
	memberAID := e.join(ownerA, tidA, mAddr, memberA, "viewer")
	rec := e.do(call{method: "POST", path: "/v1/admin/tenants/" + tidA + "/invitations", token: ownerA, body: map[string]string{"email": newEmail(), "role": "viewer"}})
	var inv struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &inv))

	_, attacker := e.account()
	e.createTenant(attacker) // owner of their own tenant B

	for _, c := range []call{
		{method: "GET", path: "/v1/admin/tenants/" + tidA},
		{method: "PATCH", path: "/v1/admin/tenants/" + tidA, body: map[string]string{"name": "pwned"}},
		{method: "DELETE", path: "/v1/admin/tenants/" + tidA},
		{method: "GET", path: "/v1/admin/tenants/" + tidA + "/members"},
		{method: "PATCH", path: "/v1/admin/tenants/" + tidA + "/members/" + memberAID, body: map[string]string{"role": "admin"}},
		{method: "DELETE", path: "/v1/admin/tenants/" + tidA + "/members/" + memberAID},
		{method: "GET", path: "/v1/admin/tenants/" + tidA + "/invitations"},
		{method: "POST", path: "/v1/admin/tenants/" + tidA + "/invitations", body: map[string]string{"email": newEmail(), "role": "owner"}},
		{method: "DELETE", path: "/v1/admin/tenants/" + tidA + "/invitations/" + inv.ID},
		{method: "GET", path: "/v1/admin/tenants/ten_0000000000000000000000"},
		{method: "GET", path: "/v1/admin/tenants/not-an-id"},
	} {
		c.token = attacker
		rec := e.do(c)
		assert.Equal(t, http.StatusNotFound, rec.Code, "%s %s -> %s", c.method, c.path, rec.Body.String())
	}

	rec = e.do(call{method: "GET", path: "/v1/admin/tenants/" + tidA, token: ownerA})
	require.Equal(t, 200, rec.Code)
	assert.Contains(t, rec.Body.String(), `"name":"Acme"`, "tenant A unchanged")
	rec = e.do(call{method: "GET", path: "/v1/admin/tenants/" + tidA + "/members", token: ownerA})
	assert.Contains(t, rec.Body.String(), `"role":"viewer"`, "member A unchanged")

	require.Equal(t, 401, e.do(call{method: "GET", path: "/v1/admin/tenants/" + tidA}).Code, "no session")
}
