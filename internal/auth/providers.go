package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/ticatwolves/notifex/internal/store"
)

// ExternalIdentity is what a provider asserts about the signed-in person.
type ExternalIdentity struct {
	// Subject is the provider's stable account id (never the email).
	Subject string
	Email   string
	// EmailVerified is true only when the provider vouches for the email
	// (ADR-011 §3: Microsoft only with xms_edov, GitHub only a verified primary).
	EmailVerified bool
	Name          string
}

// Provider is one OAuth/OIDC identity provider.
type Provider interface {
	Name() store.OAuthProvider
	// AuthCodeURL builds the authorization URL (PKCE S256 + state + nonce).
	AuthCodeURL(state, nonce, verifier, redirectURI string) string
	// Exchange redeems code and returns the verified identity.
	Exchange(ctx context.Context, code, verifier, nonce, redirectURI string) (ExternalIdentity, error)
}

// ErrProviderRejected means the provider refused the code or returned a token
// that failed verification. Not retryable.
var ErrProviderRejected = errors.New("auth: identity provider rejected the login")

// OIDCEndpoints are a provider's endpoints. Configured statically (no
// discovery at startup), so API boot never depends on the provider; signing
// keys are fetched lazily and cached by go-oidc.
type OIDCEndpoints struct {
	AuthURL  string
	TokenURL string
	JWKSURL  string
	// Issuer is the expected `iss`. For Microsoft multi-tenant it is a
	// pattern with {tenantid}, checked against the token's tid.
	Issuer string
}

// Published endpoints.
var (
	GoogleEndpoints = OIDCEndpoints{ //nolint:gosec // G101: public endpoint URLs, not credentials
		AuthURL:  "https://accounts.google.com/o/oauth2/v2/auth",
		TokenURL: "https://oauth2.googleapis.com/token",
		JWKSURL:  "https://www.googleapis.com/oauth2/v3/certs",
		Issuer:   "https://accounts.google.com",
	}
	MicrosoftEndpoints = OIDCEndpoints{ //nolint:gosec // G101: public endpoint URLs, not credentials
		AuthURL:  "https://login.microsoftonline.com/common/oauth2/v2.0/authorize",
		TokenURL: "https://login.microsoftonline.com/common/oauth2/v2.0/token",
		JWKSURL:  "https://login.microsoftonline.com/common/discovery/v2.0/keys",
		Issuer:   "https://login.microsoftonline.com/{tenantid}/v2.0",
	}
)

// ClientCredentials are an OAuth client's id and secret.
type ClientCredentials struct {
	ID     string
	Secret string
}

// httpClient bounds every provider call.
var httpClient = &http.Client{Timeout: 10 * time.Second}

type oidcProvider struct {
	name     store.OAuthProvider
	conf     oauth2.Config
	verifier *oidc.IDTokenVerifier
	ep       OIDCEndpoints
	now      func() time.Time
	claims   func(tok *oidc.IDToken, ep OIDCEndpoints) (ExternalIdentity, error)
}

// NewGoogle returns the Google OIDC provider.
func NewGoogle(ctx context.Context, c ClientCredentials, ep OIDCEndpoints) Provider {
	return newOIDC(ctx, store.ProviderGoogle, c, ep, false, googleClaims)
}

// NewMicrosoft returns the Microsoft identity platform (multi-tenant) provider.
func NewMicrosoft(ctx context.Context, c ClientCredentials, ep OIDCEndpoints) Provider {
	return newOIDC(ctx, store.ProviderMicrosoft, c, ep, true, microsoftClaims)
}

