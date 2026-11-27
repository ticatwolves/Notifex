// Package lifecycle runs a service's long-lived components and shuts them down
// gracefully on SIGTERM/SIGINT (ECS sends SIGTERM, then SIGKILL after the stop
// timeout; Spot interruptions give two minutes).
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/ticatwolves/notifex/internal/health"
)

// Component is a long-running part of a service (HTTP server, gRPC server,
// queue consumer, scheduler loop).
type Component interface {
	Name() string
	// Start blocks until the component stops. Returning before ctx is done is
	// treated as a failure and stops the whole service.
	Start(ctx context.Context) error
	// Stop asks the component to finish in-flight work within ctx's deadline.
	Stop(ctx context.Context) error
}

// Run starts all components and blocks until ctx is cancelled (signal) or a
// component fails. On shutdown it marks readiness as draining, then stops every
// component concurrently within timeout.
func Run(ctx context.Context, log *slog.Logger, h *health.Registry, timeout time.Duration, comps ...Component) error {
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	g, gctx := errgroup.WithContext(runCtx)
	for _, c := range comps {
		g.Go(func() error {
			log.InfoContext(ctx, "component starting", slog.String("component", c.Name()))
			err := c.Start(gctx)
			if gctx.Err() == nil {
				// Exited on its own: a failure even if err is nil.
				if err == nil {
					err = errors.New("exited unexpectedly")
				}
				return fmt.Errorf("%s: %w", c.Name(), err)
			}
			return nil
		})
	}

	<-gctx.Done()
	cause := context.Cause(gctx)
	if ctx.Err() != nil {
		log.InfoContext(ctx, "shutdown signal received")
	} else {
		log.ErrorContext(ctx, "component failed; shutting down", slog.Any("error", cause))
	}
	h.SetDraining()

	stopCtx, cancelStop := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancelStop()
	sg := errgroup.Group{}
	for _, c := range comps {
		sg.Go(func() error {
			if err := c.Stop(stopCtx); err != nil {
				return fmt.Errorf("stop %s: %w", c.Name(), err)
			}
			log.InfoContext(ctx, "component stopped", slog.String("component", c.Name()))
			return nil
		})
	}
	stopErr := sg.Wait()
	cancelRun()
	runErr := g.Wait()
	return errors.Join(runErr, stopErr)
}

// HTTPServer adapts an *http.Server to Component.
type HTTPServer struct {
	name string
	srv  *http.Server
	ln   net.Listener
}

// NewHTTPServer builds a hardened http.Server for handler on addr. Binding
// happens in Listen so callers can fail fast before starting anything.
func NewHTTPServer(name, addr string, handler http.Handler, log *slog.Logger) *HTTPServer {
	return &HTTPServer{name: name, srv: &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}}
}

// Listen binds the listening socket.
func (s *HTTPServer) Listen(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.srv.Addr)
	if err != nil {
		return fmt.Errorf("%s listen %s: %w", s.name, s.srv.Addr, err)
	}
	s.ln = ln
	return nil
}

// Addr returns the bound address (useful when addr was ":0").
func (s *HTTPServer) Addr() net.Addr { return s.ln.Addr() }

// Name implements Component.
func (s *HTTPServer) Name() string { return s.name }

// Start implements Component.
func (s *HTTPServer) Start(ctx context.Context) error {
	if s.ln == nil {
		if err := s.Listen(ctx); err != nil {
			return err
		}
	}
	s.srv.BaseContext = func(net.Listener) context.Context { return context.WithoutCancel(ctx) }
	if err := s.srv.Serve(s.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Stop implements Component: stops accepting and waits for in-flight requests.
func (s *HTTPServer) Stop(ctx context.Context) error {
	return s.srv.Shutdown(ctx)
}
