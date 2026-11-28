package vendor_test

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ticatwolves/notifex/internal/channel"
	"github.com/ticatwolves/notifex/internal/channel/vendor"
	"github.com/ticatwolves/notifex/internal/netguard"
	"github.com/ticatwolves/notifex/internal/workflow/render"
)

// Tests serve from loopback, so they use a guard that allows it. The
// production guard is exercised in TestWebhookBlockedAddress.
var loopback = netguard.NewAllowing(func(netip.Addr) bool { return true })

// recorder is a fake vendor: it records requests and answers with respond.
type recorder struct {
	mu      sync.Mutex
	reqs    []*http.Request
	bodies  [][]byte
	respond func(w http.ResponseWriter, r *http.Request)
}

func (rc *recorder) server(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rc.mu.Lock()
		rc.reqs = append(rc.reqs, r)
		rc.bodies = append(rc.bodies, b)
		f := rc.respond
		rc.mu.Unlock()
		f(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func (rc *recorder) last() (*http.Request, []byte) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.reqs[len(rc.reqs)-1], rc.bodies[len(rc.bodies)-1]
}

func status(code int, hdr ...string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		for i := 0; i+1 < len(hdr); i += 2 {
			w.Header().Set(hdr[i], hdr[i+1])
		}
		w.WriteHeader(code)
	}
}

func body(code int, b string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code); _, _ = io.WriteString(w, b) }
}

func whsec(t *testing.T) (string, []byte) {
	t.Helper()
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	return "whsec_" + base64.RawURLEncoding.EncodeToString(k), k
}

func msg(ch string) channel.Message {
	m := channel.Message{NotificationID: uuid.Must(uuid.NewV7()), TenantID: uuid.Must(uuid.NewV7()), Channel: ch}
	switch ch {
	case "email":
		m.To, m.Email = "user@example.com", &render.EmailOutput{Subject: "Order A1", HTML: "<p>Hi</p>", Text: "Hi"}
	case "sms":
		m.To, m.SMS = "+919876543210", &render.SMSOutput{Text: "Order A1"}
	case "slack":
		m.Slack = &render.SlackOutput{Text: "Order A1", Blocks: []any{map[string]any{"type": "divider"}}}
	case "webhook":
		m.Webhook = json.RawMessage(`{"order":"A1"}`)
	}
	return m
}

func classify(t *testing.T, err error) *channel.SendError {
	t.Helper()
	require.Error(t, err)
	var se *channel.SendError
	require.ErrorAs(t, err, &se, "adapters return classified errors")
	return se
}

func TestWebhookSignsAndDelivers(t *testing.T) {
	t.Parallel()
	rc := &recorder{respond: status(204, "X-Request-Id", "req-7")}
	srv := rc.server(t)
	secret, key := whsec(t)
	w := vendor.NewWebhook(loopback)
	w.AllowHTTP = true
	now := time.Unix(1_800_000_000, 0)
	w.Now = func() time.Time { return now }
	m := msg("webhook")
	res, err := w.Send(context.Background(), m, channel.Credential{Mode: "byo", Provider: "signing",
		Config: map[string]any{"url": srv.URL + "/hook"}, Secret: map[string]any{"secret": secret}})
	require.NoError(t, err)
	assert.True(t, res.Delivered)
	assert.Equal(t, "req-7", res.VendorMessageID)

	r, b := rc.last()
	assert.JSONEq(t, `{"order":"A1"}`, string(b))
	id := m.NotificationID.String()
	assert.Equal(t, id, r.Header.Get("webhook-id"))
	assert.Equal(t, id, r.Header.Get("Idempotency-Key"))
	assert.Equal(t, "1800000000", r.Header.Get("webhook-timestamp"))
	// Verify as a receiver would, independently of vendor.Sign.
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + ".1800000000." + string(b)))
	assert.Equal(t, "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)), r.Header.Get("webhook-signature"), "receivers can verify")
	assert.True(t, strings.HasPrefix(r.Header.Get("webhook-signature"), "v1,"))
	assert.NotEqual(t, vendor.Sign(key, id, "1800000001", b), r.Header.Get("webhook-signature"), "timestamp is signed")
	assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
	assert.NotContains(t, r.Header.Get("webhook-signature"), secret)
}

