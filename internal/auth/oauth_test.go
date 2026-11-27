package auth_test

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ticatwolves/notifex/internal/auth"
	"github.com/ticatwolves/notifex/internal/auth/oauthtest"
	"github.com/ticatwolves/notifex/internal/store"
)

const redirect = "https://console.test/oauth/callback"

type oauthEnv struct {
	console
	oauth                  *auth.OAuth
	google, microsoft, git *oauthtest.Server
}

func newOAuthEnv(t *testing.T) oauthEnv {
	t.Helper()
	c := newConsole(t)
	e := oauthEnv{console: c, google: oauthtest.New(false), microsoft: oauthtest.New(true), git: oauthtest.New(false)}
	t.Cleanup(func() { e.google.Close(); e.microsoft.Close(); e.git.Close() })
	creds := auth.ClientCredentials{ID: oauthtest.ClientID, Secret: oauthtest.ClientSecret}
	ctx := context.Background()
	oidcEP := func(s *oauthtest.Server) auth.OIDCEndpoints {
		return auth.OIDCEndpoints{AuthURL: s.URL + "/authorize", TokenURL: s.URL + "/token", JWKSURL: s.URL + "/jwks", Issuer: s.Issuer()}
	}
	e.oauth = auth.NewOAuth(c.accounts, []auth.Provider{
		auth.NewGoogle(ctx, creds, oidcEP(e.google)),
		auth.NewMicrosoft(ctx, creds, oidcEP(e.microsoft)),
		auth.NewGitHub(creds, auth.GitHubEndpoints{AuthURL: e.git.URL + "/authorize", TokenURL: e.git.URL + "/token", APIURL: e.git.URL}),
	}, []string{redirect})
	e.oauth.SetClock(c.clock.now)
	return e
}

func (e oauthEnv) server(provider string) *oauthtest.Server {
	switch provider {
	case "google":
		return e.google
	case "microsoft":
		return e.microsoft
	default:
		return e.git
	}
}

// signIn runs authorize -> consent -> callback.
func (e oauthEnv) signIn(t *testing.T, provider string, u oauthtest.User, session *auth.SessionPrincipal) (auth.CallbackResult, error) {
	t.Helper()
	authURL, state, err := e.oauth.Authorize(context.Background(), provider, redirect, session)
	require.NoError(t, err)
	code, err := e.server(provider).Approve(authURL, u)
	require.NoError(t, err)
	return e.oauth.Callback(context.Background(), provider, code, state, session, "test")
}

func googleUser(sub, email string, verified bool) oauthtest.User {
	return oauthtest.User{Claims: map[string]any{"sub": sub, "email": email, "email_verified": verified, "name": "G User"}}
}

func msUser(tid, oid, email string, edov any) oauthtest.User {
	c := map[string]any{"sub": "pairwise-" + oid, "tid": tid, "oid": oid, "email": email, "name": "M User"}
	if edov != nil {
		c["xms_edov"] = edov
	}
	return oauthtest.User{Claims: c}
}

func (e oauthEnv) userCount(t *testing.T, email string) int {
	t.Helper()
	_, err := e.st.Users().GetByEmail(context.Background(), email)
	if err != nil {
		require.ErrorIs(t, err, store.ErrNotFound)
		return 0
	}
	return 1
}

func TestOAuthNewUserThenReturningUser(t *testing.T) {
	t.Parallel()
	e := newOAuthEnv(t)
	ctx := context.Background()
	em := email()

	res, err := e.signIn(t, "google", googleUser("g-"+uuid.NewString(), em, true), nil)
	require.NoError(t, err)
	require.NotNil(t, res.Session)
	p, err := e.sessions.Authenticate(ctx, res.Session.Token)
	require.NoError(t, err)
	assert.True(t, p.EmailVerified, "provider-verified email is trusted for a new account")
	u, err := e.st.Users().Get(ctx, p.UserID)
	require.NoError(t, err)
	assert.Nil(t, u.PasswordHash, "OAuth-only account has no password")

	sub := "g-" + uuid.NewString()
	first, err := e.signIn(t, "google", googleUser(sub, email(), true), nil)
	require.NoError(t, err)
	// Same subject, different email at the provider: still the same account,
	// because identities are keyed on the subject, never the email.
	again, err := e.signIn(t, "google", googleUser(sub, email(), true), nil)
	require.NoError(t, err)
	assert.Equal(t, first.Session.Session.UserID, again.Session.Session.UserID)
}

