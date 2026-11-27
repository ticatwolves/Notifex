package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"github.com/ticatwolves/notifex/internal/audit"
	"github.com/ticatwolves/notifex/internal/observability"
	"github.com/ticatwolves/notifex/internal/store"
)

// OAuth errors (ADR-011 §3). Transports map them to docs/03 codes.
var (
	ErrProviderNotEnabled      = errors.New("auth: oauth provider not enabled")
	ErrRedirectNotAllowed      = errors.New("auth: redirect_uri is not allowlisted")
	ErrLinkRequired            = errors.New("auth: sign in to link this identity")
	ErrProviderEmailUnverified = errors.New("auth: the provider has not verified this email")
	ErrIdentityInUse           = errors.New("auth: identity is linked to another account")
	ErrLastLoginMethod         = errors.New("auth: cannot remove the last way to sign in")
)

// OAuthStateTTL bounds how long an authorization may take.
const OAuthStateTTL = 10 * time.Minute

// OAuth implements provider sign-in and identity linking.
type OAuth struct {
	accounts  *Accounts
	st        store.Store
	providers map[store.OAuthProvider]Provider
	redirects map[string]bool
	now       func() time.Time
}

// NewOAuth enables the given providers. redirectURIs is the exact-match
// allowlist for redirect_uri (the console's callback URLs).
func NewOAuth(a *Accounts, providers []Provider, redirectURIs []string) *OAuth {
	o := &OAuth{accounts: a, st: a.st, providers: map[store.OAuthProvider]Provider{}, redirects: map[string]bool{}, now: time.Now}
	for _, p := range providers {
		o.providers[p.Name()] = p
	}
	for _, r := range redirectURIs {
		o.redirects[strings.TrimSpace(r)] = true
	}
	return o
}

// SetClock overrides the clock (tests).
func (o *OAuth) SetClock(now func() time.Time) { o.now = now }

// Enabled lists configured providers.
func (o *OAuth) Enabled() []store.OAuthProvider {
	out := make([]store.OAuthProvider, 0, len(o.providers))
	for name := range o.providers {
		out = append(out, name)
	}
	return out
}

// Authorize starts a login, or — when linkFor is set — a link of a new
// provider to that signed-in user. The returned state is opaque to callers.
func (o *OAuth) Authorize(ctx context.Context, provider, redirectURI string, linkFor *SessionPrincipal) (authURL, state string, err error) {
	p, ok := o.providers[store.OAuthProvider(provider)]
	if !ok {
		return "", "", ErrProviderNotEnabled
	}
	if !o.redirects[redirectURI] {
		return "", "", ErrRedirectNotAllowed
	}
	state = randomToken()
	nonce := randomToken()
	verifier := oauth2.GenerateVerifier()
	now := o.now()
	var link *uuid.UUID
	if linkFor != nil {
		id := linkFor.UserID
		link = &id
	}
	err = o.st.OAuthStates().Create(ctx, store.NewOAuthState{
		StateHash: hashToken(state), Provider: p.Name(), RedirectURI: redirectURI,
		CodeVerifier: verifier, Nonce: nonce, LinkUserID: link, Now: now, ExpiresAt: now.Add(OAuthStateTTL),
	})
	if err != nil {
		return "", "", fmt.Errorf("oauth authorize: %w", err)
	}
	return p.AuthCodeURL(state, nonce, verifier, redirectURI), state, nil
}

// CallbackResult is a completed login (Session set) or link (Linked).
type CallbackResult struct {
	Session *NewSessionToken
	Linked  bool
}