func TestWebhookClassification(t *testing.T) {
	t.Parallel()
	secret, _ := whsec(t)
	for name, c := range map[string]struct {
		respond   func(http.ResponseWriter, *http.Request)
		code      string
		retryable bool
		after     time.Duration
	}{
		"429 with Retry-After": {status(429, "Retry-After", "120"), "rate_limited", true, 2 * time.Minute},
		"503":                  {status(503), "vendor_unavailable", true, 0},
		"408":                  {status(408), "vendor_unavailable", true, 0},
		"410 rejected":         {status(410), "endpoint_rejected", false, 0},
		"401":                  {status(401), "auth_failed", false, 0},
		"redirect not followed": {func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://169.254.169.254/latest", http.StatusFound)
		}, "endpoint_redirected", false, 0},
	} {
		rc := &recorder{respond: c.respond}
		srv := rc.server(t)
		w := vendor.NewWebhook(loopback)
		w.AllowHTTP = true
		_, err := w.Send(context.Background(), msg("webhook"), channel.Credential{Config: map[string]any{"url": srv.URL}, Secret: map[string]any{"secret": secret}})
		se := classify(t, err)
		assert.Equal(t, c.code, se.Code, name)
		assert.Equal(t, c.retryable, se.Retryable, name)
		assert.Equal(t, c.after, se.RetryAfter, name)
		assert.Len(t, rc.reqs, 1, "%s: one request, no redirect follow", name)
	}
}

func TestWebhookCredentialChecks(t *testing.T) {
	t.Parallel()
	secret, _ := whsec(t)
	w := vendor.NewWebhook(netguard.New())
	for name, cred := range map[string]channel.Credential{
		"no url (pre-slice-6 credential)": {Secret: map[string]any{"secret": secret}},
		"private url":                     {Config: map[string]any{"url": "https://10.0.0.8/x"}, Secret: map[string]any{"secret": secret}},
		"bad secret":                      {Config: map[string]any{"url": "https://hooks.acme.test/x"}, Secret: map[string]any{"secret": "nope"}},
	} {
		_, err := w.Send(context.Background(), msg("webhook"), cred)
		se := classify(t, err)
		assert.Equal(t, "credential_invalid", se.Code, name)
		assert.False(t, se.Retryable, name)
	}
}

// TestWebhookBlockedAddress: the production guard refuses a loopback
// target at connect time, without sending anything.
func TestWebhookBlockedAddress(t *testing.T) {
	t.Parallel()
	rc := &recorder{respond: status(200)}
	srv := rc.server(t)
	secret, _ := whsec(t)
	w := vendor.NewWebhook(netguard.New())
	w.AllowHTTP = true
	_, err := w.Send(context.Background(), msg("webhook"), channel.Credential{Config: map[string]any{"url": srv.URL}, Secret: map[string]any{"secret": secret}})
	se := classify(t, err)
	assert.Equal(t, "endpoint_blocked", se.Code)
	assert.False(t, se.Retryable)
	assert.Empty(t, rc.reqs)
}

func TestSlackIncomingWebhook(t *testing.T) {
	t.Parallel()
	rc := &recorder{respond: body(200, "ok")}
	srv := rc.server(t)
	s := vendor.NewSlack(loopback)
	s.HooksBase = srv.URL
	hookPath := "/services/" + strings.Repeat("T", 9) + "/" + strings.Repeat("B", 9) + "/" + strings.Repeat("x", 24)
	cred := channel.Credential{Secret: map[string]any{"webhook_url": "https://hooks.slack.com" + hookPath}}
	res, err := s.IncomingWebhook().Send(context.Background(), msg("slack"), cred)
	require.NoError(t, err)
	assert.True(t, res.Delivered)
	r, b := rc.last()
	assert.Equal(t, hookPath, r.URL.Path)
	assert.JSONEq(t, `{"text":"Order A1","blocks":[{"type":"divider"}]}`, string(b))

	rc.respond = body(404, "no_service")
	_, err = s.IncomingWebhook().Send(context.Background(), msg("slack"), cred)
	se := classify(t, err)
	assert.Equal(t, "destination_gone", se.Code)
	assert.NotContains(t, se.Message, hookPath, "the webhook URL is a secret")

	_, err = s.IncomingWebhook().Send(context.Background(), msg("slack"), channel.Credential{Secret: map[string]any{"webhook_url": "https://evil.test/services/x"}})
	assert.Equal(t, "credential_invalid", classify(t, err).Code)
}

