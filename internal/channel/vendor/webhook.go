package vendor

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ticatwolves/notifex/internal/channel"
	"github.com/ticatwolves/notifex/internal/credential"
	"github.com/ticatwolves/notifex/internal/netguard"
)

// Webhook posts the rendered JSON body to the credential's URL, signed in
// the Standard Webhooks style (ADR-014 slice 6 amendment):
//
//	webhook-id:        <notification id>   (also Idempotency-Key)
//	webhook-timestamp: <unix seconds>
//	webhook-signature: v1,<base64 HMAC-SHA256(key, id + "." + timestamp + "." + body)>
//
// key is the 32 bytes after the secret's "whsec_" prefix, base64url-decoded.
// A 2xx is delivery; redirects are never followed.
type Webhook struct {
	Client *http.Client
	Now    func() time.Time
	// AllowHTTP permits http:// URLs (tests only).
	AllowHTTP bool
}

// NewWebhook returns a webhook sender over a guarded client.
func NewWebhook(g *netguard.Guard) *Webhook {
	return &Webhook{Client: g.HTTPClient(15*time.Second, nil), Now: time.Now}
}

// SigningKey decodes a whsec_ secret.
func SigningKey(secret string) ([]byte, bool) {
	raw, ok := strings.CutPrefix(secret, "whsec_")
	if !ok {
		return nil, false
	}
	k, err := base64.RawURLEncoding.DecodeString(raw)
	return k, err == nil && len(k) >= 16
}

// Sign returns the webhook-signature header value.
func Sign(key []byte, id, ts string, body []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + ts + "."))
	mac.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// Send implements channel.Sender.
func (w *Webhook) Send(ctx context.Context, m channel.Message, c channel.Credential) (channel.Result, error) {
	u := cfgStr(c.Config, "url")
	if err := credential.WebhookURL(u); err != nil && (!w.AllowHTTP || !strings.HasPrefix(u, "http://")) {
		return channel.Result{}, channel.Permanent("credential_invalid", "the webhook credential has no valid url; update it in the console")
	}
	key, ok := SigningKey(cfgStr(c.Secret, "secret"))
	if !ok {
		return channel.Result{}, channel.Permanent("credential_invalid", "the webhook signing secret is unreadable; rotate it")
	}
	body := []byte(m.Webhook)
	if len(body) == 0 {
		body = []byte("{}")
	}
	id := m.NotificationID.String()
	ts := strconv.FormatInt(w.Now().Unix(), 10)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return channel.Result{}, channel.Permanent("credential_invalid", "the webhook url is not a valid request target")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Notifex-Webhook/1")
	req.Header.Set("webhook-id", id)
	req.Header.Set("webhook-timestamp", ts)
	req.Header.Set("webhook-signature", Sign(key, id, ts, body))
	req.Header.Set("Idempotency-Key", id)
	resp, err := w.Client.Do(req)
	if err != nil {
		return channel.Result{}, netError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	netguard.Drain(resp.Body, respLimit)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return channel.Result{VendorMessageID: short(resp.Header.Get("X-Request-Id"), 128), Delivered: true}, nil
	}
	return channel.Result{}, httpStatus(resp.StatusCode, resp.Header, "endpoint_rejected")
}
