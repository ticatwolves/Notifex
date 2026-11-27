package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ticatwolves/notifex/internal/audit"
	nmail "github.com/ticatwolves/notifex/internal/mail"
	"github.com/ticatwolves/notifex/internal/observability"
	"github.com/ticatwolves/notifex/internal/store"
)

// Account errors. Transports map them to the docs/03 error codes.
var (
	ErrInvalidCredentials = errors.New("auth: invalid credentials")
	ErrInvalidToken       = errors.New("auth: invalid or expired token")
	ErrInvalidEmail       = errors.New("auth: invalid email address")
)

// LockedError means the key is in backoff until RetryAfter.
type LockedError struct{ RetryAfter time.Duration }

func (e *LockedError) Error() string {
	return fmt.Sprintf("auth: too many attempts; retry after %s", e.RetryAfter.Round(time.Second))
}

// SessionPolicy holds the ADR-011 lifetimes.
type SessionPolicy struct {
	Idle          time.Duration
	Absolute      time.Duration
	TouchInterval time.Duration
	ReauthWindow  time.Duration
	VerifyTTL     time.Duration
	ResetTTL      time.Duration
	Login         store.ThrottlePolicy
	// ForgotPerHour caps reset emails per address (mail-bomb protection).
	ForgotPerHour int
}

// DefaultSessionPolicy is ADR-011 §1–2.
var DefaultSessionPolicy = SessionPolicy{
	Idle:          12 * time.Hour,
	Absolute:      7 * 24 * time.Hour,
	TouchInterval: time.Minute,
	ReauthWindow:  10 * time.Minute,
	VerifyTTL:     24 * time.Hour,
	ResetTTL:      time.Hour,
	Login:         store.ThrottlePolicy{FreeFailures: 5, Window: 15 * time.Minute, MaxBackoff: 15 * time.Minute},
	ForgotPerHour: 3,
}

// Accounts implements console signup, login and credential recovery.
type Accounts struct {
	st         store.Store
	hasher     *PasswordHasher
	mailer     nmail.Mailer
	log        *slog.Logger
	consoleURL string
	policy     SessionPolicy
	now        func() time.Time
}

// NewAccounts builds the account service. consoleURL is the web app's base
// URL used in emailed links.
func NewAccounts(st store.Store, h *PasswordHasher, m nmail.Mailer, log *slog.Logger, consoleURL string, p SessionPolicy) *Accounts {
	return &Accounts{st: st, hasher: h, mailer: m, log: log, consoleURL: strings.TrimRight(consoleURL, "/"), policy: p, now: time.Now}
}

// SetClock overrides the clock (tests).
func (a *Accounts) SetClock(now func() time.Time) { a.now = now }

// SignupInput is a new account request.
type SignupInput struct {
	Email    string
	Password string
	Name     string
}

// Signup creates an unverified account and emails a verification link. If the
// email is already registered it emails the owner instead and returns nil, so
// the response never reveals whether an account exists.
func (a *Accounts) Signup(ctx context.Context, in SignupInput) error {
	email, err := normalizeEmail(in.Email)
	if err != nil {
		return err
	}
	if err := ValidatePassword(in.Password, email); err != nil {
		return err
	}
	// Hash before the existence check so both paths cost the same.
	hash, err := a.hasher.Hash(ctx, in.Password)
	if err != nil {
		return err
	}
	if _, err := a.st.Users().GetByEmail(ctx, email); err == nil {
		a.sendAlreadyRegistered(ctx, email)
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("signup: %w", err)
	}

	now := a.now()
	token, tokenHash := newEmailToken()
	err = a.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		u, err := tx.Users().Create(ctx, store.NewUser{Email: email, Name: in.Name, PasswordHash: &hash})
		if err != nil {
			return err
		}
		if err := tx.EmailTokens().Create(ctx, u.ID, store.TokenVerifyEmail, tokenHash, now, now.Add(a.policy.VerifyTTL)); err != nil {
			return err
		}
		return audit.Record(ctx, tx, userActor(u.ID), audit.Event{Action: "user.signup", EntityType: "user", EntityID: u.ID.String()})
	})
	if errors.Is(err, store.ErrConflict) { // lost a race with a concurrent signup
		a.sendAlreadyRegistered(ctx, email)
		return nil
	}
	if err != nil {
		return fmt.Errorf("signup: %w", err)
	}
	a.send(ctx, nmail.Message{
		To:      email,
		Subject: "Verify your Notifex email",
		Text: "Confirm your email address to finish setting up your Notifex account:\n\n" +
			a.consoleURL + "/verify-email#token=" + token + "\n\n" +
			"The link expires in 24 hours. If you didn't sign up, ignore this email.\n",
	})
	return nil
}

