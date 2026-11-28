package httptransport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/ticatwolves/notifex/internal/auth"
	"github.com/ticatwolves/notifex/internal/ids"
	"github.com/ticatwolves/notifex/internal/observability"
	"github.com/ticatwolves/notifex/internal/ratelimit"
	"github.com/ticatwolves/notifex/internal/store"
	"github.com/ticatwolves/notifex/internal/tenant"
	"github.com/ticatwolves/notifex/internal/workflow"
	"github.com/ticatwolves/notifex/internal/workflow/render"
)

// ConsoleAccounts is what the console routes need from internal/auth.
type ConsoleAccounts interface {
	Signup(ctx context.Context, in auth.SignupInput) error
	VerifyEmail(ctx context.Context, token string) error
	Login(ctx context.Context, in auth.LoginInput) (auth.NewSessionToken, error)
	Logout(ctx context.Context, p auth.SessionPrincipal) error
	Reauthenticate(ctx context.Context, p auth.SessionPrincipal, password string) error
	ForgotPassword(ctx context.Context, email string) error
	ResetPassword(ctx context.Context, token, password string) error
	GetMe(ctx context.Context, p auth.SessionPrincipal) (auth.Me, error)
	ListSessions(ctx context.Context, p auth.SessionPrincipal) ([]store.Session, error)
	RevokeSession(ctx context.Context, p auth.SessionPrincipal, id uuid.UUID) error
}

// ConsoleOAuth is what the OAuth routes need from internal/auth.
type ConsoleOAuth interface {
	Authorize(ctx context.Context, provider, redirectURI string, linkFor *auth.SessionPrincipal) (authURL, state string, err error)
	Callback(ctx context.Context, provider, code, state string, session *auth.SessionPrincipal, userAgent string) (auth.CallbackResult, error)
	ListIdentities(ctx context.Context, p auth.SessionPrincipal) ([]store.Identity, error)
	Unlink(ctx context.Context, p auth.SessionPrincipal, provider string) error
}

// SessionAuthenticator resolves console session tokens.
type SessionAuthenticator interface {
	Authenticate(ctx context.Context, token string) (auth.SessionPrincipal, error)
}

// Limits are per-IP request limits (per process; the WAF adds a shared layer).
type Limits struct {
	Login      *ratelimit.Limiter // login, reauth
	Credential *ratelimit.Limiter // signup, forgot, reset, verify
	APIKeyFail *ratelimit.Limiter // failed API-key authentications
	Preview    *ratelimit.Limiter // template previews (CPU-bound renders)
	Trigger    *ratelimit.Limiter // triggers per API key (ADR-014 §8)
	// OperatorLogin is separate from console login (ADR-011 §5).
	OperatorLogin *ratelimit.Limiter
}

// DefaultLimits follow ADR-011 §2.
func DefaultLimits() Limits {
	return Limits{
		Login:         ratelimit.New(20, 20, 100_000),
		Credential:    ratelimit.New(10, 10, 100_000),
		APIKeyFail:    ratelimit.New(20, 20, 100_000),
		OperatorLogin: ratelimit.New(5, 5, 10_000),
		Preview:       ratelimit.New(120, 30, 100_000),
		Trigger:       ratelimit.New(6000, 1000, 100_000),
	}
}

const ctxSession = "session_principal"

