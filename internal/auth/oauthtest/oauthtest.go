// Package oauthtest is a fake OAuth/OIDC identity provider (Google- and
// Microsoft-style OIDC, plus GitHub's OAuth + REST API) for tests. It
// enforces PKCE, redirect URI and client credentials at the token endpoint
// and signs real RS256 ID tokens, so verification code runs for real.
// Never use outside tests.
package oauthtest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// Client credentials the fake accepts.
const (
	ClientID     = "test-client"
	ClientSecret = "test-secret"
)

// Tamper breaks one property of the next issued token.
type Tamper int

// Tamper modes.
const (
	None Tamper = iota
	WrongAudience
	WrongNonce
	Expired
	ForeignKey
	WrongTenantIssuer
	NoIDToken
)

// User is the person "signing in" at the fake provider.
type User struct {
	// OIDC claims (Google/Microsoft): sub, email, email_verified, tid, oid, xms_edov, name.
	Claims map[string]any
	// GitHub: numeric id and emails.
	GitHubID     int64
	GitHubEmails []GitHubEmail
	Tamper       Tamper
}

// GitHubEmail is an entry of GitHub's /user/emails.
type GitHubEmail struct {
	Email    string `json:"email"`
	Primary  bool   `json:"primary"`
	Verified bool   `json:"verified"`
}

type grant struct {
	user      User
	challenge string
	nonce     string
	redirect  string
	used      bool
}

// Server is a running fake provider.
type Server struct {
	*httptest.Server
	key, foreign *rsa.PrivateKey
	mu           sync.Mutex
	grants       map[string]*grant
	tokens       map[string]User
	// Microsoft-style issuer per tenant when true.
	multiTenant bool
}

// New starts a fake provider. multiTenant makes ID tokens carry a
// Microsoft-style per-tenant issuer (base + "/{tid}/v2.0").
func New(multiTenant bool) *Server {
	s := &Server{grants: map[string]*grant{}, tokens: map[string]User{}, multiTenant: multiTenant}
	s.key, _ = rsa.GenerateKey(rand.Reader, 2048)
	s.foreign, _ = rsa.GenerateKey(rand.Reader, 2048)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /jwks", s.jwks)
	mux.HandleFunc("POST /token", s.token)
	mux.HandleFunc("GET /user", s.ghUser)
	mux.HandleFunc("GET /user/emails", s.ghEmails)
	s.Server = httptest.NewServer(mux)
	return s
}

// Issuer is the expected issuer (with {tenantid} when multi-tenant).
func (s *Server) Issuer() string {
	if s.multiTenant {
		return s.URL + "/{tenantid}/v2.0"
	}
	return s.URL
}

// Approve simulates the user consenting at authURL (as built by the code
// under test) and returns the authorization code the provider would send
// to the redirect URI.
func (s *Server) Approve(authURL string, u User) (string, error) {
	q, err := url.Parse(authURL)
	if err != nil {
		return "", fmt.Errorf("oauthtest: parse authorization URL: %w", err)
	}
	v := q.Query()
	if v.Get("client_id") != ClientID || v.Get("code_challenge_method") != "S256" || v.Get("code_challenge") == "" || v.Get("state") == "" {
		return "", fmt.Errorf("oauthtest: authorization URL missing client_id/PKCE/state: %s", authURL)
	}
	code := randomString()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grants[code] = &grant{user: u, challenge: v.Get("code_challenge"), nonce: v.Get("nonce"), redirect: v.Get("redirect_uri")}
	return code, nil
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	g, ok := s.grants[r.PostForm.Get("code")]
	if ok && g.used {
		ok = false // codes are single use
	}
	if ok {
		g.used = true
	}
	s.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	switch {
	case !ok,
		r.PostForm.Get("client_id") != ClientID || r.PostForm.Get("client_secret") != ClientSecret,
		r.PostForm.Get("redirect_uri") != g.redirect,
		base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
		return
	}
	access := randomString()
	s.mu.Lock()
	s.tokens[access] = g.user
	s.mu.Unlock()
	resp := map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": 3600}
	if g.user.Claims != nil && g.user.Tamper != NoIDToken {
		resp["id_token"] = s.idToken(g)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) idToken(g *grant) string {
	now := time.Now()
	c := map[string]any{"aud": ClientID, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(), "nonce": g.nonce, "iss": s.URL}
	for k, v := range g.user.Claims {
		c[k] = v
	}
	if s.multiTenant {
		c["iss"] = s.URL + "/" + fmt.Sprint(c["tid"]) + "/v2.0"
	}
	key := s.key
	switch g.user.Tamper {
	case WrongAudience:
		c["aud"] = "someone-else"
	case WrongNonce:
		c["nonce"] = "not-the-nonce"
	case Expired:
		c["iat"], c["exp"] = now.Add(-2*time.Hour).Unix(), now.Add(-time.Hour).Unix()
	case ForeignKey:
		key = s.foreign
	case WrongTenantIssuer:
		c["iss"] = s.URL + "/attacker-tenant/v2.0"
	}
	sig, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
	tok, _ := jwt.Signed(sig).Claims(c).Serialize()
	return tok
}

func (s *Server) jwks(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &s.key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
}

func (s *Server) bearerUser(r *http.Request) (User, bool) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.tokens[tok]
	return u, ok
}

func (s *Server) ghUser(w http.ResponseWriter, r *http.Request) {
	u, ok := s.bearerUser(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"id": u.GitHubID, "name": "GitHub User"})
}

func (s *Server) ghEmails(w http.ResponseWriter, r *http.Request) {
	u, ok := s.bearerUser(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	_ = json.NewEncoder(w).Encode(u.GitHubEmails)
}

func randomString() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