// Callback completes an authorization started by Authorize. session is the
// caller's console session, if any; it is required (and must match) for a
// link started while signed in.
func (o *OAuth) Callback(ctx context.Context, provider, code, state string, session *SessionPrincipal, userAgent string) (CallbackResult, error) {
	p, ok := o.providers[store.OAuthProvider(provider)]
	if !ok {
		return CallbackResult{}, ErrProviderNotEnabled
	}
	now := o.now()
	st, err := o.st.OAuthStates().Consume(ctx, hashToken(state), p.Name(), now)
	if errors.Is(err, store.ErrNotFound) {
		return CallbackResult{}, ErrInvalidToken
	}
	if err != nil {
		return CallbackResult{}, fmt.Errorf("oauth callback: %w", err)
	}
	// A link flow only completes for the user who started it. Without this,
	// an attacker could get a victim to attach the attacker's provider
	// account to the victim's Notifex account (linking CSRF).
	if st.LinkUserID != nil && (session == nil || session.UserID != *st.LinkUserID) {
		return CallbackResult{}, ErrInvalidToken
	}

	ext, err := p.Exchange(ctx, code, st.CodeVerifier, st.Nonce, st.RedirectURI)
	if errors.Is(err, ErrProviderRejected) {
		o.accounts.log.InfoContext(ctx, "oauth login rejected", slog.String("provider", provider), slog.Any("error", err))
		return CallbackResult{}, ErrInvalidToken
	}
	if err != nil {
		return CallbackResult{}, fmt.Errorf("oauth callback: %w", err)
	}

	if st.LinkUserID != nil {
		return CallbackResult{Linked: true}, o.link(ctx, *st.LinkUserID, p.Name(), ext, now)
	}
	s, err := o.login(ctx, p.Name(), ext, userAgent, now)
	if err != nil {
		return CallbackResult{}, err
	}
	return CallbackResult{Session: &s}, nil
}

// login applies the ADR-011 §3 rules (with the unverified-email amendment).
func (o *OAuth) login(ctx context.Context, provider store.OAuthProvider, ext ExternalIdentity, ua string, now time.Time) (NewSessionToken, error) {
	method := "oauth:" + string(provider)
	var out NewSessionToken
	err := o.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		// 1. Known identity: sign in.
		id, err := tx.Identities().GetBySubject(ctx, provider, ext.Subject)
		if err == nil {
			u, err := tx.Users().Get(ctx, id.UserID)
			if err != nil {
				return err
			}
			if u.Status != store.UserActive {
				return ErrInvalidCredentials
			}
			if err := tx.Identities().Touch(ctx, id.ID, now); err != nil {
				return err
			}
			out, err = o.accounts.createSession(ctx, tx, u.ID, ua, now, method)
			return err
		}
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		// 2. Unknown identity: the email decides, but only a verified one is
		// ever looked up, so this flow can't be used to probe for accounts.
		email, verr := normalizeEmail(ext.Email)
		if !ext.EmailVerified || verr != nil {
			return ErrProviderEmailUnverified
		}
		u, err := tx.Users().GetByEmail(ctx, email)
		switch {
		case errors.Is(err, store.ErrNotFound):
			// 3. New person: create a verified, password-less account.
			u, err = tx.Users().Create(ctx, store.NewUser{Email: email, Name: ext.Name, EmailVerifiedAt: &now})
			if err != nil {
				return err
			}
			if err := audit.Record(ctx, tx, userActor(u.ID), audit.Event{
				Action: "user.signup", EntityType: "user", EntityID: u.ID.String(),
				After: map[string]any{"method": method},
			}); err != nil {
				return err
			}
		case err != nil:
			return err
		case u.EmailVerifiedAt == nil:
			// 4. Existing account whose email is unproven: never auto-link.
			return ErrLinkRequired
		}
		if u.Status != store.UserActive {
			return ErrInvalidCredentials
		}
		if err := o.attach(ctx, tx, u.ID, provider, ext, now, "auto"); err != nil {
			return err
		}
		out, err = o.accounts.createSession(ctx, tx, u.ID, ua, now, method)
		return err
	})
	if errors.Is(err, store.ErrConflict) {
		// The verified user already has a different identity for this
		// provider: they must unlink it first.
		return NewSessionToken{}, ErrLinkRequired
	}
	if err != nil {
		return NewSessionToken{}, wrapUnlessSentinel("oauth login", err)
	}
	return out, nil
}

