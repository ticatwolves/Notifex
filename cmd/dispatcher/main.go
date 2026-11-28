// Command dispatcher consumes the per-channel SQS dispatch queues and sends
// notifications through the channel registry (ADR-004, ADR-014 §5).
// Consumers run only with DISPATCH_ENABLED=true. DISPATCH_VENDORS picks the
// senders: live vendors (the default when deployed) or the mock vendor
// (the local default, refused outside local by config validation).
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"

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
	if rt.Config.DispatchVendors == "mock" {
		vendor.NewMock().RegisterAll(reg)
		rt.Logger.Warn("DISPATCH_VENDORS=mock: sends are recorded in memory, nothing leaves this process")
	} else {
		awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(rt.Config.AWSRegion))
		if err != nil {
			return nil, fmt.Errorf("aws config: %w", err)
		}
		vendor.RegisterLive(reg, vendor.LiveConfig{
			AWS: awsCfg, PlatformFrom: rt.Config.PlatformEmailFrom,
			ConfigurationSet: rt.Config.SESConfigurationSet, EHLOName: rt.Config.SMTPEHLOName,
		})
		if rt.Config.PlatformEmailFrom == "" {
			rt.Logger.Warn("PLATFORM_EMAIL_FROM not set: platform email sends will retry until it is configured")
		}
	}

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
