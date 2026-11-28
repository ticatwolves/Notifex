// Command api serves the public REST and gRPC APIs (trigger intake and the
// control plane) plus the internal ops port.
package main

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/ticatwolves/notifex/internal/app"
	"github.com/ticatwolves/notifex/internal/auth"
	"github.com/ticatwolves/notifex/internal/credential"
	"github.com/ticatwolves/notifex/internal/crypto"
	"github.com/ticatwolves/notifex/internal/db"
	"github.com/ticatwolves/notifex/internal/dispatch"
	"github.com/ticatwolves/notifex/internal/dispatch/sqsqueue"
	"github.com/ticatwolves/notifex/internal/lifecycle"
	"github.com/ticatwolves/notifex/internal/mail"
	"github.com/ticatwolves/notifex/internal/operator"
	"github.com/ticatwolves/notifex/internal/ratelimit"
	"github.com/ticatwolves/notifex/internal/store"
	"github.com/ticatwolves/notifex/internal/store/entstore"
	"github.com/ticatwolves/notifex/internal/tenant"
	grpctransport "github.com/ticatwolves/notifex/internal/transport/grpc"
	httptransport "github.com/ticatwolves/notifex/internal/transport/http"
	"github.com/ticatwolves/notifex/internal/trigger"
	"github.com/ticatwolves/notifex/internal/workflow"
)

func main() { os.Exit(app.Main("api", build)) }

func build(ctx context.Context, rt *app.Runtime) ([]lifecycle.Component, error) {
	if rt.Config.DatabaseURL == "" {
		return nil, errors.New("DATABASE_URL is required")
	}
	hasher, err := auth.NewHasher(rt.Config.APIKeyPepper)
	if err != nil {
		return nil, err
	}
	sqlDB, err := db.Open(ctx, rt.Config.DatabaseURL, db.PoolConfig{})
	if err != nil {
		return nil, err
	}
	rt.OnShutdown(sqlDB.Close) // after servers drain
	rt.Health.Register("postgres", sqlDB.PingContext)
	st := entstore.New(sqlDB)

	mailer, err := systemMailer(rt)
	if err != nil {
		return nil, err
	}
	// One hasher, so console and operator logins share the argon2 memory bound.
	pw := auth.NewPasswordHasher(rt.Config.PasswordHashConcurrency)
	accounts := auth.NewAccounts(st, pw, mailer, rt.Logger,
		rt.Config.ConsoleURL, auth.DefaultSessionPolicy)
	if err := auth.ValidateRedirectURIs(rt.Config.OAuthRedirectURIs); err != nil {
		return nil, err
	}
	oauth := auth.NewOAuth(accounts, oauthProviders(ctx, rt), rt.Config.OAuthRedirectURIs)
	keys, err := app.KeyProvider(rt)
	if err != nil {
		return nil, err
	}
	tenants := tenant.NewService(st, keys, mailer, rt.Logger, rt.Config.ConsoleURL, tenant.DefaultPolicy)
	tenants.KeyHasher = hasher
	// One field cipher per process: it caches unwrapped tenant DEKs, and a
	// crypto-shred drops the tenant's key from it at once.
	cipher := crypto.NewCipher(keys, st.Tenants(), crypto.CipherOptions{Metrics: rt.Metrics})
	tenants.OnShred = func(id store.TenantID) { cipher.Forget(id.UUID()) }
	credSvc := credential.NewService(st, cipher)
	recipientKey, err := base64.StdEncoding.DecodeString(rt.Config.RecipientHashKey)
	if err != nil || len(recipientKey) < 32 {
		return nil, errors.New("RECIPIENT_HASH_KEY must be base64 of at least 32 bytes")
	}
	// Fast path: publish right after commit; the scheduler's relay publishes
	// anything this misses (ADR-004).
	sqsClient, err := sqsqueue.NewClient(ctx, rt.Config.AWSRegion, rt.Config.SQSEndpoint)
	if err != nil {
		return nil, err
	}
	queue := sqsqueue.New(sqsClient, sqsqueue.Options{
		Prefix: rt.Config.QueuePrefix, FairQueues: rt.Config.SQSFairQueues, Log: rt.Logger, Metrics: rt.Metrics,
	})
	triggers := trigger.NewService(st, tenants, credSvc, cipher, dispatch.OutboxPublisher{Q: queue},
		trigger.Config{RecipientHashKey: recipientKey, PlatformSMSDailyCap: rt.Config.PlatformSMSDailyCap}, rt.Logger, rt.Metrics)
	limits := httptransport.DefaultLimits()
	limits.Trigger = ratelimit.New(rt.Config.TriggerRatePerMinute, max(rt.Config.TriggerRatePerMinute/6, 1), 100_000)

	rest := lifecycle.NewHTTPServer("http", rt.Config.HTTPAddr, httptransport.NewRouter(httptransport.Deps{
		Service:        rt.Config.Service,
		Logger:         rt.Logger,
		Metrics:        rt.Metrics,
		Health:         rt.Health,
		APIKeys:        auth.NewAPIKeyAuthenticator(st, hasher, rt.Logger, rt.Metrics),
		Accounts:       accounts,
		Sessions:       auth.NewSessionAuthenticator(st, rt.Logger, auth.DefaultSessionPolicy),
		OAuth:          oauth,
		Tenants:        tenants,
		Workflows:      workflow.NewService(st, tenants, rt.Logger),
		Credentials:    credSvc,
		Triggers:       triggers,
		Limits:         limits,
		Operators:      operator.NewService(st, keys, pw, rt.Logger, operator.DefaultPolicy),
		TrustedProxies: rt.Config.TrustedProxies,
	}), rt.Logger)
	if err := rest.Listen(ctx); err != nil {
		return nil, err
	}

	grpc := grpctransport.NewComponent(grpctransport.NewServer(rt.Logger), rt.Config.GRPCAddr)
	if err := grpc.Listen(ctx); err != nil {
		return nil, err
	}

	return []lifecycle.Component{rest, grpc}, nil
}

