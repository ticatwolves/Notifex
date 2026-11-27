package lifecycle

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ticatwolves/notifex/internal/health"
)

type fakeComp struct {
	name     string
	startErr error         // returned immediately when set
	exitNil  bool          // return nil immediately (unexpected exit)
	stopWait time.Duration // simulate slow drain
	stopped  atomic.Bool
}

func (f *fakeComp) Name() string { return f.name }

func (f *fakeComp) Start(ctx context.Context) error {
	if f.startErr != nil {
		return f.startErr
	}
	if f.exitNil {
		return nil
	}
	<-ctx.Done()
	return nil
}

func (f *fakeComp) Stop(ctx context.Context) error {
	defer f.stopped.Store(true)
	select {
	case <-time.After(f.stopWait):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestRun(t *testing.T) {
	tests := []struct {
		name    string
		comps   func() []*fakeComp
		signal  bool
		timeout time.Duration
		wantErr string
	}{
		{
			name:    "signal stops all cleanly",
			comps:   func() []*fakeComp { return []*fakeComp{{name: "a"}, {name: "b"}} },
			signal:  true,
			timeout: time.Second,
		},
		{
			name:    "component failure stops the others",
			comps:   func() []*fakeComp { return []*fakeComp{{name: "a", startErr: errors.New("bind failed")}, {name: "b"}} },
			timeout: time.Second,
			wantErr: "a: bind failed",
		},
		{
			name:    "unexpected nil exit is a failure",
			comps:   func() []*fakeComp { return []*fakeComp{{name: "a", exitNil: true}, {name: "b"}} },
			timeout: time.Second,
			wantErr: "a: exited unexpectedly",
		},
		{
			name:    "slow drain hits timeout",
			comps:   func() []*fakeComp { return []*fakeComp{{name: "slow", stopWait: time.Second}} },
			signal:  true,
			timeout: 20 * time.Millisecond,
			wantErr: "stop slow",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			comps := tt.comps()
			lc := make([]Component, len(comps))
			for i, c := range comps {
				lc[i] = c
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			h := health.NewRegistry(time.Second)
			if tt.signal {
				time.AfterFunc(20*time.Millisecond, cancel)
			}

			err := Run(ctx, discard, h, tt.timeout, lc...)
			if tt.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			}
			for _, c := range comps {
				assert.True(t, c.stopped.Load(), "component %s was not stopped", c.name)
			}
			ok, _ := h.Check(context.Background())
			assert.False(t, ok, "readiness must report draining after shutdown")
		})
	}
}

func TestHTTPServerGracefulShutdown(t *testing.T) {
	release := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusNoContent)
	})
	srv := NewHTTPServer("test", "127.0.0.1:0", handler, discard)
	require.NoError(t, srv.Listen(context.Background()))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, discard, health.NewRegistry(time.Second), 2*time.Second, srv) }()

	respCh := make(chan int, 1)
	go func() {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+srv.Addr().String()+"/", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			respCh <- -1
			return
		}
		_ = resp.Body.Close()
		respCh <- resp.StatusCode
	}()

	time.Sleep(50 * time.Millisecond) // request is in flight
	cancel()                          // SIGTERM
	time.Sleep(50 * time.Millisecond)
	close(release) // in-flight request finishes during drain

	assert.Equal(t, http.StatusNoContent, <-respCh, "in-flight request must complete during shutdown")
	require.NoError(t, <-done)
}
