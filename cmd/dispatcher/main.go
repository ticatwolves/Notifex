// Command dispatcher consumes the per-channel SQS dispatch queues and sends
// notifications through the channel registry. Consumers arrive in Phase 3;
// for now it runs only the ops port so the deploy shape is real.
package main

import (
	"context"
	"os"

	"github.com/sbshobhit/notifex/internal/app"
	"github.com/sbshobhit/notifex/internal/lifecycle"
)

func main() { os.Exit(app.Main("dispatcher", build)) }

func build(_ context.Context, rt *app.Runtime) ([]lifecycle.Component, error) {
	rt.Logger.Warn("no channel consumers registered yet (Phase 3)")
	return nil, nil
}
