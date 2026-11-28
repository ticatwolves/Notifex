// Package vendor holds VendorClient implementations. MockVendorClient is
// the dev and test sender: it records messages and returns scripted
// outcomes, and never calls the network.
package vendor

import (
	"context"
	"fmt"
	"maps"
	"sync"

	"github.com/google/uuid"

	"github.com/ticatwolves/notifex/internal/channel"
)

// Sent is one recorded send.
type Sent struct {
	Message    channel.Message
	Credential channel.Credential
}

// MockVendorClient records sends. Outcome, if set, decides each send's
// result; otherwise every send succeeds (Delivered for slack and webhook).
type MockVendorClient struct {
	mu      sync.Mutex
	sent    []Sent
	Outcome func(m channel.Message, attempt int) (channel.Result, error)
	attempt map[uuid.UUID]int
	// Block, if set, is received from before returning (crash/slow tests).
	Block chan struct{}
}

// NewMock returns a mock that accepts everything.
func NewMock() *MockVendorClient { return &MockVendorClient{attempt: map[uuid.UUID]int{}} }

// Send implements channel.Sender.
func (m *MockVendorClient) Send(ctx context.Context, msg channel.Message, c channel.Credential) (channel.Result, error) {
	if m.Block != nil {
		select {
		case <-m.Block:
		case <-ctx.Done():
			return channel.Result{}, ctx.Err()
		}
	}
	m.mu.Lock()
	m.attempt[msg.NotificationID]++
	n := m.attempt[msg.NotificationID]
	// Copy the secret: the dispatcher wipes its map after the call.
	c.Secret = maps.Clone(c.Secret)
	m.sent = append(m.sent, Sent{Message: msg, Credential: c})
	out := m.Outcome
	m.mu.Unlock()
	if out != nil {
		return out(msg, n)
	}
	return channel.Result{VendorMessageID: fmt.Sprintf("mock-%s-%d", msg.NotificationID, n),
		Delivered: msg.Channel == "slack" || msg.Channel == "webhook"}, nil
}

// Sent returns every recorded send.
func (m *MockVendorClient) Sent() []Sent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Sent(nil), m.sent...)
}

// SendsFor counts the sends of one notification.
func (m *MockVendorClient) SendsFor(id uuid.UUID) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.attempt[id]
}

// RegisterAll registers the mock for every Phase 2/3 (channel, provider).
func (m *MockVendorClient) RegisterAll(r *channel.Registry) {
	for _, cp := range [][2]string{
		{"email", "platform"}, {"email", "sendgrid"}, {"email", "smtp"}, {"email", "ses"},
		{"sms", "platform"}, {"sms", "twilio"},
		{"slack", "incoming_webhook"}, {"slack", "bot_token"},
		{"webhook", "signing"},
	} {
		r.Register(cp[0], cp[1], m)
	}
}