func newOIDC(ctx context.Context, name store.OAuthProvider, c ClientCredentials, ep OIDCEndpoints, multiTenant bool,
	claims func(*oidc.IDToken, OIDCEndpoints) (ExternalIdentity, error),
) *oidcProvider {
	keyCtx := oidc.ClientContext(context.WithoutCancel(ctx), httpClient)
	p := &oidcProvider{
		name: name,
		conf: oauth2.Config{
			ClientID: c.ID, ClientSecret: c.Secret,
			Endpoint: oauth2.Endpoint{AuthURL: ep.AuthURL, TokenURL: ep.TokenURL, AuthStyle: oauth2.AuthStyleInParams},
			Scopes:   []string{oidc.ScopeOpenID, "email", "profile"},
		},
		ep: ep, now: time.Now, claims: claims,
	}
	p.verifier = oidc.NewVerifier(ep.Issuer, oidc.NewRemoteKeySet(keyCtx, ep.JWKSURL), &oidc.Config{
		ClientID:        c.ID,
		SkipIssuerCheck: multiTenant, // checked per token against tid instead
		Now:             func() time.Time { return p.now() },
	})
	return p
}

func (p *oidcProvider) Name() store.OAuthProvider { return p.name }

func (p *oidcProvider) AuthCodeURL(state, nonce, verifier, redirectURI string) string {
	c := p.conf
	c.RedirectURL = redirectURI
	return c.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oidc.Nonce(nonce))
}

func (p *oidcProvider) Exchange(ctx context.Context, code, verifier, nonce, redirectURI string) (ExternalIdentity, error) {
	c := p.conf
	c.RedirectURL = redirectURI
	hctx := context.WithValue(ctx, oauth2.HTTPClient, httpClient)
	tok, err := c.Exchange(hctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return ExternalIdentity{}, classifyExchange(err)
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok || raw == "" {
		return ExternalIdentity{}, fmt.Errorf("%w: no id_token", ErrProviderRejected)
	}
	idt, err := p.verifier.Verify(oidc.ClientContext(ctx, httpClient), raw)
	if err != nil {
		return ExternalIdentity{}, fmt.Errorf("%w: id_token: %w", ErrProviderRejected, err)
	}
	// go-oidc does not check the nonce; a mismatch means a replayed or
	// injected token.
	if idt.Nonce == "" || idt.Nonce != nonce {
		return ExternalIdentity{}, fmt.Errorf("%w: nonce mismatch", ErrProviderRejected)
	}
	return p.claims(idt, p.ep)
}

// classifyExchange separates "the provider said no" (bad or reused code:
// not retryable) from transport failures (retryable).
func classifyExchange(err error) error {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		return fmt.Errorf("%w: token endpoint returned %d", ErrProviderRejected, re.Response.StatusCode)
	}
	return fmt.Errorf("%w: token exchange: %w", ErrUnavailable, err)
}

func googleClaims(t *oidc.IDToken, _ OIDCEndpoints) (ExternalIdentity, error) {
	var c struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
	}
	if err := t.Claims(&c); err != nil {
		return ExternalIdentity{}, fmt.Errorf("%w: claims: %w", ErrProviderRejected, err)
	}
	if t.Subject == "" {
		return ExternalIdentity{}, fmt.Errorf("%w: empty subject", ErrProviderRejected)
	}
	return ExternalIdentity{Subject: t.Subject, Email: strings.ToLower(c.Email), EmailVerified: c.EmailVerified && c.Email != "", Name: c.Name}, nil
}

// microsoftClaims keys the identity on tid:oid (stable, unique across
// tenants) and trusts the email only with xms_edov ("email domain owner
// verified"): Entra's email claim is otherwise mutable (nOAuth). The app
// registration must request the xms_edov optional claim.
func microsoftClaims(t *oidc.IDToken, ep OIDCEndpoints) (ExternalIdentity, error) {
	var c struct {
		TID   string `json:"tid"`
		OID   string `json:"oid"`
		Email string `json:"email"`
		EDOV  any    `json:"xms_edov"`
		Name  string `json:"name"`
	}
	if err := t.Claims(&c); err != nil {
		return ExternalIdentity{}, fmt.Errorf("%w: claims: %w", ErrProviderRejected, err)
	}
	if c.TID == "" || c.OID == "" {
		return ExternalIdentity{}, fmt.Errorf("%w: missing tid/oid", ErrProviderRejected)
	}
	if want := strings.ReplaceAll(ep.Issuer, "{tenantid}", c.TID); t.Issuer != want {
		return ExternalIdentity{}, fmt.Errorf("%w: issuer %q does not match tenant", ErrProviderRejected, t.Issuer)
	}
	verified := false
	switch v := c.EDOV.(type) { // Entra sends true or "1"
	case bool:
		verified = v
	case string:
		verified = v == "1" || strings.EqualFold(v, "true")
	}
	return ExternalIdentity{
		Subject: c.TID + ":" + c.OID, Email: strings.ToLower(c.Email),
		EmailVerified: verified && c.Email != "", Name: c.Name,
	}, nil
}

