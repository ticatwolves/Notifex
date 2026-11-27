// Package health implements liveness and readiness checks.
//
// Liveness (/healthz) only says the process is running; it never checks
// dependencies, so a Postgres blip doesn't make ECS kill every task at once.
// Readiness (/readyz) runs the registered checks and fails while the service
// cannot do useful work or is draining for shutdown.
package health

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// CheckFunc reports whether a dependency is usable. It must honour ctx.
type CheckFunc func(ctx context.Context) error

// ErrDraining is reported by readiness once shutdown has begun.
var ErrDraining = errors.New("draining")

// Registry holds named readiness checks.
type Registry struct {
	timeout  time.Duration
	draining atomic.Bool

	mu     sync.RWMutex
	checks map[string]CheckFunc
}

// NewRegistry returns a Registry whose checks each get the given timeout.
func NewRegistry(timeout time.Duration) *Registry {
	return &Registry{timeout: timeout, checks: map[string]CheckFunc{}}
}

// Register adds a named check. Registering the same name twice panics: it is
// a wiring bug.
func (r *Registry) Register(name string, fn CheckFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.checks[name]; dup {
		panic(fmt.Sprintf("health: duplicate check %q", name))
	}
	r.checks[name] = fn
}

// SetDraining flips readiness to failing so load balancers stop routing new
// work before servers stop.
func (r *Registry) SetDraining() { r.draining.Store(true) }

// Result is the outcome of one check.
type Result struct {
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// Check runs all checks concurrently and reports whether all passed.
func (r *Registry) Check(ctx context.Context) (bool, []Result) {
	if r.draining.Load() {
		return false, []Result{{Name: "lifecycle", OK: false, Error: ErrDraining.Error()}}
	}
	r.mu.RLock()
	names := make([]string, 0, len(r.checks))
	for n := range r.checks {
		names = append(names, n)
	}
	checks := make([]CheckFunc, len(names))
	sort.Strings(names)
	for i, n := range names {
		checks[i] = r.checks[n]
	}
	r.mu.RUnlock()

	results := make([]Result, len(names))
	var wg sync.WaitGroup
	for i := range names {
		wg.Go(func() {
			cctx, cancel := context.WithTimeout(ctx, r.timeout)
			defer cancel()
			res := Result{Name: names[i], OK: true}
			if err := checks[i](cctx); err != nil {
				res.OK, res.Error = false, err.Error()
			}
			results[i] = res
		})
	}
	wg.Wait()

	ok := true
	for _, res := range results {
		ok = ok && res.OK
	}
	return ok, results
}
