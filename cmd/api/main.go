// Command api serves the public REST and gRPC APIs (trigger intake and the
// control plane) plus the internal ops port.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/ticatwolves/notifex/internal/app"
	"github.com/ticatwolves/notifex/internal/auth"
	"github.com/ticatwolves/notifex/internal/credential"
	"github.com/ticatwolves/notifex/internal/crypto"
	"github.com/ticatwolves/notifex/internal/db"
	"github.com/ticatwolves/notifex/internal/lifecycle"
	"github.com/ticatwolves/notifex/internal/mail"
	"github.com/ticatwolves/notifex/internal/operator"
	"github.com/ticatwolves/notifex/internal/store"
	"github.com/ticatwolves/notifex/internal/store/entstore"
	"github.com/ticatwolves/notifex/internal/tenant"
	grpctransport "github.com/ticatwolves/notifex/internal/transport/grpc"
	httptransport "github.com/ticatwolves/notifex/internal/transport/http"
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
	keys, err := keyProvider(rt)
	if err != nil {
		return nil, err
	}
	tenants := tenant.NewService(st, keys, mailer, rt.Logger, rt.Config.ConsoleURL, tenant.DefaultPolicy)
	tenants.KeyHasher = hasher
	// One field cipher per process: it caches unwrapped tenant DEKs, and a
	// crypto-shred drops the tenant's key from it at once.
	cipher := crypto.NewCipher(keys, st.Tenants(), crypto.CipherOptions{Metrics: rt.Metrics})
	tenants.OnShred = func(id store.TenantID) { cipher.Forget(id.UUID()) }

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
		Credentials:    credential.NewService(st, cipher),
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

// keyProvider wraps tenant DEKs: the local master key in local runs; KMS
// (Phase 8) everywhere else. Until KMS exists, deployed environments get a
// provider that refuses to create DEKs, so tenant creation fails there while
// the rest of the API keeps working. Never fall back to the local key.
func keyProvider(rt *app.Runtime) (crypto.KeyProvider, error) {
	if rt.Config.Deployed() {
		rt.Logger.Warn("no KMS KeyProvider yet (Phase 8): tenant creation is disabled in this environment")
		return noKMS{}, nil
	}
	kp, err := crypto.NewLocalFromBase64(rt.Config.LocalMasterKey)
	if err != nil {
		return nil, fmt.Errorf("NOTIFEX_LOCAL_MASTER_KEY: %w", err)
	}
	return kp, nil
}

// noKMS refuses every key operation (deployed environments before Phase 8).
type noKMS struct{}

var errNoKMS = errors.New("tenant encryption keys are unavailable: KMS KeyProvider not configured (Phase 8)")

func (noKMS) GenerateDataKey(context.Context, crypto.EncryptionContext) (crypto.DataKey, error) {
	return crypto.DataKey{}, errNoKMS
}

func (noKMS) Unwrap(context.Context, []byte, string, crypto.EncryptionContext) ([]byte, error) {
	return nil, errNoKMS
}