// VerifyEmail consumes a verification token.
func (a *Accounts) VerifyEmail(ctx context.Context, token string) error {
	now := a.now()
	return a.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		uid, err := tx.EmailTokens().Consume(ctx, store.TokenVerifyEmail, hashToken(token), now)
		if errors.Is(err, store.ErrNotFound) {
			return ErrInvalidToken
		}
		if err != nil {
			return fmt.Errorf("verify email: %w", err)
		}
		if err := tx.Users().MarkEmailVerified(ctx, uid, now); err != nil {
			return fmt.Errorf("verify email: %w", err)
		}
		return audit.Record(ctx, tx, userActor(uid), audit.Event{Action: "user.email_verified", EntityType: "user", EntityID: uid.String()})
	})
}

// LoginInput is a password login attempt.
type LoginInput struct {
	Email     string
	Password  string
	UserAgent string
}

// NewSessionToken is a freshly created session. Token is shown once.
type NewSessionToken struct {
	Token   string
	Session store.Session
}

// Login verifies credentials and creates a session. Every credential failure
// is ErrInvalidCredentials; repeated failures for the same email return
// *LockedError (progressive backoff, never a hard lock).
func (a *Accounts) Login(ctx context.Context, in LoginInput) (NewSessionToken, error) {
	email, err := normalizeEmail(in.Email)
	if err != nil {
		a.hasher.VerifyDummy(ctx, in.Password)
		return NewSessionToken{}, ErrInvalidCredentials
	}
	key := throttleKey("login", email)
	now := a.now()
	if until, err := a.st.Throttle().BlockedUntil(ctx, key, now); err != nil {
		return NewSessionToken{}, fmt.Errorf("login: %w", err)
	} else if !until.IsZero() {
		return NewSessionToken{}, &LockedError{RetryAfter: until.Sub(now)}
	}

	u, err := a.st.Users().GetByEmail(ctx, email)
	switch {
	case errors.Is(err, store.ErrNotFound):
		a.hasher.VerifyDummy(ctx, in.Password)
		return NewSessionToken{}, a.loginFailed(ctx, key, email, nil, now)
	case err != nil:
		return NewSessionToken{}, fmt.Errorf("login: %w", err)
	}
	if u.PasswordHash == nil || u.Status != store.UserActive {
		a.hasher.VerifyDummy(ctx, in.Password)
		return NewSessionToken{}, a.loginFailed(ctx, key, email, &u.ID, now)
	}
	ok, rehash, err := a.hasher.Verify(ctx, *u.PasswordHash, in.Password)
	if err != nil {
		return NewSessionToken{}, fmt.Errorf("login: %w", err)
	}
	if !ok {
		return NewSessionToken{}, a.loginFailed(ctx, key, email, &u.ID, now)
	}

	var out NewSessionToken
	err = a.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.Throttle().Reset(ctx, key); err != nil {
			return err
		}
		if rehash {
			h, err := a.hasher.Hash(ctx, in.Password)
			if err != nil {
				return err
			}
			if err := tx.Users().SetPasswordHash(ctx, u.ID, h); err != nil {
				return err
			}
		}
		var err error
		out, err = a.createSession(ctx, tx, u.ID, in.UserAgent, now, "password")
		return err
	})
	if err != nil {
		return NewSessionToken{}, fmt.Errorf("login: %w", err)
	}
	return out, nil
}

// createSession issues a session for userID inside tx and audits the login.
func (a *Accounts) createSession(ctx context.Context, tx store.Store, userID uuid.UUID, userAgent string, now time.Time, method string) (NewSessionToken, error) {
	token, tokenHash := newSessionToken()
	s, err := tx.Sessions().Create(ctx, store.NewSession{
		UserID: userID, TokenHash: tokenHash, Now: now, AbsoluteExpiresAt: now.Add(a.policy.Absolute), UserAgent: userAgent,
	})
	if err != nil {
		return NewSessionToken{}, err
	}
	err = audit.Record(ctx, tx, userActor(userID), audit.Event{
		Action: "session.login", EntityType: "session", EntityID: s.ID.String(),
		After: map[string]any{"method": method},
	})
	if err != nil {
		return NewSessionToken{}, err
	}
	return NewSessionToken{Token: token, Session: s}, nil
}

