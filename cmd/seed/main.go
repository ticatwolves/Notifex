// Command seed provisions a local development tenant and prints a fresh API
// key. It refuses to run outside NOTIFEX_ENV=local. The key goes to stdout
// (once); logs go to stderr.
//
//	make seed
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/sbshobhit/notifex/internal/app"
	"github.com/sbshobhit/notifex/internal/auth"
	"github.com/sbshobhit/notifex/internal/config"
	"github.com/sbshobhit/notifex/internal/crypto"
	"github.com/sbshobhit/notifex/internal/db"
	"github.com/sbshobhit/notifex/internal/devseed"
	"github.com/sbshobhit/notifex/internal/observability"
	"github.com/sbshobhit/notifex/internal/store"
	"github.com/sbshobhit/notifex/internal/store/entstore"
)

func main() { os.Exit(run()) }

func run() int {
	slug := flag.String("tenant", "acme-dev", "tenant slug")
	email := flag.String("owner", "dev@example.com", "owner email")
	appKey := flag.String("app", "shop", "application key")
	env := flag.String("env", "dev", "environment: dev | staging | prod")
	apiURL := flag.String("api", "http://localhost:8080", "API base URL for the printed curl command")
	flag.Parse()

	cfg, err := config.Load("seed", app.Version)
	if err != nil {
		fmt.Fprintln(os.Stderr, "seed: config:", err)
		return 2
	}
	log := observability.NewLogger(cfg, os.Stderr)
	if cfg.Env != config.EnvLocal {
		log.Error("seed only runs with NOTIFEX_ENV=local", slog.String("env", cfg.Env))
		return 2
	}
	hasher, err := auth.NewHasher(cfg.APIKeyPepper)
	if err != nil {
		log.Error("API_KEY_PEPPER", slog.Any("error", err))
		return 2
	}
	kp, err := crypto.NewLocalFromBase64(cfg.LocalMasterKey)
	if err != nil {
		log.Error("NOTIFEX_LOCAL_MASTER_KEY", slog.Any("error", err))
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	sqlDB, err := db.Open(ctx, cfg.DatabaseURL, db.PoolConfig{})
	if err != nil {
		log.Error("open database", slog.Any("error", err))
		return 1
	}
	defer func() { _ = sqlDB.Close() }()

	res, err := devseed.Run(ctx, entstore.New(sqlDB), kp, hasher, devseed.Options{
		TenantSlug: *slug, OwnerEmail: *email, AppKey: *appKey, Env: store.EnvironmentName(*env),
	})
	if err != nil {
		log.Error("seed failed", slog.Any("error", err))
		return 1
	}
	log.Info("seeded",
		slog.String("tenant_id", res.Tenant.ID.String()), slog.String("tenant_slug", res.Tenant.Slug),
		slog.String("application_id", res.Application.ID.String()), slog.String("environment", string(res.Environment.Name)),
		slog.String("api_key_id", res.APIKey.Key.ID.String()), slog.String("api_key_prefix", res.APIKey.Key.Prefix))

	fmt.Printf("API key (shown once): %s\n\n", res.APIKey.Plaintext)
	fmt.Printf("curl -s -H 'Authorization: Bearer %s' %s/v1/whoami\n", res.APIKey.Plaintext, *apiURL)
	return 0
}
