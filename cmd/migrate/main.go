// Command migrate applies the versioned SQL migrations embedded from
// migrations/. It runs as a one-off ECS task before each deploy (and as a
// one-shot compose service locally). Forward-only: there is no down command;
// schema changes follow expand/contract.
//
//	migrate up        apply pending migrations (default)
//	migrate version   print the applied version
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/sbshobhit/notifex/internal/app"
	"github.com/sbshobhit/notifex/internal/config"
	"github.com/sbshobhit/notifex/internal/db"
	"github.com/sbshobhit/notifex/internal/observability"
)

func main() { os.Exit(run()) }

func run() int {
	cmd := "up"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	cfg, err := config.Load("migrate", app.Version)
	if err != nil {
		fmt.Fprintln(os.Stderr, "notifex-migrate: config:", err)
		return 2
	}
	log := observability.NewLogger(cfg, os.Stdout)
	if cfg.DatabaseURL == "" {
		log.Error("DATABASE_URL is required")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	switch cmd {
	case "up":
		v, err := db.MigrateUp(ctx, cfg.DatabaseURL, log)
		if err != nil {
			log.Error("migration failed", slog.Any("error", err))
			return 1
		}
		log.Info("schema is current", slog.Uint64("schema_version", uint64(v)))
	case "version":
		v, dirty, err := db.SchemaVersion(cfg.DatabaseURL, log)
		if err != nil {
			log.Error("read version failed", slog.Any("error", err))
			return 1
		}
		fmt.Printf("version=%d dirty=%t\n", v, dirty)
	default:
		fmt.Fprintf(os.Stderr, "usage: migrate [up|version]\n")
		return 2
	}
	return 0
}