func TestSlackBot(t *testing.T) {
	t.Parallel()
	rc := &recorder{respond: body(200, `{"ok":true,"channel":"C123","ts":"1700000000.0001"}`)}
	srv := rc.server(t)
	s := vendor.NewSlack(loopback)
	s.APIBase = srv.URL
	cred := channel.Credential{Config: map[string]any{"default_channel": "#ops"}, Secret: map[string]any{"bot_token": "test-bot-token"}}
	res, err := s.Bot().Send(context.Background(), msg("slack"), cred)
	require.NoError(t, err)
	assert.True(t, res.Delivered)
	assert.Equal(t, "C123:1700000000.0001", res.VendorMessageID)
	r, b := rc.last()
	assert.Equal(t, "/chat.postMessage", r.URL.Path)
	assert.Equal(t, "Bearer test-bot-token", r.Header.Get("Authorization"))
	var p map[string]any
	require.NoError(t, json.Unmarshal(b, &p))
	assert.Equal(t, "#ops", p["channel"], "default channel when no slack_user")

	m := msg("slack")
	m.To = "U999"
	_, err = s.Bot().Send(context.Background(), m, cred)
	require.NoError(t, err)
	_, b = rc.last()
	require.NoError(t, json.Unmarshal(b, &p))
	assert.Equal(t, "U999", p["channel"], "slack_user wins")

	for errStr, want := range map[string][2]any{
		"channel_not_found": {"slack_channel_not_found", false},
		"token_revoked":     {"auth_failed", false},
		"ratelimited":       {"vendor_unavailable", true},
		"Weird Error!":      {"slack_rejected", false},
	} {
		rc.respond = body(200, `{"ok":false,"error":"`+errStr+`"}`)
		_, err := s.Bot().Send(context.Background(), msg("slack"), cred)
		se := classify(t, err)
		assert.Equal(t, want[0], se.Code, errStr)
		assert.Equal(t, want[1], se.Retryable, errStr)
	}
	_, err = s.Bot().Send(context.Background(), msg("slack"), channel.Credential{Secret: map[string]any{"bot_token": "t"}})
	assert.Equal(t, "recipient_missing", classify(t, err).Code)
}

func TestSendGrid(t *testing.T) {
	t.Parallel()
	rc := &recorder{respond: status(202, "X-Message-Id", "sg-1")}
	srv := rc.server(t)
	sg := vendor.NewSendGrid(loopback)
	sg.Base = srv.URL
	m := msg("email")
	m.FromName, m.ReplyTo = "Brand", "help@acme.test"
	cred := channel.Credential{Config: map[string]any{"from_email": "no-reply@acme.test"}, Secret: map[string]any{"api_key": "test-key"}}
	res, err := sg.Send(context.Background(), m, cred)
	require.NoError(t, err)
	assert.False(t, res.Delivered, "sent; delivery comes from event webhooks")
	assert.Equal(t, "sg-1", res.VendorMessageID)
	r, b := rc.last()
	assert.Equal(t, "/v3/mail/send", r.URL.Path)
	assert.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
	assert.JSONEq(t, `{
		"personalizations":[{"to":[{"email":"user@example.com"}],"custom_args":{"notification_id":"`+m.NotificationID.String()+`"}}],
		"from":{"email":"no-reply@acme.test","name":"Brand"},
		"reply_to":{"email":"help@acme.test"},
		"subject":"Order A1",
		"content":[{"type":"text/plain","value":"Hi"},{"type":"text/html","value":"<p>Hi</p>"}]}`, string(b))

	cred.Config["from_name"] = "Acme Orders"
	_, err = sg.Send(context.Background(), m, cred)
	require.NoError(t, err)
	_, b = rc.last()
	assert.Contains(t, string(b), `"name":"Acme Orders"`, "the credential's from_name wins over the brand")

	for code, want := range map[int]string{400: "email_rejected", 401: "auth_failed", 429: "rate_limited", 500: "vendor_unavailable"} {
		rc.respond = status(code)
		_, err := sg.Send(context.Background(), m, cred)
		assert.Equal(t, want, classify(t, err).Code, code)
	}
}