func (a *Accounts) loginFailed(ctx context.Context, key, email string, uid *uuid.UUID, now time.Time) error {
	if _, err := a.st.Throttle().RecordFailure(ctx, key, now, a.policy.Login); err != nil {
		return fmt.Errorf("login: record failure: %w", err)
	}
	entityID := observability.MaskEmail(email)
	if uid != nil {
		entityID = uid.String()
	}
	if err := audit.Record(ctx, a.st, audit.System, audit.Event{
		Action: "session.login_failed", EntityType: "user", EntityID: entityID,
	}); err != nil {
		a.log.WarnContext(ctx, "audit login failure", slog.Any("error", err))
	}
	return ErrInvalidCredentials
}

// Logout revokes the caller's session.
func (a *Accounts) Logout(ctx context.Context, p SessionPrincipal) error {
	return a.revoke(ctx, p, p.SessionID, "session.logout")
}

// RevokeSession revokes one of the caller's sessions.
func (a *Accounts) RevokeSession(ctx context.Context, p SessionPrincipal, id uuid.UUID) error {
	return a.revoke(ctx, p, id, "session.revoke")
}

func (a *Accounts) revoke(ctx context.Context, p SessionPrincipal, id uuid.UUID, action string) error {
	return a.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.Sessions().Revoke(ctx, p.UserID, id, a.now()); err != nil {
			return err
		}
		return audit.Record(ctx, tx, userActor(p.UserID), audit.Event{Action: action, EntityType: "session", EntityID: id.String()})
	})
}

// ListSessions returns the caller's active sessions.
func (a *Accounts) ListSessions(ctx context.Context, p SessionPrincipal) ([]store.Session, error) {
	ss, err := a.st.Sessions().ListActive(ctx, p.UserID, a.now(), a.policy.Idle)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	return ss, nil
}

// Reauthenticate confirms the caller's password and opens the re-auth window
// for sensitive actions. Failures share the login backoff for that account.
func (a *Accounts) Reauthenticate(ctx context.Context, p SessionPrincipal, password string) error {
	key := throttleKey("login", p.Email)
	now := a.now()
	if until, err := a.st.Throttle().BlockedUntil(ctx, key, now); err != nil {
		return fmt.Errorf("reauth: %w", err)
	} else if !until.IsZero() {
		return &LockedError{RetryAfter: until.Sub(now)}
	}
	u, err := a.st.Users().Get(ctx, p.UserID)
	if err != nil {
		return fmt.Errorf("reauth: %w", err)
	}
	if u.PasswordHash == nil {
		a.hasher.VerifyDummy(ctx, password)
		return a.loginFailed(ctx, key, p.Email, &u.ID, now)
	}
	ok, _, err := a.hasher.Verify(ctx, *u.PasswordHash, password)
	if err != nil {
		return fmt.Errorf("reauth: %w", err)
	}
	if !ok {
		return a.loginFailed(ctx, key, p.Email, &u.ID, now)
	}
	return a.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.Sessions().MarkReauthenticated(ctx, p.SessionID, now); err != nil {
			return err
		}
		return audit.Record(ctx, tx, userActor(p.UserID), audit.Event{Action: "session.reauthenticated", EntityType: "session", EntityID: p.SessionID.String()})
	})
}

// ForgotPassword emails a reset link if the account exists. It always returns
// nil for a well-formed address (no enumeration), and silently stops sending
// after ForgotPerHour requests per address.
func (a *Accounts) ForgotPassword(ctx context.Context, rawEmail string) error {
	email, err := normalizeEmail(rawEmail)
	if err != nil {
		return err
	}
	now := a.now()
	st, err := a.st.Throttle().RecordFailure(ctx, throttleKey("forgot", email), now,
		store.ThrottlePolicy{FreeFailures: a.policy.ForgotPerHour + 1, Window: time.Hour, MaxBackoff: time.Hour})
	if err != nil {
		return fmt.Errorf("forgot password: %w", err)
	}
	if st.Failures > a.policy.ForgotPerHour {
		return nil
	}
	u, err := a.st.Users().GetByEmail(ctx, email)
	if errors.Is(err, store.ErrNotFound) || (err == nil && u.Status != store.UserActive) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("forgot password: %w", err)
	}
	token, tokenHash := newEmailToken()
	err = a.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.EmailTokens().InvalidateForUser(ctx, u.ID, store.TokenResetPassword, now); err != nil {
			return err
		}
		if err := tx.EmailTokens().Create(ctx, u.ID, store.TokenResetPassword, tokenHash, now, now.Add(a.policy.ResetTTL)); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.System, audit.Event{Action: "user.password_reset_requested", EntityType: "user", EntityID: u.ID.String()})
	})
	if err != nil {
		return fmt.Errorf("forgot password: %w", err)
	}
	a.send(ctx, nmail.Message{
		To:      email,
		Subject: "Reset your Notifex password",
		Text: "Use this link to choose a new password:\n\n" +
			a.consoleURL + "/reset-password#token=" + token + "\n\n" +
			"The link expires in 1 hour and can be used once. If you didn't ask for this, ignore this email; your password is unchanged.\n",
	})
	return nil
}

