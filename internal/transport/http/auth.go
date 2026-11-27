package httptransport

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/ticatwolves/notifex/internal/auth"
	"github.com/ticatwolves/notifex/internal/ids"
	"github.com/ticatwolves/notifex/internal/observability"
	"github.com/ticatwolves/notifex/internal/ratelimit"
)

// APIKeyAuthenticator is what the transport needs from internal/auth.
type APIKeyAuthenticator interface {
	Authenticate(ctx context.Context, raw string) (auth.Principal, error)
}

const ctxPrincipal = "principal"

// requireAPIKey authenticates `Authorization: Bearer <api key>`. Every
// credential failure is the same 401; an undecidable check is a 503. Clients
// that keep failing are rate-limited per IP (threat model R1).
func requireAPIKey(a APIKeyAuthenticator, fail *ratelimit.Limiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		ip := c.ClientIP()
		if blocked, retry := fail.Blocked(ip); blocked {
			rateLimited(c, "rate_limited", retry)
			return
		}
		raw, ok := bearer(c.GetHeader("Authorization"))
		if !ok {
			fail.Allow(ip)
			unauthenticated(c)
			return
		}
		p, err := a.Authenticate(c.Request.Context(), raw)
		switch {
		case errors.Is(err, auth.ErrUnavailable):
			c.Header("Retry-After", "1")
			writeError(c, http.StatusServiceUnavailable, "unavailable", "authentication temporarily unavailable")
			return
		case err != nil:
			fail.Allow(ip)
			unauthenticated(c)
			return
		}
		c.Set(ctxPrincipal, p)
		ctx := observability.WithLogAttrs(c.Request.Context(),
			slog.String(observability.KeyTenantID, p.TenantID.String()),
			slog.String("application_id", p.ApplicationID.String()),
			slog.String("api_key_id", p.APIKeyID.String()),
		)
		trace.SpanFromContext(ctx).SetAttributes(
			attribute.String(observability.KeyTenantID, p.TenantID.String()),
			attribute.String("notifex.api_key_id", p.APIKeyID.String()),
		)
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

func bearer(h string) (string, bool) {
	const scheme = "bearer "
	if len(h) <= len(scheme) || !strings.EqualFold(h[:len(scheme)], scheme) {
		return "", false
	}
	tok := strings.TrimSpace(h[len(scheme):])
	return tok, tok != ""
}

func unauthenticated(c *gin.Context) {
	c.Header("WWW-Authenticate", `Bearer realm="notifex"`)
	writeError(c, http.StatusUnauthorized, "unauthenticated", "missing or invalid API key")
}

func principal(c *gin.Context) auth.Principal {
	p, _ := c.Get(ctxPrincipal)
	return p.(auth.Principal) // set by requireAPIKey before any handler runs
}

// whoamiResponse is GET /v1/whoami.
type whoamiResponse struct {
	TenantID      string   `json:"tenant_id"`
	ApplicationID string   `json:"application_id"`
	EnvironmentID string   `json:"environment_id"`
	Environment   string   `json:"environment"`
	APIKeyID      string   `json:"api_key_id"`
	APIKeyPrefix  string   `json:"api_key_prefix"`
	Scopes        []string `json:"scopes"`
}

func whoami(c *gin.Context) {
	p := principal(c)
	scopes := p.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	c.JSON(http.StatusOK, whoamiResponse{
		TenantID:      ids.Format(ids.Tenant, p.TenantID.UUID()),
		ApplicationID: ids.Format(ids.Application, p.ApplicationID),
		EnvironmentID: ids.Format(ids.Environment, p.EnvironmentID),
		Environment:   string(p.Environment),
		APIKeyID:      ids.Format(ids.APIKey, p.APIKeyID),
		APIKeyPrefix:  p.APIKeyPrefix,
		Scopes:        scopes,
	})
}