// GitHubEndpoints are GitHub's OAuth and API endpoints.
type GitHubEndpoints struct {
	AuthURL  string
	TokenURL string
	APIURL   string
}

// GitHubDefaultEndpoints are github.com's.
var GitHubDefaultEndpoints = GitHubEndpoints{ //nolint:gosec // G101: public endpoint URLs, not credentials
	AuthURL:  "https://github.com/login/oauth/authorize",
	TokenURL: "https://github.com/login/oauth/access_token",
	APIURL:   "https://api.github.com",
}

type githubProvider struct {
	conf oauth2.Config
	api  string
}

// NewGitHub returns the GitHub OAuth provider. GitHub is OAuth 2.0 (no ID
// token): the identity comes from /user and the primary verified email from
// /user/emails.
func NewGitHub(c ClientCredentials, ep GitHubEndpoints) Provider {
	return &githubProvider{
		conf: oauth2.Config{
			ClientID: c.ID, ClientSecret: c.Secret,
			Endpoint: oauth2.Endpoint{AuthURL: ep.AuthURL, TokenURL: ep.TokenURL, AuthStyle: oauth2.AuthStyleInParams},
			Scopes:   []string{"read:user", "user:email"},
		},
		api: strings.TrimRight(ep.APIURL, "/"),
	}
}

func (g *githubProvider) Name() store.OAuthProvider { return store.ProviderGitHub }

func (g *githubProvider) AuthCodeURL(state, _, verifier, redirectURI string) string {
	c := g.conf
	c.RedirectURL = redirectURI
	return c.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier))
}

func (g *githubProvider) Exchange(ctx context.Context, code, verifier, _, redirectURI string) (ExternalIdentity, error) {
	c := g.conf
	c.RedirectURL = redirectURI
	hctx := context.WithValue(ctx, oauth2.HTTPClient, httpClient)
	tok, err := c.Exchange(hctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return ExternalIdentity{}, classifyExchange(err)
	}
	client := c.Client(hctx, tok)
	var user struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := g.get(ctx, client, "/user", &user); err != nil {
		return ExternalIdentity{}, err
	}
	if user.ID <= 0 {
		return ExternalIdentity{}, fmt.Errorf("%w: no user id", ErrProviderRejected)
	}
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := g.get(ctx, client, "/user/emails", &emails); err != nil {
		return ExternalIdentity{}, err
	}
	id := ExternalIdentity{Subject: strconv.FormatInt(user.ID, 10), Name: user.Name}
	for _, e := range emails {
		if e.Primary {
			id.Email = strings.ToLower(e.Email)
			id.EmailVerified = e.Verified && e.Email != ""
		}
	}
	return id, nil
}

func (g *githubProvider) get(ctx context.Context, client *http.Client, path string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.api+path, nil)
	if err != nil {
		return fmt.Errorf("github %s: %w", path, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: github %s: %w", ErrUnavailable, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("%w: github %s: status %d", ErrUnavailable, path, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: github %s: status %d", ErrProviderRejected, path, resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(dst); err != nil {
		return fmt.Errorf("%w: github %s: %w", ErrProviderRejected, path, err)
	}
	return nil
}