func TestOAuthUnverifiedEmailNeverLooksUpAccounts(t *testing.T) {
	t.Parallel()
	e := newOAuthEnv(t)
	victim := e.signupVerified(t)
	nobody := email()

	_, errExisting := e.signIn(t, "google", googleUser("g-"+uuid.NewString(), victim, false), nil)
	_, errNew := e.signIn(t, "google", googleUser("g-"+uuid.NewString(), nobody, false), nil)
	require.ErrorIs(t, errExisting, auth.ErrProviderEmailUnverified)
	require.ErrorIs(t, errNew, auth.ErrProviderEmailUnverified)
	assert.Equal(t, errExisting.Error(), errNew.Error(), "same outcome whether or not an account exists")
	assert.Zero(t, e.userCount(t, nobody), "no account created from an unverified email")
}

func TestOAuthLinksVerifiedEmailOnlyToVerifiedAccount(t *testing.T) {
	t.Parallel()
	e := newOAuthEnv(t)
	ctx := context.Background()

	verified := e.signupVerified(t)
	res, err := e.signIn(t, "google", googleUser("g-"+uuid.NewString(), verified, true), nil)
	require.NoError(t, err, "verified on both sides: auto-link")
	u, _ := e.st.Users().GetByEmail(ctx, verified)
	assert.Equal(t, u.ID, res.Session.Session.UserID)
	ids, err := e.st.Identities().ListForUser(ctx, u.ID)
	require.NoError(t, err)
	assert.Len(t, ids, 1)

	// Account exists but its email was never verified: someone may have
	// squatted the address, so never auto-link.
	unverified := email()
	require.NoError(t, e.accounts.Signup(ctx, auth.SignupInput{Email: unverified, Password: goodPassword}))
	_, err = e.signIn(t, "google", googleUser("g-"+uuid.NewString(), unverified, true), nil)
	require.ErrorIs(t, err, auth.ErrLinkRequired)
}

func TestOAuthMicrosoftTrustsEmailOnlyWithEDOV(t *testing.T) {
	t.Parallel()
	e := newOAuthEnv(t)
	ctx := context.Background()
	tid := uuid.NewString()

	// nOAuth: an attacker's tenant can put any address in `email`. Without
	// xms_edov it must not reach (or create) an account by email.
	victim := e.signupVerified(t)
	_, err := e.signIn(t, "microsoft", msUser(tid, uuid.NewString(), victim, nil), nil)
	require.ErrorIs(t, err, auth.ErrProviderEmailUnverified)
	_, err = e.signIn(t, "microsoft", msUser(tid, uuid.NewString(), victim, false), nil)
	require.ErrorIs(t, err, auth.ErrProviderEmailUnverified)

	for _, edov := range []any{true, "1"} {
		em := email()
		oid := uuid.NewString()
		res, err := e.signIn(t, "microsoft", msUser(tid, oid, em, edov), nil)
		require.NoError(t, err, "xms_edov=%v", edov)
		ids, err := e.st.Identities().ListForUser(ctx, res.Session.Session.UserID)
		require.NoError(t, err)
		require.Len(t, ids, 1)
		assert.Equal(t, tid+":"+oid, ids[0].Subject, "keyed on tid:oid, not sub or email")
	}

	u := msUser(tid, uuid.NewString(), email(), true)
	u.Tamper = oauthtest.WrongTenantIssuer
	_, err = e.signIn(t, "microsoft", u, nil)
	require.ErrorIs(t, err, auth.ErrInvalidToken, "issuer must match the token's tenant")
}

func TestOAuthRejectsBadTokens(t *testing.T) {
	t.Parallel()
	e := newOAuthEnv(t)
	for name, tamper := range map[string]oauthtest.Tamper{
		"wrong audience": oauthtest.WrongAudience,
		"wrong nonce":    oauthtest.WrongNonce,
		"expired":        oauthtest.Expired,
		"foreign key":    oauthtest.ForeignKey,
		"no id_token":    oauthtest.NoIDToken,
	} {
		t.Run(name, func(t *testing.T) {
			em := email()
			u := googleUser("g-"+uuid.NewString(), em, true)
			u.Tamper = tamper
			_, err := e.signIn(t, "google", u, nil)
			require.ErrorIs(t, err, auth.ErrInvalidToken)
			assert.Zero(t, e.userCount(t, em), "nothing created from a bad token")
		})
	}
}

