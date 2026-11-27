package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ticatwolves/notifex/internal/store"
)

// SessionPrincipal is an authenticated console user.
type SessionPrincipal struct {
	UserID            uuid.UUID
	SessionID         uuid.UUID
	Email             string
	EmailVerified     bool
	ReauthenticatedAt time.Time
}

// RecentlyAuthenticated reports whether sensitive actions are allowed.
func (p SessionPrincipal) RecentlyAuthenticated(now time.Time, window time.Duration) bool {
	return now.Sub(p.ReauthenticatedAt) < window
}

// SessionAuthenticator resolves console session tokens.
type SessionAuthenticator struct {
	st     store.Store
	log    *slog.Logger
	policy SessionPolicy
	now    func() time.Time
}

// NewSessionAuthenticator builds a session authenticator.
func NewSessionAuthenticator(st store.Store, log *slog.Logger, p SessionPolicy) *SessionAuthenticator {
	return &SessionAuthenticator{st: st, log: log, policy: p, now: time.Now}
}

// SetClock overrides the clock (tests).
func (s *SessionAuthenticator) SetClock(now func() time.Time) { s.now = now }

// Authenticate returns ErrUnauthenticated for any invalid, revoked, idle or
// expired session (or disabled user) and ErrUnavailable if it can't decide.
func (s *SessionAuthenticator) Authenticate(ctx context.Context, token string) (SessionPrincipal, error) {
	if !strings.HasPrefix(token, sessionTokenPrefix) || len(token) != len(sessionTokenPrefix)+secretLen || !only(token[len(sessionTokenPrefix):], base62Alphabet) {
		return SessionPrincipal{}, ErrUnauthenticated
	}
	sess, err := s.st.Sessions().GetByTokenHash(ctx, hashToken(token))
	if errors.Is(err, store.ErrNotFound) {
		return SessionPrincipal{}, ErrUnauthenticated
	}
	if err != nil {
		return SessionPrincipal{}, s.unavailable(ctx, err)
	}
	now := s.now()
	if sess.RevokedAt != nil || !now.Before(sess.AbsoluteExpiresAt) || now.Sub(sess.LastSeenAt) >= s.policy.Idle {
		return SessionPrincipal{}, ErrUnauthenticated
	}
	u, err := s.st.Users().Get(ctx, sess.UserID)
	if errors.Is(err, store.ErrNotFound) {
		return SessionPrincipal{}, ErrUnauthenticated
	}
	if err != nil {
		return SessionPrincipal{}, s.unavailable(ctx, err)
	}
	if u.Status != store.UserActive {
		return SessionPrincipal{}, ErrUnauthenticated
	}
	if err := s.st.Sessions().Touch(ctx, sess.ID, now, s.policy.TouchInterval); err != nil {
		s.log.WarnContext(ctx, "session touch failed", slog.Any("error", err))
	}
	return SessionPrincipal{
		UserID: u.ID, SessionID: sess.ID, Email: u.Email,
		EmailVerified: u.EmailVerifiedAt != nil, ReauthenticatedAt: sess.ReauthenticatedAt,
	}, nil
}

func (s *SessionAuthenticator) unavailable(ctx context.Context, err error) error {
	s.log.ErrorContext(ctx, "session authentication failed", slog.Any("error", err))
	return fmt.Errorf("%w: %w", ErrUnavailable, err)
}

// Me is the caller's profile and tenant memberships.
type Me struct {
	User        store.User
	Memberships []MyMembership
}

// MyMembership is one tenant the user belongs to.
type MyMembership struct {
	Tenant store.Tenant
	Role   store.Role
}

// GetMe returns the caller's profile and active-tenant memberships.
func (a *Accounts) GetMe(ctx context.Context, p SessionPrincipal) (Me, error) {
	u, err := a.st.Users().Get(ctx, p.UserID)
	if err != nil {
		return Me{}, fmt.Errorf("me: %w", err)
	}
	ms, err := a.st.Memberships().ListForUser(ctx, p.UserID)
	if err != nil {
		return Me{}, fmt.Errorf("me: %w", err)
	}
	out := Me{User: u, Memberships: make([]MyMembership, 0, len(ms))}
	for _, m := range ms {
		t, err := a.st.Tenants().Get(ctx, m.TenantID)
		if err != nil {
			return Me{}, fmt.Errorf("me: %w", err)
		}
		if t.Status == store.TenantDeleted {
			continue
		}
		out.Memberships = append(out.Memberships, MyMembership{Tenant: t, Role: m.Role})
	}
	return out, nil
}
