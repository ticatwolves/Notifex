// Package httptransport is the public REST transport (Gin). It stays thin:
// decode, authenticate, call a service, encode. No business logic here.
package httptransport

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"

	"github.com/sbshobhit/notifex/internal/health"
)

// MaxRequestBody caps every request body on the public API.
const MaxRequestBody = 1 << 20 // 1 MiB

// Deps are the router's collaborators.
type Deps struct {
	Service string
	Logger  *slog.Logger
	Metrics prometheus.Registerer
	Health  *health.Registry
}

// NewRouter builds the public REST handler.
func NewRouter(d Deps) http.Handler {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.HandleMethodNotAllowed = true
	r.ContextWithFallback = true // c.Request.Context() values reachable via c
	_ = r.SetTrustedProxies(nil) // ALB/CloudFront client IP handling is decided in Phase 1

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

	return r
}

// errorBody is the error envelope for every REST error (codes per docs/03).
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

func writeError(c *gin.Context, status int, code, msg string) {
	c.AbortWithStatusJSON(status, errorBody{Error: errorDetail{
		Code: code, Message: msg, RequestID: c.GetString(ctxRequestID),
	}})
}
