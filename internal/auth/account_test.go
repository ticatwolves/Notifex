package auth_test

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ticatwolves/notifex/internal/auth"
	"github.com/ticatwolves/notifex/internal/mail"
	"github.com/ticatwolves/notifex/internal/store"
	"github.com/ticatwolves/notifex/internal/store/entstore"
	"github.com/ticatwolves/notifex/internal/store/storetest"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type console struct {
	st       *entstore.Store
	accounts *auth.Accounts
	sessions *auth.SessionAuthenticator
	mail     *mail.Recorder
	clock    *clock
}

func newConsole(t *testing.T) console {
	t.Helper()
	c := console{
		st:    entstore.New(storetest.DB(t)),
		mail:  &mail.Recorder{},
		clock: &clock{t: time.Now().UTC().Truncate(time.Microsecond)},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c.accounts = auth.NewAccounts(c.st, auth.NewPasswordHasher(4), c.mail, log, "https://console.test", auth.DefaultSessionPolicy)
	c.accounts.SetClock(c.clock.now)
	c.sessions = auth.NewSessionAuthenticator(c.st, log, auth.DefaultSessionPolicy)
	c.sessions.SetClock(c.clock.now)
	return c
}

var tokenRE = regexp.MustCompile(`#token=([0-9A-Za-z]+)`)

func (c console) tokenFor(t *testing.T, email, subjectPart string) string {
	t.Helper()
	m, ok := c.mail.Last(email)
	require.True(t, ok, "no mail to %s", email)
	require.Contains(t, m.Subject, subjectPart)
	match := tokenRE.FindStringSubmatch(m.Text)
	require.Len(t, match, 2, "no token link in mail")
	return match[1]
}

func email() string { return "u" + strings.ReplaceAll(uuid.NewString()[:12], "-", "") + "@example.com" }

const goodPassword = "plum-orbit-candle-91"

func (c console) signupVerified(t *testing.T) string {
	t.Helper()
	e := email()
	require.NoError(t, c.accounts.Signup(context.Background(), auth.SignupInput{Email: e, Password: goodPassword}))
	require.NoError(t, c.accounts.VerifyEmail(context.Background(), c.tokenFor(t, e, "Verify")))
	return e
}

func (c console) login(t *testing.T, e string) (auth.NewSessionToken, auth.SessionPrincipal) {
	t.Helper()
	s, err := c.accounts.Login(context.Background(), auth.LoginInput{Email: e, Password: goodPassword})
	require.NoError(t, err)
	p, err := c.sessions.Authenticate(context.Background(), s.Token)
	require.NoError(t, err)
	return s, p
}

func TestSignupAndVerify(t *testing.T) {
	t.Parallel()
	c := newConsole(t)
	ctx := context.Background()
	e := email()

	require.NoError(t, c.accounts.Signup(ctx, auth.SignupInput{Email: "  " + strings.ToUpper(e) + " ", Password: goodPassword, Name: "Ada"}))
	u, err := c.st.Users().GetByEmail(ctx, e)
	require.NoError(t, err)
	assert.Nil(t, u.EmailVerifiedAt, "unverified until the link is used")
	require.NotNil(t, u.PasswordHash)
	assert.NotContains(t, *u.PasswordHash, goodPassword)

	m, _ := c.mail.Last(e)
	assert.Contains(t, m.Text, "https://console.test/verify-email#token=", "token travels in the URL fragment, never sent to servers")
	tok := c.tokenFor(t, e, "Verify")
	require.NoError(t, c.accounts.VerifyEmail(ctx, tok))
	u, _ = c.st.Users().Get(ctx, u.ID)
	assert.NotNil(t, u.EmailVerifiedAt)
	require.ErrorIs(t, c.accounts.VerifyEmail(ctx, tok), auth.ErrInvalidToken, "single use")
	require.ErrorIs(t, c.accounts.VerifyEmail(ctx, "nonsense"), auth.ErrInvalidToken)
}

func TestSignupDoesNotRevealExistingAccounts(t *testing.T) {
	t.Parallel()
	c := newConsole(t)
	ctx := context.Background()
	e := c.signupVerified(t)
	before := len(c.mail.Sent())

	err := c.accounts.Signup(ctx, auth.SignupInput{Email: e, Password: "another-strong-pass-7"})
	require.NoError(t, err, "same outcome as a new signup")
	m, ok := c.mail.Last(e)
	require.True(t, ok)
	assert.Contains(t, m.Subject, "already have", "the real owner is told instead")
	assert.NotContains(t, m.Text, "#token=", "no usable token for whoever triggered it")
	assert.Len(t, c.mail.Sent(), before+1)
}

func TestSignupValidation(t *testing.T) {
	t.Parallel()
	c := newConsole(t)
	ctx := context.Background()
	require.ErrorIs(t, c.accounts.Signup(ctx, auth.SignupInput{Email: "not-an-email", Password: goodPassword}), auth.ErrInvalidEmail)
	require.ErrorIs(t, c.accounts.Signup(ctx, auth.SignupInput{Email: "Ada <ada@example.com>", Password: goodPassword}), auth.ErrInvalidEmail, "display names are not accepted")
	require.ErrorIs(t, c.accounts.Signup(ctx, auth.SignupInput{Email: email(), Password: "password1234"}), auth.ErrWeakPassword)
	assert.Empty(t, c.mail.Sent())
}

func TestLoginAndSessionLifecycle(t *testing.T) {
	t.Parallel()
	c := newConsole(t)
	ctx := context.Background()
	e := c.signupVerified(t)

	s, p := c.login(t, e)
	assert.True(t, strings.HasPrefix(s.Token, "nfs_"))
	assert.Len(t, s.Token, 4+43)
	assert.True(t, p.EmailVerified)
	assert.True(t, p.RecentlyAuthenticated(c.clock.now(), 10*time.Minute), "a fresh login counts as recent authentication")

	// Idle expiry: activity keeps it alive, 12 h of silence ends it.
	c.clock.advance(11 * time.Hour)
	_, err := c.sessions.Authenticate(ctx, s.Token)
	require.NoError(t, err)
	c.clock.advance(11 * time.Hour)
	_, err = c.sessions.Authenticate(ctx, s.Token)
	require.NoError(t, err, "touched 11 h ago")
	c.clock.advance(12 * time.Hour)
	_, err = c.sessions.Authenticate(ctx, s.Token)
	require.ErrorIs(t, err, auth.ErrUnauthenticated, "idle for 12 h")

	// Absolute expiry: 7 days even with constant activity.
	s2, _ := c.login(t, e)
	for range 15 { // 15 × 11 h = 165 h: still under 168 h
		c.clock.advance(11 * time.Hour)
		_, err = c.sessions.Authenticate(ctx, s2.Token)
		require.NoError(t, err, "active and within 7 days")
	}
	c.clock.advance(11 * time.Hour) // 176 h
	_, err = c.sessions.Authenticate(ctx, s2.Token)
	require.ErrorIs(t, err, auth.ErrUnauthenticated, "past the 7-day absolute limit despite activity")

	for _, bad := range []string{"", "nfs_short", "nfx_" + strings.Repeat("a", 43), "nfs_" + strings.Repeat("!", 43), "nfs_" + strings.Repeat("a", 43)} {
		_, err := c.sessions.Authenticate(ctx, bad)
		require.ErrorIs(t, err, auth.ErrUnauthenticated, bad)
	}
}

func TestLoginFailuresAreUniformAndBackOff(t *testing.T) {
	t.Parallel()
	c := newConsole(t)
	ctx := context.Background()
	e := c.signupVerified(t)

	_, err := c.accounts.Login(ctx, auth.LoginInput{Email: e, Password: "wrong-password-123"})
	require.ErrorIs(t, err, auth.ErrInvalidCredentials)
	_, err = c.accounts.Login(ctx, auth.LoginInput{Email: email(), Password: goodPassword})
	require.ErrorIs(t, err, auth.ErrInvalidCredentials, "unknown email: same error")
	_, err = c.accounts.Login(ctx, auth.LoginInput{Email: "garbage", Password: goodPassword})
	require.ErrorIs(t, err, auth.ErrInvalidCredentials, "malformed email: same error")

	for range 4 {
		_, err = c.accounts.Login(ctx, auth.LoginInput{Email: e, Password: "wrong-password-123"})
		require.ErrorIs(t, err, auth.ErrInvalidCredentials)
	}
	_, err = c.accounts.Login(ctx, auth.LoginInput{Email: e, Password: goodPassword})
	var locked *auth.LockedError
	require.ErrorAs(t, err, &locked, "backoff applies even to the right password")
	assert.Positive(t, locked.RetryAfter)

	c.clock.advance(locked.RetryAfter)
	_, err = c.accounts.Login(ctx, auth.LoginInput{Email: e, Password: goodPassword})
	require.NoError(t, err, "allowed after the backoff; success resets the counter")
	_, err = c.accounts.Login(ctx, auth.LoginInput{Email: e, Password: "wrong-password-123"})
	require.ErrorIs(t, err, auth.ErrInvalidCredentials, "counter was reset")

	// Unknown emails back off too, so backoff can't be used to find accounts.
	ghost := email()
	for range 5 {
		_, _ = c.accounts.Login(ctx, auth.LoginInput{Email: ghost, Password: goodPassword})
	}
	_, err = c.accounts.Login(ctx, auth.LoginInput{Email: ghost, Password: goodPassword})
	require.ErrorAs(t, err, &locked)
}

func TestLogoutListAndRevokeSessions(t *testing.T) {
	t.Parallel()
	c := newConsole(t)
	ctx := context.Background()
	e := c.signupVerified(t)
	s1, p1 := c.login(t, e)
	s2, _ := c.login(t, e)

	ss, err := c.accounts.ListSessions(ctx, p1)
	require.NoError(t, err)
	assert.Len(t, ss, 2)

	require.NoError(t, c.accounts.RevokeSession(ctx, p1, s2.Session.ID))
	_, err = c.sessions.Authenticate(ctx, s2.Token)
	require.ErrorIs(t, err, auth.ErrUnauthenticated, "revocation is immediate")

	other := c.signupVerified(t)
	_, otherP := c.login(t, other)
	require.ErrorIs(t, c.accounts.RevokeSession(ctx, otherP, s1.Session.ID), store.ErrNotFound, "can't revoke someone else's session")

	require.NoError(t, c.accounts.Logout(ctx, p1))
	_, err = c.sessions.Authenticate(ctx, s1.Token)
	require.ErrorIs(t, err, auth.ErrUnauthenticated)
}

func TestReauthenticate(t *testing.T) {
	t.Parallel()
	c := newConsole(t)
	ctx := context.Background()
	e := c.signupVerified(t)
	s, _ := c.login(t, e)

	c.clock.advance(11 * time.Minute)
	p, err := c.sessions.Authenticate(ctx, s.Token)
	require.NoError(t, err)
	assert.False(t, p.RecentlyAuthenticated(c.clock.now(), 10*time.Minute))

	require.ErrorIs(t, c.accounts.Reauthenticate(ctx, p, "wrong-password-123"), auth.ErrInvalidCredentials)
	require.NoError(t, c.accounts.Reauthenticate(ctx, p, goodPassword))
	p, err = c.sessions.Authenticate(ctx, s.Token)
	require.NoError(t, err)
	assert.True(t, p.RecentlyAuthenticated(c.clock.now(), 10*time.Minute))
}

func TestForgotAndResetPassword(t *testing.T) {
	t.Parallel()
	c := newConsole(t)
	ctx := context.Background()
	e := c.signupVerified(t)
	s1, _ := c.login(t, e)
	s2, _ := c.login(t, e)

	require.NoError(t, c.accounts.ForgotPassword(ctx, email()), "unknown email: same response")
	require.NoError(t, c.accounts.ForgotPassword(ctx, e))
	tok := c.tokenFor(t, e, "Reset")

	require.ErrorIs(t, c.accounts.ResetPassword(ctx, tok, "password1234"), auth.ErrWeakPassword)
	const newPw = "glacier-tuba-mosaic-44"
	require.NoError(t, c.accounts.ResetPassword(ctx, tok, newPw), "a rejected password did not burn the token")
	require.ErrorIs(t, c.accounts.ResetPassword(ctx, tok, newPw), auth.ErrInvalidToken, "single use")

	for _, s := range []auth.NewSessionToken{s1, s2} {
		_, err := c.sessions.Authenticate(ctx, s.Token)
		require.ErrorIs(t, err, auth.ErrUnauthenticated, "every session revoked on reset")
	}
	_, err := c.accounts.Login(ctx, auth.LoginInput{Email: e, Password: goodPassword})
	require.ErrorIs(t, err, auth.ErrInvalidCredentials, "old password no longer works")
	_, err = c.accounts.Login(ctx, auth.LoginInput{Email: e, Password: newPw})
	require.NoError(t, err)
}

func TestForgotPasswordIsMailBombLimited(t *testing.T) {
	t.Parallel()
	c := newConsole(t)
	ctx := context.Background()
	e := c.signupVerified(t)
	start := len(c.mail.Sent())
	for range 10 {
		require.NoError(t, c.accounts.ForgotPassword(ctx, e))
	}
	assert.Equal(t, 3, len(c.mail.Sent())-start, "at most 3 reset emails per address per hour")
	c.clock.advance(61 * time.Minute)
	require.NoError(t, c.accounts.ForgotPassword(ctx, e))
	assert.Equal(t, 4, len(c.mail.Sent())-start, "the window resets")
}

func TestNewResetInvalidatesOlderLinks(t *testing.T) {
	t.Parallel()
	c := newConsole(t)
	ctx := context.Background()
	e := c.signupVerified(t)
	require.NoError(t, c.accounts.ForgotPassword(ctx, e))
	first := c.tokenFor(t, e, "Reset")
	require.NoError(t, c.accounts.ForgotPassword(ctx, e))
	second := c.tokenFor(t, e, "Reset")
	require.ErrorIs(t, c.accounts.ResetPassword(ctx, first, "glacier-tuba-mosaic-44"), auth.ErrInvalidToken)
	require.NoError(t, c.accounts.ResetPassword(ctx, second, "glacier-tuba-mosaic-44"))
}

func TestGetMeListsActiveTenants(t *testing.T) {
	t.Parallel()
	c := newConsole(t)
	ctx := context.Background()
	e := c.signupVerified(t)
	_, p := c.login(t, e)

	me, err := c.accounts.GetMe(ctx, p)
	require.NoError(t, err)
	assert.Equal(t, e, me.User.Email)
	assert.Empty(t, me.Memberships, "signup creates no tenant (ADR-011 §2)")
}

func TestSecurityEventsAreAudited(t *testing.T) {
	t.Parallel()
	c := newConsole(t)
	ctx := context.Background()
	e := c.signupVerified(t)
	_, p := c.login(t, e)
	_, _ = c.accounts.Login(ctx, auth.LoginInput{Email: e, Password: "wrong-password-123"})
	require.NoError(t, c.accounts.Logout(ctx, p))

	rows, err := storetest.DB(t).QueryContext(ctx,
		`SELECT action, entity_id, coalesce(after::text, '') FROM audit_events WHERE actor_id = $1 OR entity_id = $1 ORDER BY created_at`, p.UserID.String())
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var actions []string
	for rows.Next() {
		var action, entity, after string
		require.NoError(t, rows.Scan(&action, &entity, &after))
		actions = append(actions, action)
		assert.NotContains(t, after, goodPassword)
		assert.NotContains(t, entity, e, "raw email never stored in audit")
	}
	require.NoError(t, rows.Err())
	for _, want := range []string{"user.signup", "user.email_verified", "session.login", "session.login_failed", "session.logout"} {
		assert.Contains(t, actions, want)
	}
}

func storetestDB(t *testing.T) *sql.DB { return storetest.DB(t) }