func TestTwilio(t *testing.T) {
	t.Parallel()
	rc := &recorder{respond: body(201, `{"sid":"SM1"}`)}
	srv := rc.server(t)
	tw := vendor.NewTwilio(loopback)
	tw.Base = srv.URL
	sid := "AC" + strings.Repeat("ab", 16)
	cred := channel.Credential{Config: map[string]any{"account_sid": sid, "from": "+14155550100"}, Secret: map[string]any{"auth_token": "test-token"}}
	res, err := tw.Send(context.Background(), msg("sms"), cred)
	require.NoError(t, err)
	assert.Equal(t, "SM1", res.VendorMessageID)
	assert.False(t, res.Delivered)
	r, b := rc.last()
	assert.Equal(t, "/2010-04-01/Accounts/"+sid+"/Messages.json", r.URL.Path)
	user, pass, ok := r.BasicAuth()
	assert.True(t, ok)
	assert.Equal(t, sid, user)
	assert.Equal(t, "test-token", pass)
	form, err := url.ParseQuery(string(b))
	require.NoError(t, err)
	assert.Equal(t, "+919876543210", form.Get("To"))
	assert.Equal(t, "Order A1", form.Get("Body"))
	assert.Equal(t, "+14155550100", form.Get("From"))

	for name, c := range map[string]struct {
		respond   func(http.ResponseWriter, *http.Request)
		code      string
		retryable bool
	}{
		"invalid number": {body(400, `{"code":21211,"message":"The 'To' number +91... is not valid"}`), "invalid_recipient", false},
		"opted out":      {body(400, `{"code":21610}`), "recipient_opted_out", false},
		"other code":     {body(400, `{"code":21606}`), "twilio_21606", false},
		"auth":           {body(401, `{"code":20003}`), "auth_failed", false},
		"throttled":      {status(429), "rate_limited", true},
	} {
		rc.respond = c.respond
		_, err := tw.Send(context.Background(), msg("sms"), cred)
		se := classify(t, err)
		assert.Equal(t, c.code, se.Code, name)
		assert.Equal(t, c.retryable, se.Retryable, name)
		assert.NotContains(t, se.Message, "+91", "%s: vendor text (which may hold the number) is not kept", name)
	}
	_, err = tw.Send(context.Background(), msg("sms"), channel.Credential{Config: map[string]any{"account_sid": "AC/../x"}})
	assert.Equal(t, "credential_invalid", classify(t, err).Code, "no path injection through the SID")
}

func TestTransportErrors(t *testing.T) {
	t.Parallel()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(2 * time.Second) }))
	defer slow.Close()
	sg := &vendor.SendGrid{Client: loopback.HTTPClient(200*time.Millisecond, nil), Base: slow.URL}
	_, err := sg.Send(context.Background(), msg("email"), channel.Credential{Config: map[string]any{"from_email": "a@acme.test"}})
	se := classify(t, err)
	assert.Equal(t, "vendor_timeout", se.Code)
	assert.True(t, se.Retryable)

	sg.Base = "http://127.0.0.1:1"
	_, err = sg.Send(context.Background(), msg("email"), channel.Credential{Config: map[string]any{"from_email": "a@acme.test"}})
	se = classify(t, err)
	assert.Equal(t, "vendor_unreachable", se.Code)
	assert.True(t, se.Retryable)
	assert.NotContains(t, se.Message, "127.0.0.1", "transport error text is not kept")
}
