// Package channel is the send contract between the dispatcher and vendors
// (docs/02 §Channel abstraction): a rendered message, the resolved
// credential, and a Sender per (channel, provider). Real vendor clients live
// in channel/vendor; MockVendorClient serves dev and tests (CLAUDE.md:
// never call a real vendor in tests).
package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/ticatwolves/notifex/internal/workflow/render"
)

// Message is a rendered notification ready to send. It holds PII and
// content in memory only; never log it.
type Message struct {
	NotificationID uuid.UUID
	TenantID       uuid.UUID
	Channel        string
	// To is the channel address (email, E.164 phone, Slack user); empty for
	// channels that post to a configured destination.
	To      string
	Email   *render.EmailOutput
	SMS     *render.SMSOutput
	Slack   *render.SlackOutput
	Webhook json.RawMessage
	// FromName and ReplyTo come from the effective brand (email).
	FromName string
	ReplyTo  string
}

// IdempotencyKey is passed to vendors that support one (ADR-004 §duplicates).
func (m Message) IdempotencyKey() string { return m.NotificationID.String() }

// Credential is the resolved credential for one send. Secret is decrypted
// just for this send and must not be logged or retained.
type Credential struct {
	ID       *uuid.UUID
	Mode     string // platform, byo
	Provider string
	Config   map[string]any
	Secret   map[string]any
}

// Result is a vendor's acceptance.
type Result struct {
	VendorMessageID string
	// Delivered is true for synchronous channels where acceptance is
	// delivery (Slack, webhook 2xx; ADR-007).
	Delivered bool
}

// SendError is a vendor failure, classified (ADR-014 §5).
type SendError struct {
	Code       string
	Message    string // redacted: never content or addresses
	Retryable  bool
	RetryAfter time.Duration // vendor-suggested minimum delay, if any
}

func (e *SendError) Error() string { return "channel: " + e.Code + ": " + e.Message }

// Retryable builds a retryable SendError.
func Retryable(code, msg string) *SendError {
	return &SendError{Code: code, Message: msg, Retryable: true}
}

// Permanent builds a permanent SendError.
func Permanent(code, msg string) *SendError { return &SendError{Code: code, Message: msg} }

// AsSendError classifies any error: SendErrors as they are, context
// deadlines as retryable timeouts, anything else as retryable unknowns.
func AsSendError(err error) *SendError {
	var se *SendError
	if errors.As(err, &se) {
		return se
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return Retryable("vendor_timeout", "the vendor did not answer in time")
	}
	return Retryable("vendor_error", "unexpected error calling the vendor")
}

// Sender sends one message with one credential (a VendorClient).
type Sender interface {
	Send(ctx context.Context, m Message, c Credential) (Result, error)
}

// SenderFunc adapts a function to Sender.
type SenderFunc func(ctx context.Context, m Message, c Credential) (Result, error)

// Send implements Sender.
func (f SenderFunc) Send(ctx context.Context, m Message, c Credential) (Result, error) {
	return f(ctx, m, c)
}

// Registry maps (channel, provider) to a Sender.
type Registry struct{ senders map[string]Sender }

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{senders: map[string]Sender{}} }

// Register adds a sender.
func (r *Registry) Register(channel, provider string, s Sender) { r.senders[channel+"/"+provider] = s }

// Lookup returns the sender for (channel, provider).
func (r *Registry) Lookup(channel, provider string) (Sender, error) {
	s, ok := r.senders[channel+"/"+provider]
	if !ok {
		return nil, Permanent("provider_unavailable", fmt.Sprintf("no sender for %s/%s", channel, provider))
	}
	return s, nil
}
