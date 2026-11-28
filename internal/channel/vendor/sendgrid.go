package vendor

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/ticatwolves/notifex/internal/channel"
	"github.com/ticatwolves/notifex/internal/netguard"
)

// SendGrid sends email through the v3 mail/send API. Acceptance (202) is
// `sent`; delivery comes from event webhooks (Phase 4).
type SendGrid struct {
	Client *http.Client
	Base   string // https://api.sendgrid.com (tests replace it)
}

// NewSendGrid returns a SendGrid sender over a guarded client.
func NewSendGrid(g *netguard.Guard) *SendGrid {
	return &SendGrid{Client: g.HTTPClient(20*time.Second, nil), Base: "https://api.sendgrid.com"}
}

type sgAddr struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
}

type sgContent struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// fromName prefers the credential's from_name, then the brand's.
func fromName(m channel.Message, c channel.Credential) string {
	if n := cfgStr(c.Config, "from_name"); n != "" {
		return n
	}
	return m.FromName
}

// Send implements channel.Sender.
func (s *SendGrid) Send(ctx context.Context, m channel.Message, c channel.Credential) (channel.Result, error) {
	if m.Email == nil {
		return channel.Result{}, channel.Permanent("render_failed", "no email content")
	}
	if m.To == "" {
		return channel.Result{}, channel.Permanent("recipient_missing", "no email address")
	}
	var content []sgContent
	if m.Email.Text != "" {
		content = append(content, sgContent{"text/plain", m.Email.Text})
	}
	if m.Email.HTML != "" {
		content = append(content, sgContent{"text/html", m.Email.HTML})
	}
	if len(content) == 0 {
		return channel.Result{}, channel.Permanent("render_failed", "the email has no body")
	}
	p := map[string]any{
		"personalizations": []map[string]any{{
			"to":          []sgAddr{{Email: m.To}},
			"custom_args": map[string]string{"notification_id": m.NotificationID.String()},
		}},
		"from":    sgAddr{Email: cfgStr(c.Config, "from_email"), Name: fromName(m, c)},
		"subject": m.Email.Subject,
		"content": content,
	}
	if m.ReplyTo != "" {
		p["reply_to"] = sgAddr{Email: m.ReplyTo}
	}
	body, err := json.Marshal(p)
	if err != nil {
		return channel.Result{}, channel.Permanent("render_failed", "the email is not encodable")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.Base+"/v3/mail/send", bytes.NewReader(body))
	if err != nil {
		return channel.Result{}, channel.Permanent("credential_invalid", "bad SendGrid request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfgStr(c.Secret, "api_key"))
	resp, err := s.Client.Do(req)
	if err != nil {
		return channel.Result{}, netError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	netguard.Drain(resp.Body, respLimit)
	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusOK {
		return channel.Result{VendorMessageID: short(resp.Header.Get("X-Message-Id"), 128)}, nil
	}
	return channel.Result{}, httpStatus(resp.StatusCode, resp.Header, "email_rejected")
}
