package trigger

import (
	"crypto/hmac"
	"crypto/sha256"
	"strings"

	"github.com/ticatwolves/notifex/internal/observability"
)

// Recipient is one trigger recipient, passed inline (no subscriber model).
type Recipient struct {
	ID         string   `json:"id,omitempty"`
	Email      string   `json:"email,omitempty"`
	Phone      string   `json:"phone,omitempty"`
	PushTokens []string `json:"push_tokens,omitempty"`
	SlackUser  string   `json:"slack_user,omitempty"`
	WhatsApp   string   `json:"whatsapp,omitempty"`
	Locale     string   `json:"locale,omitempty"`
	Timezone   string   `json:"timezone,omitempty"`
}

// address returns the recipient's address for channel, and whether the
// channel needs one. Slack can post to the credential's channel, and the
// webhook channel posts to the tenant's endpoint, so neither requires one.
func (r Recipient) address(channel string) (addr string, required bool) {
	switch channel {
	case "email":
		return strings.ToLower(strings.TrimSpace(r.Email)), true
	case "sms":
		return strings.TrimSpace(r.Phone), true
	case "whatsapp":
		return strings.TrimSpace(r.WhatsApp), true
	case "slack":
		return strings.TrimSpace(r.SlackUser), false
	default:
		return "", false
	}
}

// mask is the stored, displayable form of the address (ADR-006).
func mask(channel, addr string) string {
	switch {
	case addr == "":
		return ""
	case channel == "email":
		return observability.MaskEmail(addr)
	case channel == "sms" || channel == "whatsapp":
		return observability.MaskPhone(addr)
	case len(addr) <= 3:
		return "***"
	default:
		return addr[:2] + "***"
	}
}

// hashAddress is HMAC-SHA256(key, channel ":" address): a stable, keyed
// lookup value that doesn't reveal the address (ADR-014 §4).
func hashAddress(key []byte, channel, addr string) []byte {
	if addr == "" {
		return nil
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte(channel))
	m.Write([]byte{':'})
	m.Write([]byte(addr))
	return m.Sum(nil)
}

// localeChain is ADR-012 §7: exact, then language, then the brand default.
func localeChain(recipient, brandDefault string) []string {
	var out []string
	add := func(l string) {
		if l == "" {
			return
		}
		for _, x := range out {
			if x == l {
				return
			}
		}
		out = append(out, l)
	}
	add(recipient)
	if i := strings.IndexByte(recipient, '-'); i > 0 {
		add(recipient[:i])
	}
	add(brandDefault)
	return out
}
