package credential

import (
	"encoding/json"
	"fmt"
	"net/mail"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// FieldType is a provider field's type.
type FieldType string

// Field types.
const (
	TypeString  FieldType = "string"
	TypeInteger FieldType = "integer"
	TypeEnum    FieldType = "enum"
)

// Field describes one config or secret field of a provider.
type Field struct {
	Name        string
	Type        FieldType
	Required    bool
	Values      []string // enum values (or allowed integers as strings)
	Description string
	// ReadOnly fields are set by Notifex (ses external_id), never by callers.
	ReadOnly bool
	check    func(string) error
}

// Provider is one way to send on a channel (ADR-013 §3).
type Provider struct {
	Channel         string
	Name            string
	Mode            string // "platform" or "byo"
	Config          []Field
	Secret          []Field
	SecretGenerated bool   // Notifex generates the secret (webhook signing)
	HintField       string // secret field whose last 4 characters are the hint
	// cross checks fields together (twilio: exactly one sender).
	cross func(cfg map[string]any) error
}

// HasSecret reports whether the provider stores a secret.
func (p Provider) HasSecret() bool { return len(p.Secret) > 0 || p.SecretGenerated }

// MaxSecretBytes caps a credential's secret JSON (ADR-013 §2).
const MaxSecretBytes = 16 << 10

var (
	twilioSID   = regexp.MustCompile(`^AC[0-9a-f]{32}$`)
	twilioMSID  = regexp.MustCompile(`^MG[0-9a-f]{32}$`)
	twilioToken = regexp.MustCompile(`^[0-9a-f]{32}$`)
	e164        = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)
	sendgridKey = regexp.MustCompile(`^SG\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}$`)
	slackBot    = regexp.MustCompile(`^xoxb-[A-Za-z0-9-]{10,}$`)
	slackChan   = regexp.MustCompile(`^(#?[a-z0-9._-]{1,80}|[CGD][A-Z0-9]{8,})$`)
	roleARN     = regexp.MustCompile(`^arn:aws:iam::[0-9]{12}:role/[A-Za-z0-9+=,.@_/-]{1,512}$`)
	hostLabel   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

// sesRegions are the regions BYO SES may use.
var sesRegions = []string{"ap-south-1", "ap-southeast-1", "ap-southeast-2", "ap-northeast-1", "eu-west-1", "eu-west-2", "eu-central-1", "us-east-1", "us-east-2", "us-west-2", "ca-central-1", "sa-east-1"}

func emailAddr(s string) error {
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s || len(s) > 254 {
		return fmt.Errorf("must be a plain email address")
	}
	return nil
}

func displayName(s string) error {
	if utf8.RuneCountInString(s) > 100 {
		return fmt.Errorf("at most 100 characters")
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("control characters are not allowed")
		}
	}
	return nil
}

func matches(re *regexp.Regexp, what string) func(string) error {
	return func(s string) error {
		if !re.MatchString(s) {
			return fmt.Errorf("is not a valid %s", what)
		}
		return nil
	}
}

func maxLen(n int) func(string) error {
	return func(s string) error {
		if len(s) > n {
			return fmt.Errorf("at most %d characters", n)
		}
		return nil
	}
}

// blockedPrefixes are address ranges an SMTP host may never point at, beyond
// what netip's predicates cover.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // CGNAT
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"), // NAT64 can reach IPv4 private space
}

// PublicAddr reports whether ip is a public unicast address. Phase 3's
// dialer applies the same rule to resolved addresses (DNS rebinding).
func PublicAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// SMTPHost is the ADR-013 §3 save-time rule: a public IP literal, or a DNS
// name with at least two labels and an alphabetic top-level label, and not a
// local or internal name. No DNS lookup happens here.
func SMTPHost(h string) error {
	h = strings.TrimSuffix(strings.ToLower(h), ".")
	if h == "" || len(h) > 253 {
		return fmt.Errorf("must be a host name or public IP address")
	}
	if ip, err := netip.ParseAddr(strings.Trim(h, "[]")); err == nil {
		if !PublicAddr(ip) {
			return fmt.Errorf("must not be a private, loopback, link-local or reserved address")
		}
		return nil
	}
	labels := strings.Split(h, ".")
	if len(labels) < 2 {
		return fmt.Errorf("must be a fully qualified host name")
	}
	for _, l := range labels {
		if !hostLabel.MatchString(l) {
			return fmt.Errorf("must be a host name or public IP address")
		}
	}
	tld := labels[len(labels)-1]
	if strings.Trim(tld, "0123456789") == "" || strings.HasPrefix(tld, "0x") {
		return fmt.Errorf("must be a host name or public IP address") // numeric forms like 2130706433 or 0x7f.1
	}
	switch tld {
	case "local", "localhost", "internal", "lan", "home", "corp", "intranet", "localdomain":
		return fmt.Errorf("must not be a local or internal name")
	}
	return nil
}

func slackWebhook(s string) error {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host != "hooks.slack.com" || u.User != nil ||
		!strings.HasPrefix(u.Path, "/services/") || u.RawQuery != "" || u.Fragment != "" || len(s) > 512 {
		return fmt.Errorf("must be a https://hooks.slack.com/services/… URL")
	}
	return nil
}

var (
	fromEmail = Field{Name: "from_email", Type: TypeString, Required: true, Description: "Sender address.", check: emailAddr}
	fromName  = Field{Name: "from_name", Type: TypeString, Description: "Sender display name.", check: displayName}
)

