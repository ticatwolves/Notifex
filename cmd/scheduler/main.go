// Command scheduler is the leader-elected background service (ADR-014 §7):
// outbox relay, scheduled_dispatch mover, partition maintenance and cleanup.
// Run two or more tasks for availability; one holds the lease at a time.
// Payload purge and rollups arrive in Phase 5.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/ticatwolves/notifex/internal/app"
	"github.com/ticatwolves/notifex/internal/db"
	"github.com/ticatwolves/notifex/internal/dispatch/sqsqueue"
	"github.com/ticatwolves/notifex/internal/lifecycle"
	"github.com/ticatwolves/notifex/internal/scheduler"
	"github.com/ticatwolves/notifex/internal/store/entstore"
)

func main() { os.Exit(app.Main("scheduler", build)) }

func build(ctx context.Context, rt *app.Runtime) ([]lifecycle.Component, error) {
	if rt.Config.DatabaseURL == "" {
		return nil, errors.New("DATABASE_URL is required")
	}
	sqlDB, err := db.Open(ctx, rt.Config.DatabaseURL, db.PoolConfig{})
	if err != nil {
		return nil, err
	}
	rt.OnShutdown(sqlDB.Close)
	rt.Health.Register("postgres", sqlDB.PingContext)

	client, err := sqsqueue.NewClient(ctx, rt.Config.AWSRegion, rt.Config.SQSEndpoint)
	if err != nil {
		return nil, err
	}
	q := sqsqueue.New(client, sqsqueue.Options{
		Prefix: rt.Config.QueuePrefix, FairQueues: rt.Config.SQSFairQueues, Log: rt.Logger, Metrics: rt.Metrics,
	})
	host, _ := os.Hostname()
	holder := fmt.Sprintf("%s/%d", host, os.Getpid())
	return []lifecycle.Component{scheduler.New(entstore.New(sqlDB), sqlDB, q, holder, rt.Logger, rt.Metrics)}, nil
}
