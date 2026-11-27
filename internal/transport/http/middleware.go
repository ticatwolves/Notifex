package httptransport

import (
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/ticatwolves/notifex/internal/observability"
)

const (
	headerRequestID = "X-Request-ID"
	ctxRequestID    = "request_id"
	unmatchedRoute  = "unmatched"
)

// validRequestID bounds caller-supplied request ids so they can't be used for
// log injection or to bloat logs.
var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// requestID accepts a well-formed X-Request-ID or generates one, echoes it on
// the response, and adds it to the log context.
func requestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(headerRequestID)
		if !validRequestID.MatchString(id) {
			id = uuid.NewString()
		}
		c.Set(ctxRequestID, id)
		c.Header(headerRequestID, id)
		c.Request = c.Request.WithContext(observability.WithLogAttrs(
			c.Request.Context(), slog.String(observability.KeyRequestID, id)))
		c.Next()
	}
}

// recoverer turns a handler panic into a 500 with a logged stack. It must be
// the outermost middleware.
func recoverer(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler { //nolint:errorlint // sentinel panic value, compared by identity as net/http does
					panic(rec)
				}
				log.ErrorContext(c.Request.Context(), "panic in http handler",
					slog.Any("panic", rec), slog.String("stack", string(debug.Stack())))
				writeError(c, http.StatusInternalServerError, "internal", "internal error")
			}
		}()
		c.Next()
	}
}

// accessLog logs one line per request. It logs the route template, never the
// raw path or query string, which can carry ids or PII.
func accessLog(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		route := c.FullPath()
		if route == "" {
			route = unmatchedRoute
		}
		status := c.Writer.Status()
		level := slog.LevelInfo
		switch {
		case status >= 500:
			level = slog.LevelError
		case route == "/healthz" || route == "/readyz":
			level = slog.LevelDebug
		}
		log.Log(c.Request.Context(), level, "http request",
			slog.String("method", c.Request.Method),
			slog.String("route", route),
			slog.Int("status", status),
			slog.Duration("duration", time.Since(start)),
			slog.Int("bytes_out", c.Writer.Size()),
		)
	}
}

// limitBody caps the request body; reads beyond the limit fail.
func limitBody(n int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.ContentLength > n {
			writeError(c, http.StatusRequestEntityTooLarge, "payload_too_large",
				"request body exceeds "+strconv.FormatInt(n, 10)+" bytes")
			return
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, n)
		c.Next()
	}
}

type httpMetrics struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	inflight prometheus.Gauge
}

func newHTTPMetrics(reg prometheus.Registerer) *httpMetrics {
	m := &httpMetrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "notifex_http_requests_total",
			Help: "HTTP requests by method, route template and status code.",
		}, []string{"method", "route", "code"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "notifex_http_request_duration_seconds",
			Help:    "HTTP request latency by method and route template.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}, []string{"method", "route"}),
		inflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "notifex_http_requests_in_flight",
			Help: "HTTP requests currently being served.",
		}),
	}
	reg.MustRegister(m.requests, m.duration, m.inflight)
	return m
}

func (m *httpMetrics) middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		m.inflight.Inc()
		start := time.Now()
		defer m.inflight.Dec()
		c.Next()
		route := c.FullPath() // template, bounded cardinality
		if route == "" {
			route = unmatchedRoute
		}
		method := metricMethod(c.Request.Method)
		m.requests.WithLabelValues(method, route, strconv.Itoa(c.Writer.Status())).Inc()
		m.duration.WithLabelValues(method, route).Observe(time.Since(start).Seconds())
	}
}

// metricMethod maps the request method onto a fixed set so a client sending
// arbitrary methods can't blow up label cardinality.
func metricMethod(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return m
	default:
		return "OTHER"
	}
}
