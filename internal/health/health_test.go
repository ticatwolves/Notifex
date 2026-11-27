package health

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestRegistryCheck(t *testing.T) {
	tests := []struct {
		name    string
		checks  map[string]CheckFunc
		drain   bool
		wantOK  bool
		wantBad []string
	}{
		{name: "no checks", wantOK: true},
		{name: "all pass", checks: map[string]CheckFunc{
			"db":  func(context.Context) error { return nil },
			"sqs": func(context.Context) error { return nil },
		}, wantOK: true},
		{name: "one fails", checks: map[string]CheckFunc{
			"db":  func(context.Context) error { return errors.New("down") },
			"sqs": func(context.Context) error { return nil },
		}, wantOK: false, wantBad: []string{"db"}},
		{name: "timeout counts as failure", checks: map[string]CheckFunc{
			"slow": func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
		}, wantOK: false, wantBad: []string{"slow"}},
		{name: "draining fails even when checks pass", checks: map[string]CheckFunc{
			"db": func(context.Context) error { return nil },
		}, drain: true, wantOK: false, wantBad: []string{"lifecycle"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewRegistry(20 * time.Millisecond)
			for n, fn := range tt.checks {
				r.Register(n, fn)
			}
			if tt.drain {
				r.SetDraining()
			}
			ok, results := r.Check(context.Background())
			assert.Equal(t, tt.wantOK, ok)
			var bad []string
			for _, res := range results {
				if !res.OK {
					bad = append(bad, res.Name)
				}
			}
			assert.Equal(t, tt.wantBad, bad)
		})
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	r := NewRegistry(time.Second)
	r.Register("db", func(context.Context) error { return nil })
	assert.Panics(t, func() { r.Register("db", func(context.Context) error { return nil }) })
}