// systemMailer sends Notifex's own email asynchronously. Local runs without
// SMTP_ADDR record (and drop) mail; deployed environments must configure it.
func systemMailer(rt *app.Runtime) (mail.Mailer, error) {
	var next mail.Mailer
	switch {
	case rt.Config.SMTPAddr != "":
		next = mail.SMTP{Addr: rt.Config.SMTPAddr, From: rt.Config.MailFrom}
	case rt.Config.Deployed():
		return nil, errors.New("SMTP_ADDR is required outside local")
	default:
		rt.Logger.Warn("SMTP_ADDR not set: system email is recorded in memory and not delivered")
		next = &mail.Recorder{}
	}
	q := mail.NewQueue(next, rt.Logger, 1000, 2)
	rt.OnShutdown(func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return q.Close(ctx)
	})
	return q, nil
}

// oauthProviders enables each provider whose client id is configured.
func oauthProviders(ctx context.Context, rt *app.Runtime) []auth.Provider {
	c := rt.Config
	var ps []auth.Provider
	if c.GoogleClientID != "" {
		ps = append(ps, auth.NewGoogle(ctx, auth.ClientCredentials{ID: c.GoogleClientID, Secret: c.GoogleClientSecret}, auth.GoogleEndpoints))
	}
	if c.MicrosoftClientID != "" {
		ps = append(ps, auth.NewMicrosoft(ctx, auth.ClientCredentials{ID: c.MicrosoftClientID, Secret: c.MicrosoftClientSecret}, auth.MicrosoftEndpoints))
	}
	if c.GitHubClientID != "" {
		ps = append(ps, auth.NewGitHub(auth.ClientCredentials{ID: c.GitHubClientID, Secret: c.GitHubClientSecret}, auth.GitHubDefaultEndpoints))
	}
	names := make([]string, len(ps))
	for i, p := range ps {
		names[i] = string(p.Name())
	}
	rt.Logger.Info("oauth providers", slog.Any("enabled", names))
	return ps
}