// ResetPassword sets a new password with a reset token and revokes every
// session of the user. A rejected password does not consume the token.
func (a *Accounts) ResetPassword(ctx context.Context, token, newPassword string) error {
	now := a.now()
	return a.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		uid, err := tx.EmailTokens().Consume(ctx, store.TokenResetPassword, hashToken(token), now)
		if errors.Is(err, store.ErrNotFound) {
			return ErrInvalidToken
		}
		if err != nil {
			return fmt.Errorf("reset password: %w", err)
		}
		u, err := tx.Users().Get(ctx, uid)
		if err != nil {
			return fmt.Errorf("reset password: %w", err)
		}
		if err := ValidatePassword(newPassword, u.Email); err != nil {
			return err // rolls back: the token stays usable
		}
		h, err := a.hasher.Hash(ctx, newPassword)
		if err != nil {
			return err
		}
		if err := tx.Users().SetPasswordHash(ctx, uid, h); err != nil {
			return fmt.Errorf("reset password: %w", err)
		}
		if err := tx.Users().MarkEmailVerified(ctx, uid, now); err != nil { // the link proved ownership
			return fmt.Errorf("reset password: %w", err)
		}
		n, err := tx.Sessions().RevokeAllForUser(ctx, uid, uuid.Nil, now)
		if err != nil {
			return fmt.Errorf("reset password: %w", err)
		}
		if err := tx.Throttle().Reset(ctx, throttleKey("login", u.Email)); err != nil {
			return fmt.Errorf("reset password: %w", err)
		}
		return audit.Record(ctx, tx, userActor(uid), audit.Event{
			Action: "user.password_reset", EntityType: "user", EntityID: uid.String(),
			After: map[string]any{"sessions_revoked": n},
		})
	})
}

func (a *Accounts) sendAlreadyRegistered(ctx context.Context, email string) {
	a.send(ctx, nmail.Message{
		To:      email,
		Subject: "You already have a Notifex account",
		Text: "Someone tried to create a Notifex account with this email address, which already has one.\n\n" +
			"If it was you, sign in or reset your password:\n\n" + a.consoleURL + "/forgot-password\n\n" +
			"If it wasn't, you can ignore this email.\n",
	})
}

// send hands mail to the (asynchronous) mailer; failures are logged, never
// surfaced, so they can't reveal account state.
func (a *Accounts) send(ctx context.Context, m nmail.Message) {
	if err := a.mailer.Send(ctx, m); err != nil {
		a.log.ErrorContext(ctx, "system mail not sent", slog.String("to", observability.MaskEmail(m.To)), slog.Any("error", err))
	}
}

func userActor(id uuid.UUID) audit.Actor { return audit.Actor{Type: store.ActorUser, ID: id.String()} }

func normalizeEmail(s string) (string, error) {
	e := strings.ToLower(strings.TrimSpace(s))
	addr, err := mail.ParseAddress(e)
	if err != nil || addr.Address != e || len(e) > 320 {
		return "", ErrInvalidEmail
	}
	return e, nil
}

// throttleKey is an opaque, PII-free key for the throttle table.
func throttleKey(kind, email string) string {
	sum := sha256.Sum256([]byte(email))
	return kind + ":" + hex.EncodeToString(sum[:])
}

const sessionTokenPrefix = "nfs_"

func newSessionToken() (string, []byte) {
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	t := sessionTokenPrefix + base62(raw, secretLen)
	return t, hashToken(t)
}

func newEmailToken() (string, []byte) {
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	t := base62(raw, secretLen)
	return t, hashToken(t)
}

// hashToken is SHA-256: tokens carry 256 random bits, so no salt or pepper
// is needed (ADR-011 §1).
func hashToken(t string) []byte {
	sum := sha256.Sum256([]byte(t))
	return sum[:]
}
