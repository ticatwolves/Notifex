package vendor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/ticatwolves/notifex/internal/channel"
	"github.com/ticatwolves/notifex/internal/netguard"
)

// Twilio sends SMS through the Messages API. Acceptance (201) is `sent`;
// delivery comes from status callbacks (Phase 4).
type Twilio struct {
	Client *http.Client
	Base   string // https://api.twilio.com (tests replace it)
}

// NewTwilio returns a Twilio sender over a guarded client.
func NewTwilio(g *netguard.Guard) *Twilio {
	return &Twilio{Client: g.HTTPClient(20*time.Second, nil), Base: "https://api.twilio.com"}
}

var twilioSIDRE = regexp.MustCompile(`^AC[0-9a-f]{32}$`)

// twilioPermanent maps Twilio error codes to our codes.
var twilioPermanent = map[int]string{
	21211: "invalid_recipient", 21614: "invalid_recipient", 21408: "destination_not_allowed",
	21610: "recipient_opted_out", 21612: "destination_not_allowed", 30007: "carrier_filtered",
}

// Send implements channel.Sender.
func (t *Twilio) Send(ctx context.Context, m channel.Message, c channel.Credential) (channel.Result, error) {
	if m.SMS == nil {
		return channel.Result{}, channel.Permanent("render_failed", "no SMS content")
	}
	if m.To == "" {
		return channel.Result{}, channel.Permanent("recipient_missing", "no phone number")
	}
	sid := cfgStr(c.Config, "account_sid")
	if !twilioSIDRE.MatchString(sid) {
		return channel.Result{}, channel.Permanent("credential_invalid", "the Twilio account SID is invalid")
	}
	form := url.Values{"To": {m.To}, "Body": {m.SMS.Text}}
	if ms := cfgStr(c.Config, "messaging_service_sid"); ms != "" {
		form.Set("MessagingServiceSid", ms)
	} else {
		form.Set("From", cfgStr(c.Config, "from"))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.Base+"/2010-04-01/Accounts/"+sid+"/Messages.json", strings.NewReader(form.Encode()))
	if err != nil {
		return channel.Result{}, channel.Permanent("credential_invalid", "bad Twilio request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(sid, cfgStr(c.Secret, "auth_token"))
	resp, err := t.Client.Do(req)
	if err != nil {
		return channel.Result{}, netError(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out struct {
		SID  string `json:"sid"`
		Code int    `json:"code"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, respLimit)).Decode(&out)
	netguard.Drain(resp.Body, respLimit)
	switch {
	case resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK:
		return channel.Result{VendorMessageID: short(out.SID, 128)}, nil
	case resp.StatusCode == http.StatusBadRequest && out.Code != 0:
		code, ok := twilioPermanent[out.Code]
		if !ok {
			code = fmt.Sprintf("twilio_%d", out.Code)
		}
		return channel.Result{}, channel.Permanent(code, fmt.Sprintf("Twilio rejected the message (error %d)", out.Code))
	}
	return channel.Result{}, httpStatus(resp.StatusCode, resp.Header, "sms_rejected")
}
