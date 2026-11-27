package mail

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHeaderInjectionRejected(t *testing.T) {
	r := &Recorder{}
	ctx := context.Background()
	for name, m := range map[string]Message{
		"CRLF in subject": {To: "a@example.com", Subject: "Hi\r\nBcc: victim@example.com", Text: "x"},
		"LF in to":        {To: "a@example.com\nBcc: v@example.com", Subject: "Hi", Text: "x"},
		"bad address":     {To: "not an address", Subject: "Hi", Text: "x"},
	} {
		t.Run(name, func(t *testing.T) {
			require.ErrorIs(t, r.Send(ctx, m), ErrInvalidMessage)
		})
	}
	assert.Empty(t, r.Sent())
	require.ErrorIs(t, SMTP{Addr: "127.0.0.1:1", From: "x@example.com"}.Send(ctx, Message{To: "a@example.com", Subject: "a\rb"}), ErrInvalidMessage)
}

func TestQueueDeliversAndDrains(t *testing.T) {
	r := &Recorder{}
	q := NewQueue(r, slog.New(slog.NewTextHandler(io.Discard, nil)), 10, 2)
	for range 5 {
		require.NoError(t, q.Send(context.Background(), Message{To: "a@example.com", Subject: "s", Text: "t"}))
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, q.Close(ctx))
	assert.Len(t, r.Sent(), 5, "Close waits for queued mail")
}

type blocking struct{ release chan struct{} }

func (b blocking) Send(context.Context, Message) error { <-b.release; return nil }

func TestQueueFullDropsInsteadOfBlocking(t *testing.T) {
	b := blocking{release: make(chan struct{})}
	q := NewQueue(b, slog.New(slog.NewTextHandler(io.Discard, nil)), 1, 1)
	m := Message{To: "a@example.com", Subject: "s", Text: "t"}
	require.NoError(t, q.Send(context.Background(), m)) // taken by the worker
	time.Sleep(20 * time.Millisecond)
	require.NoError(t, q.Send(context.Background(), m)) // fills the buffer
	start := time.Now()
	require.Error(t, q.Send(context.Background(), m), "full queue reports an error")
	assert.Less(t, time.Since(start), 50*time.Millisecond, "never blocks the request")
	close(b.release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, q.Close(ctx))
}
