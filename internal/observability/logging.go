// Package observability wires structured logging, tracing and metrics.
package observability

import (
	"context"
	"io"
	"log/slog"
	"strings"

	"go.opentelemetry.io/otel/trace"

	"github.com/ticatwolves/notifex/internal/config"
)

// Correlation keys used across the codebase. Logs, spans and metrics must use
// these exact names so they join up in queries.
const (
	KeyTenantID       = "tenant_id"
	KeyRunID          = "run_id"
	KeyNotificationID = "notification_id"
	KeyChannel        = "channel"
	KeyTraceID        = "trace_id"
	KeySpanID         = "span_id"
	KeyRequestID      = "request_id"
)

const redacted = "[REDACTED]"

// sensitiveKeys are attribute keys whose values are always redacted, at any
// nesting depth. Matching is case-insensitive on the full key.
var sensitiveKeys = map[string]struct{}{
	"password":      {},
	"secret":        {},
	"token":         {},
	"api_key":       {},
	"apikey":        {},
	"authorization": {},
	"cookie":        {},
	"set-cookie":    {},
	"credential":    {},
	"credentials":   {},
	"private_key":   {},
	"access_token":  {},
	"refresh_token": {},
	"client_secret": {},
	"payload":       {},
	"body":          {},
	"recipient":     {},
	"recipients":    {},
}

// NewLogger builds the process logger. Every record gets service/env/version,
// the active trace and span ids, and any correlation attrs stored in ctx via
// WithLogAttrs. Sensitive keys are redacted as a last line of defence; callers
// must still mask PII themselves (see MaskEmail, MaskPhone).
func NewLogger(cfg config.Config, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{
		Level:       cfg.LogLevel,
		ReplaceAttr: redactAttr,
	}
	var h slog.Handler
	if cfg.LogFormat == "text" {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}
	return slog.New(&contextHandler{Handler: h}).With(
		slog.String("service", cfg.Service),
		slog.String("env", cfg.Env),
		slog.String("version", cfg.Version),
	)
}

func redactAttr(_ []string, a slog.Attr) slog.Attr {
	if _, ok := sensitiveKeys[strings.ToLower(a.Key)]; ok {
		return slog.String(a.Key, redacted)
	}
	return a
}

type logAttrsKey struct{}

// WithLogAttrs returns a ctx carrying correlation attrs (tenant_id, run_id, …)
// that every log call made with this ctx will include.
func WithLogAttrs(ctx context.Context, attrs ...slog.Attr) context.Context {
	existing, _ := ctx.Value(logAttrsKey{}).([]slog.Attr)
	merged := make([]slog.Attr, 0, len(existing)+len(attrs))
	merged = append(merged, existing...)
	merged = append(merged, attrs...)
	return context.WithValue(ctx, logAttrsKey{}, merged)
}

// contextHandler adds trace ids and ctx-carried correlation attrs to records.
type contextHandler struct {
	slog.Handler
}

func (h *contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(
			slog.String(KeyTraceID, sc.TraceID().String()),
			slog.String(KeySpanID, sc.SpanID().String()),
		)
	}
	if attrs, ok := ctx.Value(logAttrsKey{}).([]slog.Attr); ok {
		r.AddAttrs(attrs...)
	}
	return h.Handler.Handle(ctx, r)
}

func (h *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &contextHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *contextHandler) WithGroup(name string) slog.Handler {
	return &contextHandler{Handler: h.Handler.WithGroup(name)}
}
