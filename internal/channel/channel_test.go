package channel_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ticatwolves/notifex/internal/channel"
)

func TestRegistry(t *testing.T) {
	t.Parallel()
	r := channel.NewRegistry()
	r.Register("sms", "twilio", channel.SenderFunc(func(context.Context, channel.Message, channel.Credential) (channel.Result, error) {
		return channel.Result{VendorMessageID: "x"}, nil
	}))
	s, err := r.Lookup("sms", "twilio")
	require.NoError(t, err)
	res, err := s.Send(context.Background(), channel.Message{}, channel.Credential{})
	require.NoError(t, err)
	assert.Equal(t, "x", res.VendorMessageID)

	_, err = r.Lookup("sms", "nope")
	se := channel.AsSendError(err)
	assert.Equal(t, "provider_unavailable", se.Code)
	assert.False(t, se.Retryable, "a missing adapter won't appear on retry")
}

func TestAsSendError(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		err       error
		code      string
		retryable bool
	}{
		"send error kept":      {channel.Permanent("invalid_recipient", "bad"), "invalid_recipient", false},
		"wrapped send error":   {fmt.Errorf("twilio: %w", channel.Retryable("throttled", "429")), "throttled", true},
		"deadline is timeout":  {fmt.Errorf("post: %w", context.DeadlineExceeded), "vendor_timeout", true},
		"unknown is retryable": {errors.New("connection reset"), "vendor_error", true},
	} {
		se := channel.AsSendError(c.err)
		assert.Equal(t, c.code, se.Code, name)
		assert.Equal(t, c.retryable, se.Retryable, name)
	}
	// Unknown errors never leak their text (it may hold vendor URLs or PII).
	assert.NotContains(t, channel.AsSendError(errors.New("https://hooks.example/secret")).Message, "secret")
}
