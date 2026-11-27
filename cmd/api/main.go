// Command api serves the public REST and gRPC APIs (trigger intake and the
// control plane) plus the internal ops port.
package main

import (
	"context"
	"os"

	"github.com/sbshobhit/notifex/internal/app"
	"github.com/sbshobhit/notifex/internal/lifecycle"
	grpctransport "github.com/sbshobhit/notifex/internal/transport/grpc"
	httptransport "github.com/sbshobhit/notifex/internal/transport/http"
)

func main() { os.Exit(app.Main("api", build)) }

func build(ctx context.Context, rt *app.Runtime) ([]lifecycle.Component, error) {
	rest := lifecycle.NewHTTPServer("http", rt.Config.HTTPAddr, httptransport.NewRouter(httptransport.Deps{
		Service: rt.Config.Service,
		Logger:  rt.Logger,
		Metrics: rt.Metrics,
		Health:  rt.Health,
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
