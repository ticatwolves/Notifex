// Package grpctransport is the public gRPC transport. Like the REST transport it
// stays thin and calls the same service layer.
package grpctransport

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"runtime/debug"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
)

// MaxRecvMsgSize matches the REST body limit.
const MaxRecvMsgSize = 1 << 20 // 1 MiB

// Server wraps a grpc.Server with the standard health service.
type Server struct {
	*grpc.Server
	health *health.Server
}

// NewServer builds a gRPC server with tracing, panic recovery, size limits,
// keepalive enforcement and the grpc.health.v1 service.
func NewServer(log *slog.Logger) *Server {
	s := grpc.NewServer(
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.ChainUnaryInterceptor(unaryRecover(log)),
		grpc.ChainStreamInterceptor(streamRecover(log)),
		grpc.MaxRecvMsgSize(MaxRecvMsgSize),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             10 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle:     5 * time.Minute,
			MaxConnectionAge:      30 * time.Minute, // rebalance long-lived clients across tasks
			MaxConnectionAgeGrace: 30 * time.Second,
		}),
	)
	hs := health.NewServer()
	healthpb.RegisterHealthServer(s, hs)
	return &Server{Server: s, health: hs}
}

// SetServing flips the overall health status ("" service) reported to clients.
func (s *Server) SetServing(serving bool) {
	st := healthpb.HealthCheckResponse_NOT_SERVING
	if serving {
		st = healthpb.HealthCheckResponse_SERVING
	}
	s.health.SetServingStatus("", st)
}

func unaryRecover(log *slog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if rec := recover(); rec != nil {
				log.ErrorContext(ctx, "panic in grpc handler", slog.String("method", info.FullMethod),
					slog.Any("panic", rec), slog.String("stack", string(debug.Stack())))
				err = status.Error(codes.Internal, "internal error")
			}
		}()
		return h(ctx, req)
	}
}

func streamRecover(log *slog.Logger) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, h grpc.StreamHandler) (err error) {
		defer func() {
			if rec := recover(); rec != nil {
				log.ErrorContext(ss.Context(), "panic in grpc stream", slog.String("method", info.FullMethod),
					slog.Any("panic", rec), slog.String("stack", string(debug.Stack())))
				err = status.Error(codes.Internal, "internal error")
			}
		}()
		return h(srv, ss)
	}
}

// Component adapts Server to lifecycle.Component.
type Component struct {
	srv  *Server
	addr string
	ln   net.Listener
}

// NewComponent returns a lifecycle component serving srv on addr.
func NewComponent(srv *Server, addr string) *Component {
	return &Component{srv: srv, addr: addr}
}

// Listen binds the listening socket so startup fails fast on a port clash.
func (c *Component) Listen(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", c.addr)
	if err != nil {
		return fmt.Errorf("grpc listen %s: %w", c.addr, err)
	}
	c.ln = ln
	return nil
}

// Addr returns the bound address.
func (c *Component) Addr() net.Addr { return c.ln.Addr() }

// Name implements lifecycle.Component.
func (c *Component) Name() string { return "grpc" }

// Start implements lifecycle.Component.
func (c *Component) Start(ctx context.Context) error {
	if c.ln == nil {
		if err := c.Listen(ctx); err != nil {
			return err
		}
	}
	c.srv.SetServing(true)
	if err := c.srv.Serve(c.ln); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return err
	}
	return nil
}

// Stop implements lifecycle.Component: report NOT_SERVING, drain in-flight
// RPCs, and force-close if the deadline passes.
func (c *Component) Stop(ctx context.Context) error {
	c.srv.SetServing(false)
	done := make(chan struct{})
	go func() {
		c.srv.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		c.srv.Stop()
		<-done
		return fmt.Errorf("grpc graceful stop: %w", ctx.Err())
	}
}