func TestOAuthStateAndPKCE(t *testing.T) {
	t.Parallel()
	e := newOAuthEnv(t)
	ctx := context.Background()
	u := googleUser("g-"+uuid.NewString(), email(), true)

	_, _, err := e.oauth.Authorize(ctx, "google", "https://evil.test/cb", nil)
	require.ErrorIs(t, err, auth.ErrRedirectNotAllowed)
	_, _, err = e.oauth.Authorize(ctx, "facebook", redirect, nil)
	require.ErrorIs(t, err, auth.ErrProviderNotEnabled)

	authURL, state, err := e.oauth.Authorize(ctx, "google", redirect, nil)
	require.NoError(t, err)
	q, _ := url.Parse(authURL)
	assert.Equal(t, "S256", q.Query().Get("code_challenge_method"))
	assert.Equal(t, redirect, q.Query().Get("redirect_uri"))
	assert.NotEmpty(t, q.Query().Get("nonce"))
	code, err := e.google.Approve(authURL, u)
	require.NoError(t, err)

	_, err = e.oauth.Callback(ctx, "github", code, state, nil, "")
	require.ErrorIs(t, err, auth.ErrInvalidToken, "state is bound to its provider")
	_, err = e.oauth.Callback(ctx, "google", code, state, nil, "")
	require.NoError(t, err)
	_, err = e.oauth.Callback(ctx, "google", code, state, nil, "")
	require.ErrorIs(t, err, auth.ErrInvalidToken, "state is single use")

	// Expired state.
	authURL, state, err = e.oauth.Authorize(ctx, "google", redirect, nil)
	require.NoError(t, err)
	code, _ = e.google.Approve(authURL, u)
	e.clock.advance(auth.OAuthStateTTL + time.Second)
	_, err = e.oauth.Callback(ctx, "google", code, state, nil, "")
	require.ErrorIs(t, err, auth.ErrInvalidToken)

	// PKCE: an intercepted code is useless with a different challenge.
	authURL, state, err = e.oauth.Authorize(ctx, "google", redirect, nil)
	require.NoError(t, err)
	tampered := strings.Replace(authURL, "code_challenge=", "code_challenge=x", 1)
	code, err = e.google.Approve(tampered, u)
	require.NoError(t, err)
	_, err = e.oauth.Callback(ctx, "google", code, state, nil, "")
	require.ErrorIs(t, err, auth.ErrInvalidToken)
}

func TestOAuthGitHubUsesPrimaryVerifiedEmail(t *testing.T) {
	t.Parallel()
	e := newOAuthEnv(t)
	gh := func(emails ...oauthtest.GitHubEmail) oauthtest.User {
		return oauthtest.User{GitHubID: time.Now().UnixNano() % 1_000_000_000, GitHubEmails: emails}
	}
	primary := email()
	res, err := e.signIn(t, "github", gh(oauthtest.GitHubEmail{Email: email(), Verified: true}, oauthtest.GitHubEmail{Email: primary, Primary: true, Verified: true}), nil)
	require.NoError(t, err)
	u, err := e.st.Users().Get(context.Background(), res.Session.Session.UserID)
	require.NoError(t, err)
	assert.Equal(t, primary, u.Email)

	_, err = e.signIn(t, "github", gh(oauthtest.GitHubEmail{Email: email(), Primary: true, Verified: false}), nil)
	require.ErrorIs(t, err, auth.ErrProviderEmailUnverified, "unverified primary")
	_, err = e.signIn(t, "github", gh(oauthtest.GitHubEmail{Email: email(), Verified: true}), nil)
	require.ErrorIs(t, err, auth.ErrProviderEmailUnverified, "a verified non-primary email is not used")
}

