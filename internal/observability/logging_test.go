package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/sbshobhit/notifex/internal/config"
)

func testLogger(buf *bytes.Buffer) *slog.Logger {
	return NewLogger(config.Config{Service: "api", Env: "local", Version: "t", LogFormat: "json", LogLevel: slog.LevelDebug}, buf)
}

func decode(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &m))
	return m
}

func TestLoggerAddsServiceAndCorrelation(t *testing.T) {
	var buf bytes.Buffer
	log := testLogger(&buf)

	tp := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	ctx, span := tp.Tracer("t").Start(context.Background(), "op")
	defer span.End()
	ctx = WithLogAttrs(ctx, slog.String(KeyTenantID, "t1"))
	ctx = WithLogAttrs(ctx, slog.String(KeyRunID, "r1"))

	log.InfoContext(ctx, "hello")
	m := decode(t, &buf)
	assert.Equal(t, "api", m["service"])
	assert.Equal(t, "local", m["env"])
	assert.Equal(t, "t1", m[KeyTenantID])
	assert.Equal(t, "r1", m[KeyRunID])
	assert.Equal(t, span.SpanContext().TraceID().String(), m[KeyTraceID])
	assert.Equal(t, span.SpanContext().SpanID().String(), m[KeySpanID])
}

func TestLoggerRedactsSensitiveKeys(t *testing.T) {
	tests := []struct {
		name string
		attr slog.Attr
		path []string
	}{
		{name: "password", attr: slog.String("password", "hunter2"), path: []string{"password"}},
		{name: "case-insensitive", attr: slog.String("Authorization", "Bearer nfx_x"), path: []string{"Authorization"}},
		{name: "api_key", attr: slog.String("api_key", "nfx_live_abc"), path: []string{"api_key"}},
		{name: "payload", attr: slog.Any("payload", map[string]any{"order": 1}), path: []string{"payload"}},
		{name: "nested in group", attr: slog.Group("req", slog.String("token", "abc")), path: []string{"req", "token"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			testLogger(&buf).Info("x", tt.attr)
			var v any = decode(t, &buf)
			for _, k := range tt.path {
				v = v.(map[string]any)[k]
			}
			assert.Equal(t, redacted, v)
		})
	}
}

func TestMaskEmail(t *testing.T) {
	tests := map[string]string{
		"alice@example.com":  "a***@e***.com",
		"a@d.co.in":          "a***@d***.in",
		"bob@localhost":      "b***@***",
		"not-an-email":       "***",
		"@example.com":       "***",
		"alice@":             "***",
		"ünï@exämple.de":     "ü***@e***.de",
		"x@y.z":              "x***@y***.z",
		"weird@@example.com": "w***@e***.com",
		"":                   "***",
	}
	for in, want := range tests {
		assert.Equal(t, want, MaskEmail(in), "input %q", in)
	}
}

func TestMaskPhone(t *testing.T) {
	tests := map[string]string{
		"+919999991234":     "+91******1234",
		"+1 (415) 555-0100": "+14*****0100",
		"9999991234":        "******1234",
		"+9112345":          "***",
		"1234":              "***",
		"":                  "***",
	}
	for in, want := range tests {
		assert.Equal(t, want, MaskPhone(in), "input %q", in)
	}
}