// link attaches a provider identity to a signed-in user.
func (o *OAuth) link(ctx context.Context, userID uuid.UUID, provider store.OAuthProvider, ext ExternalIdentity, now time.Time) error {
	err := o.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		existing, err := tx.Identities().GetBySubject(ctx, provider, ext.Subject)
		switch {
		case err == nil && existing.UserID == userID:
			return nil // already linked: idempotent
		case err == nil:
			return ErrIdentityInUse
		case !errors.Is(err, store.ErrNotFound):
			return err
		}
		return o.attach(ctx, tx, userID, provider, ext, now, "settings")
	})
	if errors.Is(err, store.ErrConflict) {
		return fmt.Errorf("%w: unlink your current %s identity first", store.ErrConflict, provider)
	}
	return wrapUnlessSentinel("oauth link", err)
}

func (o *OAuth) attach(ctx context.Context, tx store.Store, userID uuid.UUID, provider store.OAuthProvider, ext ExternalIdentity, now time.Time, via string) error {
	id, err := tx.Identities().Create(ctx, store.NewIdentity{UserID: userID, Provider: provider, Subject: ext.Subject, Email: ext.Email, Now: now})
	if err != nil {
		return err
	}
	return audit.Record(ctx, tx, userActor(userID), audit.Event{
		Action: "identity.link", EntityType: "identity", EntityID: id.ID.String(),
		After: map[string]any{"provider": string(provider), "via": via, "email": observability.MaskEmail(ext.Email)},
	})
}

// ListIdentities returns the caller's linked identities.
func (o *OAuth) ListIdentities(ctx context.Context, p SessionPrincipal) ([]store.Identity, error) {
	ids, err := o.st.Identities().ListForUser(ctx, p.UserID)
	if err != nil {
		return nil, fmt.Errorf("list identities: %w", err)
	}
	return ids, nil
}

// Unlink removes a provider from the caller's account, unless it is their
// last way to sign in.
func (o *OAuth) Unlink(ctx context.Context, p SessionPrincipal, provider string) error {
	return wrapUnlessSentinel("unlink identity", o.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		u, err := tx.Users().Get(ctx, p.UserID)
		if err != nil {
			return err
		}
		ids, err := tx.Identities().ListForUser(ctx, p.UserID)
		if err != nil {
			return err
		}
		var target *store.Identity
		for i := range ids {
			if string(ids[i].Provider) == provider {
				target = &ids[i]
			}
		}
		if target == nil {
			return store.ErrNotFound
		}
		if u.PasswordHash == nil && len(ids) == 1 {
			return ErrLastLoginMethod
		}
		if err := tx.Identities().Delete(ctx, p.UserID, target.Provider); err != nil {
			return err
		}
		return audit.Record(ctx, tx, userActor(p.UserID), audit.Event{
			Action: "identity.unlink", EntityType: "identity", EntityID: target.ID.String(),
			After: map[string]any{"provider": provider},
		})
	}))
}

// wrapUnlessSentinel keeps package sentinels matchable while adding context.
func wrapUnlessSentinel(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", op, err)
}

func randomToken() string {
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	return base62(raw, secretLen)
}

// ValidateRedirectURIs checks allowlist entries: absolute https URLs (http
// only for localhost), no fragments.
func ValidateRedirectURIs(uris []string) error {
	for _, raw := range uris {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || u.Host == "" || u.Fragment != "" {
			return fmt.Errorf("oauth redirect %q: must be an absolute URL without a fragment", raw)
		}
		local := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1"
		if u.Scheme != "https" && (u.Scheme != "http" || !local) {
			return fmt.Errorf("oauth redirect %q: must use https (http only for localhost)", raw)
		}
	}
	return nil
}