func TestOAuthLinkFromSettingsAndLinkingCSRF(t *testing.T) {
	t.Parallel()
	e := newOAuthEnv(t)
	ctx := context.Background()
	_, alice := e.login(t, e.signupVerified(t))
	_, bob := e.login(t, e.signupVerified(t))

	// Linking works even when the provider email is unverified or different:
	// the signed-in session proves who is linking.
	attackerAcct := googleUser("g-attacker-"+uuid.NewString(), email(), false)
	res, err := e.signIn(t, "google", attackerAcct, &alice)
	require.NoError(t, err)
	assert.True(t, res.Linked)

	// Linking CSRF: a link started by Bob can't be completed in Alice's
	// browser (or without a session) to attach Bob's provider to Alice.
	authURL, state, err := e.oauth.Authorize(ctx, "github", redirect, &bob)
	require.NoError(t, err)
	code, err := e.git.Approve(authURL, oauthtest.User{GitHubID: 4242, GitHubEmails: nil})
	require.NoError(t, err)
	_, err = e.oauth.Callback(ctx, "github", code, state, &alice, "")
	require.ErrorIs(t, err, auth.ErrInvalidToken)
	aliceIDs, _ := e.st.Identities().ListForUser(ctx, alice.UserID)
	assert.Len(t, aliceIDs, 1, "nothing attached to Alice")

	authURL, state, _ = e.oauth.Authorize(ctx, "github", redirect, &bob)
	code, _ = e.git.Approve(authURL, oauthtest.User{GitHubID: 4243})
	_, err = e.oauth.Callback(ctx, "github", code, state, nil, "")
	require.ErrorIs(t, err, auth.ErrInvalidToken, "a link can't complete without the session that started it")

	// An identity already linked to Alice can't be linked to Bob.
	_, err = e.signIn(t, "google", attackerAcct, &bob)
	require.ErrorIs(t, err, auth.ErrIdentityInUse)
	// Re-linking to the same user is idempotent.
	_, err = e.signIn(t, "google", attackerAcct, &alice)
	require.NoError(t, err)
}

func TestOAuthUnlinkKeepsAWayToSignIn(t *testing.T) {
	t.Parallel()
	e := newOAuthEnv(t)
	ctx := context.Background()

	res, err := e.signIn(t, "google", googleUser("g-"+uuid.NewString(), email(), true), nil)
	require.NoError(t, err)
	p, err := e.sessions.Authenticate(ctx, res.Session.Token)
	require.NoError(t, err)
	require.ErrorIs(t, e.oauth.Unlink(ctx, p, "google"), auth.ErrLastLoginMethod, "OAuth-only account with one identity")

	_, err = e.signIn(t, "github", oauthtest.User{GitHubID: time.Now().UnixNano() % 1e9, GitHubEmails: []oauthtest.GitHubEmail{{Email: email(), Primary: true, Verified: true}}}, &p)
	require.NoError(t, err)
	require.NoError(t, e.oauth.Unlink(ctx, p, "google"), "another identity remains")
	ids, _ := e.oauth.ListIdentities(ctx, p)
	require.Len(t, ids, 1)
	assert.Equal(t, store.ProviderGitHub, ids[0].Provider)
	require.ErrorIs(t, e.oauth.Unlink(ctx, p, "google"), store.ErrNotFound)

	// Password users can remove their only identity.
	pw := e.signupVerified(t)
	_, pp := e.login(t, pw)
	_, err = e.signIn(t, "google", googleUser("g-"+uuid.NewString(), email(), false), &pp)
	require.NoError(t, err)
	require.NoError(t, e.oauth.Unlink(ctx, pp, "google"))
}

func TestOAuthEventsAreAudited(t *testing.T) {
	t.Parallel()
	e := newOAuthEnv(t)
	ctx := context.Background()
	res, err := e.signIn(t, "google", googleUser("g-"+uuid.NewString(), email(), true), nil)
	require.NoError(t, err)
	uid := res.Session.Session.UserID.String()
	// User-level events carry no tenant, so read the table directly.
	var actions []string
	r, err := storetestDB(t).QueryContext(ctx, `SELECT action, coalesce(after->>'method', '') FROM audit_events WHERE actor_id = $1 ORDER BY created_at`, uid)
	require.NoError(t, err)
	defer func() { _ = r.Close() }()
	for r.Next() {
		var a, m string
		require.NoError(t, r.Scan(&a, &m))
		actions = append(actions, a+"/"+m)
	}
	require.NoError(t, r.Err())
	assert.Contains(t, actions, "user.signup/oauth:google")
	assert.Contains(t, actions, "identity.link/")
	assert.Contains(t, actions, "session.login/oauth:google")
}
