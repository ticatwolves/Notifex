package httptransport

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ticatwolves/notifex/internal/auth"
	"github.com/ticatwolves/notifex/internal/auth/oauthtest"
)

const oauthRedirect = "https://console.test/oauth/callback"

func newOAuthHTTPEnv(t *testing.T) (consoleEnv, *oauthtest.Server) {
	t.Helper()
	g := oauthtest.New(false)
	t.Cleanup(g.Close)
	e := newConsoleEnvWith(t, Limits{}, nil, func(a *auth.Accounts) ConsoleOAuth {
		p := auth.NewGoogle(context.Background(), auth.ClientCredentials{ID: oauthtest.ClientID, Secret: oauthtest.ClientSecret},
			auth.OIDCEndpoints{AuthURL: g.URL + "/authorize", TokenURL: g.URL + "/token", JWKSURL: g.URL + "/jwks", Issuer: g.Issuer()})
		return auth.NewOAuth(a, []auth.Provider{p}, []string{oauthRedirect})
	})
	return e, g
}

func (e consoleEnv) oauthFlow(g *oauthtest.Server, token string, u oauthtest.User) (authStatus int, cbStatus int, body string) {
	e.t.Helper()
	rec := e.do(call{method: "POST", path: "/v1/auth/oauth/google/authorize", token: token, body: map[string]string{"redirect_uri": oauthRedirect}})
	if rec.Code != 200 {
		return rec.Code, 0, rec.Body.String()
	}
	var a struct {
		AuthorizationURL string `json:"authorization_url"`
		State            string `json:"state"`
	}
	require.NoError(e.t, json.Unmarshal(rec.Body.Bytes(), &a))
	code, err := g.Approve(a.AuthorizationURL, u)
	require.NoError(e.t, err)
	rec = e.do(call{method: "POST", path: "/v1/auth/oauth/google/callback", token: token, body: map[string]string{"code": code, "state": a.State}})
	return 200, rec.Code, rec.Body.String()
}

func gUser(email string, verified bool) oauthtest.User {
	return oauthtest.User{Claims: map[string]any{"sub": "g-" + uuid.NewString(), "email": email, "email_verified": verified}}
}

func TestOAuthOverHTTP(t *testing.T) {
	t.Parallel()
	e, g := newOAuthHTTPEnv(t)

	_, cb, body := e.oauthFlow(g, "", gUser(newEmail(), true))
	require.Equal(t, 200, cb, body)
	var sess sessionCreatedResponse
	require.NoError(t, json.Unmarshal([]byte(body), &sess))

	rec := e.do(call{method: "GET", path: "/v1/me/identities", token: sess.Token})
	require.Equal(t, 200, rec.Code)
	assert.Contains(t, rec.Body.String(), `"provider":"google"`)

	rec = e.do(call{method: "DELETE", path: "/v1/me/identities/google", token: sess.Token})
	require.Equal(t, 409, rec.Code, "last way to sign in")
	assert.Equal(t, "conflict", errCode(t, rec))

	_, cb, body = e.oauthFlow(g, "", gUser(newEmail(), false))
	require.Equal(t, 403, cb, body)

	// Link from settings: 204, then the identity is listed.
	addr := newEmail()
	e.do(call{method: "POST", path: "/v1/auth/signup", body: map[string]string{"email": addr, "password": strongPW}})
	e.do(call{method: "POST", path: "/v1/auth/verify-email", body: map[string]string{"token": e.mailedToken(addr)}})
	rec = e.do(call{method: "POST", path: "/v1/auth/login", body: map[string]string{"email": addr, "password": strongPW}})
	var pw sessionCreatedResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &pw))
	_, cb, body = e.oauthFlow(g, pw.Token, gUser(newEmail(), false))
	require.Equal(t, 204, cb, body)
	require.Equal(t, 204, e.do(call{method: "DELETE", path: "/v1/me/identities/google", token: pw.Token}).Code, "password user may unlink")
}

func TestOAuthHTTPErrors(t *testing.T) {
	t.Parallel()
	e, g := newOAuthHTTPEnv(t)

	rec := e.do(call{method: "POST", path: "/v1/auth/oauth/google/authorize", body: map[string]string{"redirect_uri": "https://evil.test/cb"}})
	require.Equal(t, 400, rec.Code)
	rec = e.do(call{method: "POST", path: "/v1/auth/oauth/github/authorize", body: map[string]string{"redirect_uri": oauthRedirect}})
	require.Equal(t, 400, rec.Code, "provider not enabled")
	rec = e.do(call{method: "POST", path: "/v1/auth/oauth/google/authorize", token: "nfs_" + "x", body: map[string]string{"redirect_uri": oauthRedirect}})
	require.Equal(t, 401, rec.Code, "a bad session is rejected, never treated as anonymous")
	rec = e.do(call{method: "POST", path: "/v1/auth/oauth/google/callback", body: map[string]string{"code": "c", "state": "nope"}})
	require.Equal(t, 400, rec.Code)
	assert.Equal(t, "invalid_token", errCode(t, rec))

	// link_required: the email belongs to an account whose email is unverified.
	addr := newEmail()
	e.do(call{method: "POST", path: "/v1/auth/signup", body: map[string]string{"email": addr, "password": strongPW}})
	_, cb, body := e.oauthFlow(g, "", gUser(addr, true))
	require.Equal(t, 409, cb, body)
	assert.Contains(t, body, `"link_required"`)
}
