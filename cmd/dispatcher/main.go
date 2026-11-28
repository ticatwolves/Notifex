// Command dispatcher consumes the per-channel SQS dispatch queues and sends
// notifications through the channel registry (ADR-004, ADR-014 §5).
// Consumers run only with DISPATCH_ENABLED=true. Until the real channel
// adapters land (Phase 3 slice 6) every send goes to the mock vendor, so
// enabling dispatch outside local is refused.
package main

import (
	"context"
	"errors"
	"os"

	"github.com/ticatwolves/notifex/internal/app"
	"github.com/ticatwolves/notifex/internal/channel"
	"github.com/ticatwolves/notifex/internal/channel/vendor"
	"github.com/ticatwolves/notifex/internal/credential"
	"github.com/ticatwolves/notifex/internal/crypto"
	"github.com/ticatwolves/notifex/internal/db"
	"github.com/ticatwolves/notifex/internal/dispatch"
	"github.com/ticatwolves/notifex/internal/dispatch/sqsqueue"
	"github.com/ticatwolves/notifex/internal/dispatch/worker"
	"github.com/ticatwolves/notifex/internal/lifecycle"
	"github.com/ticatwolves/notifex/internal/store/entstore"
)

func main() { os.Exit(app.Main("dispatcher", build)) }

func build(ctx context.Context, rt *app.Runtime) ([]lifecycle.Component, error) {
	if !rt.Config.DispatchEnabled {
		rt.Logger.Warn("DISPATCH_ENABLED is false: no channel consumers running")
		return nil, nil
	}
	if rt.Config.Deployed() {
		return nil, errors.New("DISPATCH_ENABLED: real channel adapters are not built yet (Phase 3 slice 6); refusing to send through the mock vendor outside local")
	}
	if rt.Config.DatabaseURL == "" {
		return nil, errors.New("DATABASE_URL is required")
	}
	sqlDB, err := db.Open(ctx, rt.Config.DatabaseURL, db.PoolConfig{})
	if err != nil {
		return nil, err
	}
	rt.OnShutdown(sqlDB.Close)
	rt.Health.Register("postgres", sqlDB.PingContext)
	st := entstore.New(sqlDB)

	keys, err := app.KeyProvider(rt)
	if err != nil {
		return nil, err
	}
	cipher := crypto.NewCipher(keys, st.Tenants(), crypto.CipherOptions{Metrics: rt.Metrics})

	reg := channel.NewRegistry()
	vendor.NewMock().RegisterAll(reg)
	rt.Logger.Warn("channel adapters: mock vendor only (Phase 3 slice 6 adds real ones)")

	client, err := sqsqueue.NewClient(ctx, rt.Config.AWSRegion, rt.Config.SQSEndpoint)
	if err != nil {
		return nil, err
	}
	q := sqsqueue.New(client, sqsqueue.Options{
		Prefix: rt.Config.QueuePrefix, FairQueues: rt.Config.SQSFairQueues, Visibility: worker.DefaultConfig.Lease,
		Concurrency: rt.Config.DispatchConcurrency, Log: rt.Logger, Metrics: rt.Metrics,
	})
	w := worker.New(st, cipher, credential.NewService(st, cipher), reg, worker.DefaultConfig, rt.Logger, rt.Metrics)
	return []lifecycle.Component{worker.NewDispatcher(q, w, dispatch.Channels)}, nil
}
