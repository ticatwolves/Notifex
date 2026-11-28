package vendor

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/ticatwolves/notifex/internal/channel"
	"github.com/ticatwolves/notifex/internal/netguard"
)

const slackHooksBase = "https://hooks.slack.com"

// Slack sends through an incoming webhook or a bot token
// (chat.postMessage). Both are synchronous: success is delivery.
type Slack struct {
	Client *http.Client
	// HooksBase and APIBase replace the Slack hosts (tests only).
	HooksBase string // https://hooks.slack.com
	APIBase   string // https://slack.com/api
}

// NewSlack returns a Slack sender over a guarded client.
func NewSlack(g *netguard.Guard) *Slack {
	return &Slack{Client: g.HTTPClient(15*time.Second, nil), HooksBase: slackHooksBase, APIBase: "https://slack.com/api"}
}

// IncomingWebhook is the sender for slack/incoming_webhook.
func (s *Slack) IncomingWebhook() channel.Sender { return channel.SenderFunc(s.sendHook) }

// Bot is the sender for slack/bot_token.
func (s *Slack) Bot() channel.Sender { return channel.SenderFunc(s.sendBot) }

func slackBody(m channel.Message, ch string) ([]byte, error) {
	if m.Slack == nil {
		return nil, channel.Permanent("render_failed", "no Slack content")
	}
	p := map[string]any{"text": m.Slack.Text}
	if len(m.Slack.Blocks) > 0 {
		p["blocks"] = m.Slack.Blocks
	}
	if ch != "" {
		p["channel"] = ch
	}
	b, err := json.Marshal(p)
	if err != nil {
		return nil, channel.Permanent("render_failed", "Slack content is not encodable")
	}
	return b, nil
}

func (s *Slack) sendHook(ctx context.Context, m channel.Message, c channel.Credential) (channel.Result, error) {
	hook := cfgStr(c.Secret, "webhook_url")
	path, ok := strings.CutPrefix(hook, slackHooksBase+"/services/")
	if !ok || strings.ContainsAny(path, "?#") {
		return channel.Result{}, channel.Permanent("credential_invalid", "the Slack webhook URL is invalid; update the credential")
	}
	body, err := slackBody(m, "")
	if err != nil {
		return channel.Result{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.HooksBase+"/services/"+path, bytes.NewReader(body))
	if err != nil {
		return channel.Result{}, channel.Permanent("credential_invalid", "the Slack webhook URL is invalid; update the credential")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.Client.Do(req)
	if err != nil {
		return channel.Result{}, netError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	netguard.Drain(resp.Body, respLimit)
	switch resp.StatusCode {
	case http.StatusOK:
		return channel.Result{Delivered: true}, nil
	case http.StatusNotFound, http.StatusGone:
		return channel.Result{}, channel.Permanent("destination_gone", "the Slack webhook or its channel no longer exists")
	}
	return channel.Result{}, httpStatus(resp.StatusCode, resp.Header, "invalid_payload")
}

var slackErrCode = regexp.MustCompile(`^[a-z_]{1,40}$`)

func (s *Slack) sendBot(ctx context.Context, m channel.Message, c channel.Credential) (channel.Result, error) {
	ch := m.To
	if ch == "" {
		ch = cfgStr(c.Config, "default_channel")
	}
	if ch == "" {
		return channel.Result{}, channel.Permanent("recipient_missing", "no slack_user and no default_channel on the credential")
	}
	body, err := slackBody(m, ch)
	if err != nil {
		return channel.Result{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.APIBase+"/chat.postMessage", bytes.NewReader(body))
	if err != nil {
		return channel.Result{}, channel.Permanent("credential_invalid", "bad Slack API request")
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+cfgStr(c.Secret, "bot_token"))
	resp, err := s.Client.Do(req)
	if err != nil {
		return channel.Result{}, netError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		netguard.Drain(resp.Body, respLimit)
		return channel.Result{}, httpStatus(resp.StatusCode, resp.Header, "invalid_payload")
	}
	var out struct {
		OK      bool   `json:"ok"`
		Error   string `json:"error"`
		TS      string `json:"ts"`
		Channel string `json:"channel"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, respLimit)).Decode(&out); err != nil {
		return channel.Result{}, channel.Retryable("vendor_error", "unreadable Slack response")
	}
	if out.OK {
		return channel.Result{VendorMessageID: short(out.Channel+":"+out.TS, 128), Delivered: true}, nil
	}
	switch out.Error {
	case "ratelimited", "internal_error", "fatal_error", "service_unavailable", "request_timeout":
		return channel.Result{}, &channel.SendError{Code: "vendor_unavailable", Message: "Slack: " + out.Error, Retryable: true, RetryAfter: retryAfter(resp.Header)}
	case "invalid_auth", "not_authed", "account_inactive", "token_revoked", "token_expired", "missing_scope", "no_permission":
		return channel.Result{}, channel.Permanent("auth_failed", "Slack: "+out.Error)
	}
	code := "slack_rejected"
	if slackErrCode.MatchString(out.Error) {
		code = "slack_" + out.Error
	}
	return channel.Result{}, channel.Permanent(short(code, 64), "Slack rejected the message")
}
