// Package app holds the bootstrap shared by every Notifex binary: config,
// logger, tracing, metrics registry, health registry and the ops server.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/sbshobhit/notifex/internal/config"
	"github.com/sbshobhit/notifex/internal/health"
	"github.com/sbshobhit/notifex/internal/lifecycle"
	"github.com/sbshobhit/notifex/internal/observability"
	httptransport "github.com/sbshobhit/notifex/internal/transport/http"
)

// Version is set at build time with -ldflags "-X github.com/sbshobhit/notifex/internal/app.Version=…".
var Version = ""

// Runtime is what a service's build function gets to wire its components.
type Runtime struct {
	Config  config.Config
	Logger  *slog.Logger
	Metrics *prometheus.Registry
	Health  *health.Registry
}

// BuildFunc returns the service-specific components. The ops server is added
// by Main and must not be returned here.
type BuildFunc func(ctx context.Context, rt *Runtime) ([]lifecycle.Component, error)

// Main runs a service to completion and returns the process exit code.
func Main(service string, build BuildFunc) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	cfg, err := config.Load(service, version())
	if err != nil {
		// No logger yet; config errors never contain secret values (env lib reports names only).
		fmt.Fprintf(os.Stderr, "notifex-%s: config: %v\n", service, err)
		return 2
	}
	log := observability.NewLogger(cfg, os.Stdout)
	slog.SetDefault(log)

	if err := run(ctx, cfg, log, build); err != nil {
		log.Error("service exited with error", slog.Any("error", err))
		return 1
	}
	log.Info("service stopped")
	return 0
}

func run(ctx context.Context, cfg config.Config, log *slog.Logger, build BuildFunc) error {
	shutdownTracing, err := observability.SetupTracing(ctx, cfg)
	if err != nil {
		return fmt.Errorf("tracing: %w", err)
	}
	defer func() {
		tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := shutdownTracing(tctx); err != nil {
			log.Warn("tracing shutdown", slog.Any("error", err))
		}
	}()

	rt := &Runtime{
		Config:  cfg,
		Logger:  log,
		Metrics: observability.NewRegistry(),
		Health:  health.NewRegistry(2 * time.Second),
	}
	buildInfo := prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "notifex_build_info",
		Help:        "Build metadata; always 1.",
		ConstLabels: prometheus.Labels{"service": cfg.Service, "version": cfg.Version},
	})
	buildInfo.Set(1)
	rt.Metrics.MustRegister(buildInfo)

	comps, err := build(ctx, rt)
	if err != nil {
		return fmt.Errorf("build %s: %w", cfg.Service, err)
	}

	ops := lifecycle.NewHTTPServer("ops", cfg.OpsAddr, httptransport.NewOpsHandler(rt.Metrics, rt.Health), log)
	if err := ops.Listen(ctx); err != nil {
		return err
	}
	comps = append(comps, ops)

	log.Info("service starting", slog.String("ops_addr", cfg.OpsAddr))
	return lifecycle.Run(ctx, log, rt.Health, cfg.ShutdownTimeout, comps...)
}

// version prefers the -ldflags value and falls back to the VCS revision Go
// embeds in the binary.
func version() string {
	if Version != "" {
		return Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 12 {
				return s.Value[:12]
			}
		}
	}
	return "dev"
}