func registerConsole(r *gin.Engine, d Deps) {
	lim := d.Limits
	a := d.Accounts

	authg := r.Group("/v1/auth")
	authg.POST("/signup", perIP(lim.Credential), func(c *gin.Context) {
		var in struct {
			Email    string `json:"email"`
			Password string `json:"password"`
			Name     string `json:"name"`
		}
		if !bindJSON(c, &in) {
			return
		}
		if err := a.Signup(c.Request.Context(), auth.SignupInput{Email: in.Email, Password: in.Password, Name: in.Name}); err != nil {
			consoleError(c, err)
			return
		}
		c.Status(http.StatusAccepted)
	})
	authg.POST("/verify-email", perIP(lim.Credential), func(c *gin.Context) {
		var in struct {
			Token string `json:"token"`
		}
		if !bindJSON(c, &in) {
			return
		}
		if err := a.VerifyEmail(c.Request.Context(), in.Token); err != nil {
			consoleError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	})
	authg.POST("/login", perIP(lim.Login), func(c *gin.Context) {
		var in struct {
			Email    string `json:"email"`
			Password string `json:"password"`
		}
		if !bindJSON(c, &in) {
			return
		}
		s, err := a.Login(c.Request.Context(), auth.LoginInput{Email: in.Email, Password: in.Password, UserAgent: c.Request.UserAgent()})
		if err != nil {
			consoleError(c, err)
			return
		}
		c.JSON(http.StatusOK, sessionCreatedJSON(s))
	})
	authg.POST("/password/forgot", perIP(lim.Credential), func(c *gin.Context) {
		var in struct {
			Email string `json:"email"`
		}
		if !bindJSON(c, &in) {
			return
		}
		if err := a.ForgotPassword(c.Request.Context(), in.Email); err != nil {
			consoleError(c, err)
			return
		}
		c.Status(http.StatusAccepted)
	})
	authg.POST("/password/reset", perIP(lim.Credential), func(c *gin.Context) {
		var in struct {
			Token    string `json:"token"`
			Password string `json:"password"`
		}
		if !bindJSON(c, &in) {
			return
		}
		if err := a.ResetPassword(c.Request.Context(), in.Token, in.Password); err != nil {
			consoleError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	})

	sess := requireSession(d.Sessions)
	authg.POST("/logout", sess, func(c *gin.Context) {
		if err := a.Logout(c.Request.Context(), sessionPrincipal(c)); err != nil {
			consoleError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	})
	authg.POST("/reauth", perIP(lim.Login), sess, func(c *gin.Context) {
		var in struct {
			Password string `json:"password"`
		}
		if !bindJSON(c, &in) {
			return
		}
		if err := a.Reauthenticate(c.Request.Context(), sessionPrincipal(c), in.Password); err != nil {
			consoleError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	})

	if o := d.OAuth; o != nil {
		authg.POST("/oauth/:provider/authorize", perIP(lim.Credential), optionalSession(d.Sessions), func(c *gin.Context) {
			var in struct {
				RedirectURI string `json:"redirect_uri"`
			}
			if !bindJSON(c, &in) {
				return
			}
			u, state, err := o.Authorize(c.Request.Context(), c.Param("provider"), in.RedirectURI, optionalPrincipal(c))
			if err != nil {
				consoleError(c, err)
				return
			}
			c.JSON(http.StatusOK, gin.H{"authorization_url": u, "state": state})
		})
		authg.POST("/oauth/:provider/callback", perIP(lim.Credential), optionalSession(d.Sessions), func(c *gin.Context) {
			var in struct {
				Code  string `json:"code"`
				State string `json:"state"`
			}
			if !bindJSON(c, &in) {
				return
			}
			res, err := o.Callback(c.Request.Context(), c.Param("provider"), in.Code, in.State, optionalPrincipal(c), c.Request.UserAgent())
			if err != nil {
				consoleError(c, err)
				return
			}
			if res.Linked {
				c.Status(http.StatusNoContent)
				return
			}
			c.JSON(http.StatusOK, sessionCreatedJSON(*res.Session))
		})
	}

	me := r.Group("/v1/me", sess)
	if o := d.OAuth; o != nil {
		me.GET("/identities", func(c *gin.Context) {
			ids, err := o.ListIdentities(c.Request.Context(), sessionPrincipal(c))
			if err != nil {
				consoleError(c, err)
				return
			}
			out := make([]identityJSON, len(ids))
			for i, x := range ids {
				out[i] = identityJSON{Provider: string(x.Provider), Email: x.Email, CreatedAt: x.CreatedAt, LastUsedAt: x.LastUsedAt}
			}
			c.JSON(http.StatusOK, gin.H{"data": out})
		})
		me.DELETE("/identities/:provider", func(c *gin.Context) {
			if err := o.Unlink(c.Request.Context(), sessionPrincipal(c), c.Param("provider")); err != nil {
				consoleError(c, err)
				return
			}
			c.Status(http.StatusNoContent)
		})
	}
	me.GET("", func(c *gin.Context) {
		m, err := a.GetMe(c.Request.Context(), sessionPrincipal(c))
		if err != nil {
			consoleError(c, err)
			return
		}
		c.JSON(http.StatusOK, meJSON(m))
	})
	me.GET("/sessions", func(c *gin.Context) {
		p := sessionPrincipal(c)
		ss, err := a.ListSessions(c.Request.Context(), p)
		if err != nil {
			consoleError(c, err)
			return
		}
		out := make([]sessionJSON, len(ss))
		for i, s := range ss {
			out[i] = sessionJSON{
				ID: ids.Format(ids.Session, s.ID), CreatedAt: s.CreatedAt, LastSeenAt: s.LastSeenAt,
				UserAgent: s.UserAgent, Current: s.ID == p.SessionID,
			}
		}
		c.JSON(http.StatusOK, gin.H{"data": out})
	})
	me.DELETE("/sessions/:session_id", func(c *gin.Context) {
		id, err := ids.Parse(ids.Session, c.Param("session_id"))
		if err != nil {
			writeError(c, http.StatusNotFound, "not_found", "session not found")
			return
		}
		if err := a.RevokeSession(c.Request.Context(), sessionPrincipal(c), id); err != nil {
			consoleError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	})
}

// requireSession authenticates `Authorization: Bearer nfs_…`.
func requireSession(sa SessionAuthenticator) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, ok := bearer(c.GetHeader("Authorization"))
		if !ok {
			unauthenticated(c)
			return
		}
		p, err := sa.Authenticate(c.Request.Context(), raw)
		switch {
		case errors.Is(err, auth.ErrUnavailable):
			c.Header("Retry-After", "1")
			writeError(c, http.StatusServiceUnavailable, "unavailable", "authentication temporarily unavailable")
			return
		case err != nil:
			unauthenticated(c)
			return
		}
		c.Set(ctxSession, p)
		ctx := observability.WithLogAttrs(c.Request.Context(),
			slog.String("user_id", p.UserID.String()), slog.String("session_id", p.SessionID.String()))
		trace.SpanFromContext(ctx).SetAttributes(attribute.String("notifex.user_id", p.UserID.String()))
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// optionalSession authenticates a session if one is presented. A presented
// but invalid session is a 401, never "anonymous": a link attempt with an
// expired session must not silently turn into a login.
func optionalSession(sa SessionAuthenticator) gin.HandlerFunc {
	required := requireSession(sa)
	return func(c *gin.Context) {
		if c.GetHeader("Authorization") == "" {
			c.Next()
			return
		}
		required(c)
	}
}

func optionalPrincipal(c *gin.Context) *auth.SessionPrincipal {
	v, ok := c.Get(ctxSession)
	if !ok {
		return nil
	}
	p := v.(auth.SessionPrincipal)
	return &p
}

type identityJSON struct {
	Provider   string    `json:"provider"`
	Email      string    `json:"email"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
}

func sessionPrincipal(c *gin.Context) auth.SessionPrincipal {
	p, _ := c.Get(ctxSession)
	return p.(auth.SessionPrincipal) // set by requireSession before any handler runs
}

// perIP rate-limits by client IP (X-Forwarded-For only from trusted proxies).
func perIP(l *ratelimit.Limiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		if ok, retry := l.Allow(c.ClientIP()); !ok {
			rateLimited(c, "rate_limited", retry)
			return
		}
		c.Next()
	}
}

func rateLimited(c *gin.Context, code string, retry time.Duration) {
	secs := int(retry.Round(time.Second) / time.Second)
	if secs < 1 {
		secs = 1
	}
	c.Header("Retry-After", strconv.Itoa(secs))
	writeError(c, http.StatusTooManyRequests, code, "too many attempts; retry later")
}

// bindJSON decodes the body strictly: unknown fields are rejected, matching
// `additionalProperties: false` in the OpenAPI schemas.
func bindJSON(c *gin.Context, dst any) bool {
	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()
	dec.UseNumber() // payloads and schemas keep exact numbers
	if err := dec.Decode(dst); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(c, http.StatusRequestEntityTooLarge, "payload_too_large", "request body too large")
			return false
		}
		writeError(c, http.StatusBadRequest, "validation_failed", jsonErrorMessage(err))
		return false
	}
	if dec.More() {
		writeError(c, http.StatusBadRequest, "validation_failed", "request body must be a single JSON object")
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeError(c, http.StatusBadRequest, "validation_failed", "request body must be a single JSON object")
		return false
	}
	return true
}

// jsonErrorMessage describes a decode error without echoing input values.
func jsonErrorMessage(err error) string {
	var ute *json.UnmarshalTypeError
	switch {
	case errors.As(err, &ute):
		return fmt.Sprintf("field %q has the wrong type", ute.Field)
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		return "unknown field " + strings.TrimPrefix(err.Error(), "json: unknown field ")
	case errors.Is(err, io.EOF):
		return "request body is required"
	default:
		return "request body is not valid JSON"
	}
}

// consoleError maps service errors to the docs/03 error codes.
func consoleError(c *gin.Context, err error) {
	var locked *auth.LockedError
	switch {
	case errors.As(err, &locked):
		rateLimited(c, "account_locked", locked.RetryAfter)
	case errors.Is(err, auth.ErrInvalidCredentials):
		c.Header("WWW-Authenticate", `Bearer realm="notifex"`)
		writeError(c, http.StatusUnauthorized, "invalid_credentials", "email or password is incorrect")
	case errors.Is(err, auth.ErrProviderNotEnabled):
		writeError(c, http.StatusBadRequest, "validation_failed", "this sign-in provider is not enabled")
	case errors.Is(err, auth.ErrRedirectNotAllowed):
		writeError(c, http.StatusBadRequest, "validation_failed", "redirect_uri is not allowed")
	case errors.Is(err, auth.ErrProviderEmailUnverified):
		writeError(c, http.StatusForbidden, "email_not_verified", "the identity provider has not verified your email; sign in another way and link it from settings")
	case errors.Is(err, auth.ErrLinkRequired):
		writeError(c, http.StatusConflict, "link_required", "an account with this email exists; sign in to it and link this provider from settings")
	case errors.Is(err, auth.ErrIdentityInUse):
		writeError(c, http.StatusConflict, "conflict", "this identity is linked to another account")
	case errors.Is(err, auth.ErrLastLoginMethod):
		writeError(c, http.StatusConflict, "conflict", "set a password or link another provider before removing this one")
	case errors.Is(err, auth.ErrInvalidToken):
		writeError(c, http.StatusBadRequest, "invalid_token", "the link is invalid, expired or already used")
	case errors.Is(err, auth.ErrWeakPassword):
		writeError(c, http.StatusBadRequest, "validation_failed", strings.TrimPrefix(err.Error(), "auth: "))
	case errors.Is(err, auth.ErrInvalidEmail):
		writeError(c, http.StatusBadRequest, "validation_failed", "email address is not valid")
	case errors.Is(err, tenant.ErrReauthRequired):
		writeError(c, http.StatusForbidden, "reauth_required", "confirm your password to continue")
	case errors.Is(err, tenant.ErrEmailNotVerified):
		writeError(c, http.StatusForbidden, "email_not_verified", "verify your email address first")
	case errors.Is(err, tenant.ErrTenantLimit):
		writeError(c, http.StatusForbidden, "tenant_limit_reached", "you already own the maximum number of tenants")
	case errors.Is(err, tenant.ErrInviteEmail):
		writeError(c, http.StatusForbidden, "forbidden", "this invitation was sent to a different email address")
	case errors.Is(err, tenant.ErrForbidden):
		writeError(c, http.StatusForbidden, "forbidden", "your role does not allow this")
	case errors.Is(err, tenant.ErrLastOwner):
		writeError(c, http.StatusConflict, "conflict", "a tenant must keep at least one owner")
	case errors.Is(err, tenant.ErrKeyInactive):
		writeError(c, http.StatusConflict, "conflict", "this API key is already revoked or rotated")
	case errors.Is(err, tenant.ErrAlreadyMember):
		writeError(c, http.StatusConflict, "conflict", "already a member of this tenant")
	case errors.Is(err, tenant.ErrInvalidToken):
		writeError(c, http.StatusBadRequest, "invalid_token", "the invitation is invalid, expired or already used")
	case errors.Is(err, render.ErrInvalid):
		templateInvalid(c, err)
	case errors.As(err, new(*workflow.RuleError)):
		var re *workflow.RuleError
		errors.As(err, &re)
		writeError(c, http.StatusConflict, "conflict", re.Msg)
	case errors.As(err, new(*tenant.FieldError)):
		// Field-level messages are ours (never echo input values).
		writeError(c, http.StatusBadRequest, "validation_failed", strings.ReplaceAll(err.Error(), "\n", "; "))
	case errors.Is(err, store.ErrInvalid):
		writeError(c, http.StatusBadRequest, "validation_failed", "invalid input")
	case errors.Is(err, store.ErrNotFound):
		writeError(c, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, store.ErrConflict):
		writeError(c, http.StatusConflict, "conflict", "conflict: the resource already exists or is still in use")
	case errors.Is(err, auth.ErrUnavailable):
		c.Header("Retry-After", "1")
		writeError(c, http.StatusServiceUnavailable, "unavailable", "temporarily unavailable")
	default:
		slog.Default().ErrorContext(c.Request.Context(), "console request failed", slog.Any("error", err))
		writeError(c, http.StatusInternalServerError, "internal", "internal error")
	}
}

type sessionCreatedResponse struct {
	Token     string    `json:"token"`
	SessionID string    `json:"session_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

func sessionCreatedJSON(s auth.NewSessionToken) sessionCreatedResponse {
	return sessionCreatedResponse{Token: s.Token, SessionID: ids.Format(ids.Session, s.Session.ID), ExpiresAt: s.Session.AbsoluteExpiresAt.UTC()}
}

type sessionJSON struct {
	ID         string    `json:"id"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	UserAgent  string    `json:"user_agent"`
	Current    bool      `json:"current"`
}

type userJSON struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	Name          string `json:"name"`
	EmailVerified bool   `json:"email_verified"`
}

type myMembershipJSON struct {
	TenantID   string `json:"tenant_id"`
	TenantName string `json:"tenant_name"`
	TenantSlug string `json:"tenant_slug"`
	Role       string `json:"role"`
}

func toUserJSON(u store.User) userJSON {
	return userJSON{ID: ids.Format(ids.User, u.ID), Email: u.Email, Name: u.Name, EmailVerified: u.EmailVerifiedAt != nil}
}

func meJSON(m auth.Me) gin.H {
	ms := make([]myMembershipJSON, len(m.Memberships))
	for i, x := range m.Memberships {
		ms[i] = myMembershipJSON{
			TenantID: ids.Format(ids.Tenant, x.Tenant.ID.UUID()), TenantName: x.Tenant.Name,
			TenantSlug: x.Tenant.Slug, Role: string(x.Role),
		}
	}
	return gin.H{"user": toUserJSON(m.User), "memberships": ms}
}

// templateInvalid reports renderer problems with their locations. Messages
// come from the renderer and never contain payload values.
func templateInvalid(c *gin.Context, err error) {
	var es render.Errors
	var one *render.Error
	var list []*render.Error
	switch {
	case errors.As(err, &es):
		list = es
	case errors.As(err, &one):
		list = []*render.Error{one}
	}
	details := make([]errorDetail2, 0, len(list))
	for _, e := range list {
		field := e.Field
		if field == "" {
			field = "template"
		}
		details = append(details, errorDetail2{Field: field, Line: e.Line, Column: e.Column, Message: e.Msg})
	}
	msg := "the template is invalid"
	if len(details) > 0 {
		msg = details[0].Field + ": " + details[0].Message
	}
	writeErrorDetails(c, http.StatusBadRequest, "template_invalid", msg, details)
}
