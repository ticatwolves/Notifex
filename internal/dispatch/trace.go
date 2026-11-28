package dispatch

import (
	"context"

	"go.opentelemetry.io/otel/propagation"
)

var traceContext = propagation.TraceContext{}

// TraceParent returns the W3C traceparent of ctx's span ("" if none), for
// the envelope (CLAUDE.md: propagate traceparent through the SQS envelope).
func TraceParent(ctx context.Context) string {
	c := propagation.MapCarrier{}
	traceContext.Inject(ctx, c)
	return c.Get("traceparent")
}

// ContextWithTraceParent continues the trace an envelope carries.
func ContextWithTraceParent(ctx context.Context, tp string) context.Context {
	if tp == "" {
		return ctx
	}
	return traceContext.Extract(ctx, propagation.MapCarrier{"traceparent": tp})
}
