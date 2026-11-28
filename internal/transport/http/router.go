// Package httptransport is the public REST transport (Gin). It stays thin:
// decode, authenticate, call a service, encode. No business logic here.
package httptransport

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"

	"github.com/ticatwolves/notifex/internal/health"
)

// MaxRequestBody caps every request body on the public API.
const MaxRequestBody = 1 << 20 // 1 MiB

// Deps are the router's collaborators.
type Deps struct {
	Service string
	Logger  *slog.Logger
	Metrics prometheus.Registerer
	Health  *health.Registry
	// APIKeys authenticates /v1 data-plane routes. Nil disables them (tests of
	// the bare router only).
	APIKeys APIKeyAuthenticator
	// Accounts and Sessions enable the console routes (/v1/auth, /v1/me).
	Accounts ConsoleAccounts
	Sessions SessionAuthenticator
	// OAuth enables provider sign-in and identity linking (optional).
	OAuth ConsoleOAuth
	// Tenants enables /v1/admin/tenants and invitation acceptance.
	Tenants TenantAdmin
	// Workflows enables workflow, action, template and preview routes
	// (needs Tenants and Sessions).
	Workflows WorkflowAdmin
	// Credentials enables channel credential routes (needs Tenants and Sessions).
	Credentials CredentialAdmin
	// Operators enables the operator realm (/v1/operator).
	Operators OperatorRealm
	// Limits are per-IP limits; zero value means DefaultLimits.
	Limits Limits
	// TrustedProxies are CIDRs whose X-Forwarded-For is believed (the ALB).
	TrustedProxies []string
}

// NewRouter builds the public REST handler.
func NewRouter(d Deps) http.Handler {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.HandleMethodNotAllowed = true
	r.ContextWithFallback = true // c.Request.Context() values reachable via c
	// Client IP for rate limits: X-Forwarded-For only from configured proxies.
	if err := r.SetTrustedProxies(d.TrustedProxies); err != nil {
		_ = r.SetTrustedProxies(nil) // config validation rejects bad CIDRs; fail safe
	}
	if d.Limits.Login == nil {
		d.Limits = DefaultLimits()
	}
	if d.Limits.Preview == nil {
		d.Limits.Preview = DefaultLimits().Preview
	}

	m := newHTTPMetrics(d.Metrics)
	r.Use(
		recoverer(d.Logger),
		requestID(),
		otelgin.Middleware("notifex-"+d.Service),
		m.middleware(),
		accessLog(d.Logger),
		limitBody(MaxRequestBody),
	)

	r.NoRoute(func(c *gin.Context) { writeError(c, http.StatusNotFound, "not_found", "route not found") })
	r.NoMethod(func(c *gin.Context) {
		writeError(c, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	})

	// ALB health check. Liveness only; dependency detail lives on the ops port.
	r.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	// Public readiness reports status only, never which dependency failed.
	r.GET("/readyz", func(c *gin.Context) {
		if ok, _ := d.Health.Check(c.Request.Context()); !ok {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unavailable"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	if d.APIKeys != nil {
		r.GET("/v1/whoami", requireAPIKey(d.APIKeys, d.Limits.APIKeyFail), whoami)
	}
	if d.Accounts != nil && d.Sessions != nil {
		registerConsole(r, d)
	}
	if d.Tenants != nil && d.Sessions != nil {
		registerAdmin(r, d)
	}
	if d.Operators != nil {
		registerOperator(r, d)
	}

	return r
}

// errorBody is the error envelope for every REST error (codes per docs/03).
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	RequestID string         `json:"request_id,omitempty"`
	Details   []errorDetail2 `json:"details,omitempty"`
}

// errorDetail2 locates one problem (template_invalid).
type errorDetail2 struct {
	Field   string `json:"field"`
	Line    int    `json:"line,omitempty"`
	Column  int    `json:"column,omitempty"`
	Message string `json:"message"`
}

func writeError(c *gin.Context, status int, code, msg string) {
	writeErrorDetails(c, status, code, msg, nil)
}

func writeErrorDetails(c *gin.Context, status int, code, msg string, details []errorDetail2) {
	c.AbortWithStatusJSON(status, errorBody{Error: errorDetail{
		Code: code, Message: msg, RequestID: c.GetString(ctxRequestID), Details: details,
	}})
}
