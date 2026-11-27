// Command scheduler is the leader-elected background service: outbox relay,
// scheduled_dispatch mover, payload purge, partition management and rollups.
// Jobs arrive in later phases; for now it runs only the ops port.
package main

import (
	"context"
	"os"

	"github.com/ticatwolves/notifex/internal/app"
	"github.com/ticatwolves/notifex/internal/lifecycle"
)

func main() { os.Exit(app.Main("scheduler", build)) }

func build(_ context.Context, rt *app.Runtime) ([]lifecycle.Component, error) {
	rt.Logger.Warn("no scheduler jobs registered yet (Phases 3 and 5)")
	return nil, nil
}
