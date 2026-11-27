// Command migrate applies the versioned SQL migrations in migrations/. It runs
// as a one-off ECS task before each deploy. Implemented in Phase 0 slice 3.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "notifex-migrate: not implemented yet (Phase 0 slice 3)")
	os.Exit(1)
}
