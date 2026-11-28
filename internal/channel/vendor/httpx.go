package vendor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/ticatwolves/notifex/internal/channel"
	"github.com/ticatwolves/notifex/internal/netguard"
)

// respLimit bounds how much of a vendor response is read.
const respLimit = 64 << 10

// maxRetryAfter caps a vendor's Retry-After (the backoff cap, ADR-014 §5).
const maxRetryAfter = 6 * time.Hour

// netError classifies a transport error. Error text is never kept: it can
// carry the request URL, and a Slack webhook URL is itself a secret.
func netError(err error) *channel.SendError {
	var ne net.Error
	switch {
	case errors.Is(err, netguard.ErrBlocked):
		return channel.Permanent("endpoint_blocked", "the destination resolved to a non-public address")
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return channel.Retryable("vendor_timeout", "the vendor did not answer in time")
	case errors.Is(err, context.Canceled):
		return channel.Retryable("canceled", "the send was interrupted")
	default:
		return channel.Retryable("vendor_unreachable", "could not reach the vendor")
	}
}

// httpStatus classifies a non-success HTTP status: 408, 425, 429 and 5xx
// are retryable (honoring Retry-After); 401 and 403 are auth failures;
// other 4xx are permanent with code rejected.
func httpStatus(status int, h http.Header, rejected string) *channel.SendError {
	switch {
	case status == http.StatusTooManyRequests:
		return &channel.SendError{Code: "rate_limited", Message: "the vendor is rate limiting", Retryable: true, RetryAfter: retryAfter(h)}
	case status == http.StatusRequestTimeout || status == http.StatusTooEarly || status >= 500:
		return &channel.SendError{Code: "vendor_unavailable", Message: fmt.Sprintf("the vendor answered HTTP %d", status), Retryable: true, RetryAfter: retryAfter(h)}
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return channel.Permanent("auth_failed", fmt.Sprintf("the vendor refused the credential (HTTP %d)", status))
	case status >= 300 && status < 400:
		return channel.Permanent("endpoint_redirected", fmt.Sprintf("the endpoint redirected (HTTP %d); redirects are not followed", status))
	default:
		return channel.Permanent(rejected, fmt.Sprintf("the vendor rejected the message (HTTP %d)", status))
	}
}

// retryAfter parses Retry-After (seconds or an HTTP date), capped.
func retryAfter(h http.Header) time.Duration {
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	var d time.Duration
	if n, err := strconv.Atoi(v); err == nil {
		d = time.Duration(n) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = time.Until(t)
	}
	return min(max(d, 0), maxRetryAfter)
}

func cfgStr(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func short(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
