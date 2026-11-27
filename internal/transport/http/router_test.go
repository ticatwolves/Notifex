package httptransport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ticatwolves/notifex/internal/health"
)

type fixture struct {
	handler http.Handler
	reg     *prometheus.Registry
	health  *health.Registry
	logs    *bytes.Buffer
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	var logs bytes.Buffer
	f := fixture{
		reg:    prometheus.NewRegistry(),
		health: health.NewRegistry(50 * time.Millisecond),
		logs:   &logs,
	}
	f.handler = NewRouter(Deps{
		Service: "api",
		Logger:  slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Metrics: f.reg,
		Health:  f.health,
	})
	return f
}

func (f fixture) do(method, path string, body io.Reader, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func TestHealthEndpoints(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		check      health.CheckFunc
		drain      bool
		wantStatus int
	}{
		{name: "healthz", path: "/healthz", wantStatus: http.StatusOK},
		{name: "healthz ignores failing deps", path: "/healthz",
			check: func(context.Context) error { return errors.New("db down") }, wantStatus: http.StatusOK},
		{name: "readyz ok", path: "/readyz", wantStatus: http.StatusOK},
		{name: "readyz failing dep", path: "/readyz",
			check: func(context.Context) error { return errors.New("db down") }, wantStatus: http.StatusServiceUnavailable},
		{name: "readyz draining", path: "/readyz", drain: true, wantStatus: http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			if tt.check != nil {
				f.health.Register("db", tt.check)
			}
			if tt.drain {
				f.health.SetDraining()
			}
			rec := f.do(http.MethodGet, tt.path, nil, nil)
			assert.Equal(t, tt.wantStatus, rec.Code)
			assert.NotContains(t, rec.Body.String(), "db down", "public endpoints must not leak dependency detail")
		})
	}
}

func TestNotFoundUsesErrorEnvelope(t *testing.T) {
	f := newFixture(t)
	rec := f.do(http.MethodGet, "/v1/nope?email=a@b.com", nil, nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
	var body errorBody
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "not_found", body.Error.Code)
	assert.NotEmpty(t, body.Error.RequestID)
	assert.NotContains(t, f.logs.String(), "a@b.com", "access log must not include query strings")
	assert.NotContains(t, f.logs.String(), "/v1/nope", "access log must log route templates, not raw paths")
}

func TestRequestID(t *testing.T) {
	tests := []struct {
		name     string
		incoming string
		keep     bool
	}{
		{name: "valid id is echoed", incoming: "abc-123_x.y", keep: true},
		{name: "missing id is generated"},
		{name: "oversized id is replaced", incoming: strings.Repeat("a", 65)},
		{name: "log injection is replaced", incoming: "abc\n{\"level\":\"ERROR\"}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			rec := f.do(http.MethodGet, "/healthz", nil, map[string]string{headerRequestID: tt.incoming})
			got := rec.Header().Get(headerRequestID)
			require.NotEmpty(t, got)
			if tt.keep {
				assert.Equal(t, tt.incoming, got)
			} else {
				assert.NotEqual(t, tt.incoming, got)
				assert.Regexp(t, validRequestID, got)
			}
		})
	}
}

func TestPanicRecovered(t *testing.T) {
	f := newFixture(t)
	r := f.handler.(*gin.Engine)
	r.GET("/boom", func(*gin.Context) { panic("kaboom") })
	rec := f.do(http.MethodGet, "/boom", nil, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Contains(t, rec.Body.String(), `"code":"internal"`)
	assert.NotContains(t, rec.Body.String(), "kaboom", "panic values must not reach clients")
	assert.Contains(t, f.logs.String(), "panic in http handler")
}

func TestBodyLimit(t *testing.T) {
	f := newFixture(t)
	r := f.handler.(*gin.Engine)
	r.POST("/echo", func(c *gin.Context) {
		if _, err := io.ReadAll(c.Request.Body); err != nil {
			writeError(c, http.StatusRequestEntityTooLarge, "payload_too_large", "too large")
			return
		}
		c.Status(http.StatusNoContent)
	})

	big := bytes.Repeat([]byte("x"), MaxRequestBody+1)
	rec := f.do(http.MethodPost, "/echo", bytes.NewReader(big), nil)
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, "declared Content-Length over limit")

	// Chunked body with no Content-Length: limit enforced while reading.
	req := httptest.NewRequest(http.MethodPost, "/echo", io.NopCloser(bytes.NewReader(big)))
	req.ContentLength = -1
	rec = httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, "streamed body over limit")

	rec = f.do(http.MethodPost, "/echo", strings.NewReader("ok"), nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestMetricsUseRouteTemplateAndBoundedMethod(t *testing.T) {
	f := newFixture(t)
	f.do(http.MethodGet, "/healthz", nil, nil)
	f.do(http.MethodGet, "/random/path/1", nil, nil)
	f.do("BREW", "/healthz", nil, nil)

	assert.InDelta(t, 1, f.requestCount(t, "GET", "/healthz", "200"), 0)
	assert.InDelta(t, 1, f.requestCount(t, "GET", unmatchedRoute, "404"), 0)
	assert.InDelta(t, 1, f.requestCount(t, "OTHER", unmatchedRoute, "405"), 0)
}

func (f fixture) requestCount(t *testing.T, method, route, code string) float64 {
	t.Helper()
	families, err := f.reg.Gather()
	require.NoError(t, err)
	for _, fam := range families {
		if fam.GetName() != "notifex_http_requests_total" {
			continue
		}
		for _, m := range fam.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["method"] == method && labels["route"] == route && labels["code"] == code {
				return m.GetCounter().GetValue()
			}
		}
	}
	t.Fatalf("no counter for %s %s %s", method, route, code)
	return 0
}

func TestOpsHandler(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := health.NewRegistry(50 * time.Millisecond)
	h.Register("db", func(context.Context) error { return errors.New("db down") })
	ops := NewOpsHandler(reg, h)

	rec := httptest.NewRecorder()
	ops.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "db down", "ops readiness includes detail")

	rec = httptest.NewRecorder()
	ops.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	assert.Equal(t, http.StatusOK, rec.Code)

	rec = httptest.NewRecorder()
	ops.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
}
