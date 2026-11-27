package grpctransport

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

func TestHealthAndGracefulStop(t *testing.T) {
	srv := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)))
	comp := NewComponent(srv, "127.0.0.1:0")
	require.NoError(t, comp.Listen(context.Background()))

	errCh := make(chan error, 1)
	go func() { errCh <- comp.Start(context.Background()) }()

	conn, err := grpc.NewClient(comp.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	client := healthpb.NewHealthClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.Eventually(t, func() bool {
		resp, err := client.Check(ctx, &healthpb.HealthCheckRequest{})
		return err == nil && resp.GetStatus() == healthpb.HealthCheckResponse_SERVING
	}, time.Second, 10*time.Millisecond)

	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	require.NoError(t, comp.Stop(stopCtx))
	require.NoError(t, <-errCh, "Serve must return cleanly after GracefulStop")
}

func TestUnaryRecover(t *testing.T) {
	ic := unaryRecover(slog.New(slog.NewTextHandler(io.Discard, nil)))
	_, err := ic(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/x/Y"},
		func(context.Context, any) (any, error) { panic("kaboom") })
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Internal, st.Code())
	assert.NotContains(t, st.Message(), "kaboom")
}
