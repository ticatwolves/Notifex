// Command api serves the public REST and gRPC APIs (trigger intake and the
// control plane) plus the internal ops port.
package main

import (
	"context"
	"errors"
	"os"

	"github.com/ticatwolves/notifex/internal/app"
	"github.com/ticatwolves/notifex/internal/auth"
	"github.com/ticatwolves/notifex/internal/db"
	"github.com/ticatwolves/notifex/internal/lifecycle"
	"github.com/ticatwolves/notifex/internal/store/entstore"
	grpctransport "github.com/ticatwolves/notifex/internal/transport/grpc"
	httptransport "github.com/ticatwolves/notifex/internal/transport/http"
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

	rest := lifecycle.NewHTTPServer("http", rt.Config.HTTPAddr, httptransport.NewRouter(httptransport.Deps{
		Service: rt.Config.Service,
		Logger:  rt.Logger,
		Metrics: rt.Metrics,
		Health:  rt.Health,
		APIKeys: auth.NewAPIKeyAuthenticator(st, hasher, rt.Logger, rt.Metrics),
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