// Catalog is every provider Notifex supports in Phase 2.
var Catalog = []Provider{
	{Channel: "email", Name: "platform", Mode: "platform", Config: []Field{
		fromName,
		{Name: "reply_to", Type: TypeString, Description: "Reply-to address.", check: emailAddr},
	}},
	{Channel: "email", Name: "sendgrid", Mode: "byo", HintField: "api_key",
		Config: []Field{fromEmail, fromName},
		Secret: []Field{{Name: "api_key", Type: TypeString, Required: true, Description: "SendGrid API key (SG.…).", check: matches(sendgridKey, "SendGrid API key")}}},
	{Channel: "email", Name: "smtp", Mode: "byo",
		Config: []Field{
			{Name: "host", Type: TypeString, Required: true, Description: "SMTP host (public).", check: SMTPHost},
			{Name: "port", Type: TypeInteger, Required: true, Values: []string{"25", "465", "587", "2525"}, Description: "SMTP port."},
			{Name: "tls", Type: TypeEnum, Required: true, Values: []string{"starttls", "implicit"}, Description: "TLS mode."},
			{Name: "username", Type: TypeString, Description: "SMTP user name.", check: maxLen(256)},
			fromEmail, fromName,
		},
		Secret: []Field{{Name: "password", Type: TypeString, Required: true, Description: "SMTP password.", check: maxLen(1024)}}},
	{Channel: "email", Name: "ses", Mode: "byo",
		Config: []Field{
			{Name: "role_arn", Type: TypeString, Required: true, Description: "IAM role in your account that trusts Notifex.", check: matches(roleARN, "IAM role ARN")},
			{Name: "region", Type: TypeEnum, Required: true, Values: sesRegions, Description: "SES region."},
			{Name: "external_id", Type: TypeString, ReadOnly: true, Description: "Set by Notifex; use it in the role's trust policy."},
			fromEmail, fromName,
		}},
	{Channel: "sms", Name: "platform", Mode: "platform"},
	{Channel: "sms", Name: "twilio", Mode: "byo", HintField: "auth_token",
		Config: []Field{
			{Name: "account_sid", Type: TypeString, Required: true, Description: "Twilio account SID (AC…).", check: matches(twilioSID, "Twilio account SID")},
			{Name: "from", Type: TypeString, Description: "Sender number (E.164). Set this or messaging_service_sid.", check: matches(e164, "E.164 number")},
			{Name: "messaging_service_sid", Type: TypeString, Description: "Messaging service SID (MG…).", check: matches(twilioMSID, "messaging service SID")},
		},
		Secret: []Field{{Name: "auth_token", Type: TypeString, Required: true, Description: "Twilio auth token.", check: matches(twilioToken, "Twilio auth token")}},
		cross: func(cfg map[string]any) error {
			_, a := cfg["from"]
			_, b := cfg["messaging_service_sid"]
			if a == b {
				return fieldErr("config", "set exactly one of from and messaging_service_sid")
			}
			return nil
		}},
	{Channel: "slack", Name: "incoming_webhook", Mode: "byo", HintField: "webhook_url",
		Config: []Field{{Name: "channel_label", Type: TypeString, Description: "Label for humans.", check: maxLen(80)}},
		Secret: []Field{{Name: "webhook_url", Type: TypeString, Required: true, Description: "Incoming webhook URL.", check: slackWebhook}}},
	{Channel: "slack", Name: "bot_token", Mode: "byo", HintField: "bot_token",
		Config: []Field{{Name: "default_channel", Type: TypeString, Description: "Channel name or id.", check: matches(slackChan, "Slack channel")}},
		Secret: []Field{{Name: "bot_token", Type: TypeString, Required: true, Description: "Bot token (xoxb-…).", check: matches(slackBot, "Slack bot token")}}},
	{Channel: "webhook", Name: "signing", Mode: "byo", SecretGenerated: true, HintField: "secret"},
}

// Find returns the provider for channel and name.
func Find(channel, name string) (Provider, bool) {
	for _, p := range Catalog {
		if p.Channel == channel && p.Name == name {
			return p, true
		}
	}
	return Provider{}, false
}

// validateFields checks m against fields: no unknown or read-only keys,
// required present, types and per-field rules. It returns a normalized copy
// (integers as int).
func validateFields(kind string, fields []Field, m map[string]any) (map[string]any, error) {
	out := map[string]any{}
	byName := map[string]Field{}
	for _, f := range fields {
		byName[f.Name] = f
	}
	for k, v := range m {
		f, ok := byName[k]
		if !ok || f.ReadOnly {
			return nil, fieldErr(kind+"."+truncate(k, 40), "unknown or read-only field")
		}
		switch f.Type {
		case TypeInteger:
			n, ok := asInt(v)
			if !ok || (len(f.Values) > 0 && !contains(f.Values, strconv.Itoa(n))) {
				return nil, fieldErr(kind+"."+k, "must be one of %s", strings.Join(f.Values, ", "))
			}
			out[k] = n
		default:
			s, ok := v.(string)
			if !ok || s == "" {
				return nil, fieldErr(kind+"."+k, "must be a non-empty string")
			}
			if f.Type == TypeEnum && !contains(f.Values, s) {
				return nil, fieldErr(kind+"."+k, "must be one of %s", strings.Join(f.Values, ", "))
			}
			if f.check != nil {
				if err := f.check(s); err != nil {
					return nil, fieldErr(kind+"."+k, "%s", err.Error())
				}
			}
			out[k] = s
		}
	}
	for _, f := range fields {
		if _, ok := out[f.Name]; f.Required && !ok {
			return nil, fieldErr(kind+"."+f.Name, "is required")
		}
	}
	return out, nil
}

func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := strconv.Atoi(n.String())
		return i, err == nil
	case float64:
		if n != float64(int(n)) {
			return 0, false
		}
		return int(n), true
	case int:
		return n, true
	default:
		return 0, false
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
