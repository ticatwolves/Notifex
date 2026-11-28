package vendor

import (
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sns"

	"github.com/ticatwolves/notifex/internal/channel"
	"github.com/ticatwolves/notifex/internal/netguard"
)

// LiveConfig configures the real adapters.
type LiveConfig struct {
	AWS              aws.Config // the dispatcher's role and region
	PlatformFrom     string     // platform email sender; empty disables platform email
	ConfigurationSet string     // optional SES configuration set
	EHLOName         string     // SMTP EHLO name
}

// RegisterLive registers a real sender for every (channel, provider) in
// the credential catalog. Tenant-chosen destinations (webhook URLs, SMTP
// hosts) and vendor APIs all go through the netguard dialer.
func RegisterLive(r *channel.Registry, c LiveConfig) {
	g := netguard.New()
	var platform SESAPI
	if c.PlatformFrom != "" {
		platform = sesv2.NewFromConfig(c.AWS)
	}
	ses := NewSES(c.AWS, platform, c.PlatformFrom, c.ConfigurationSet)
	slack := NewSlack(g)
	r.Register("email", "platform", ses.PlatformSender())
	r.Register("email", "ses", ses.BYOSender())
	r.Register("email", "sendgrid", NewSendGrid(g))
	r.Register("email", "smtp", NewSMTP(g, c.EHLOName))
	r.Register("sms", "platform", &SNS{API: sns.NewFromConfig(c.AWS)})
	r.Register("sms", "twilio", NewTwilio(g))
	r.Register("slack", "incoming_webhook", slack.IncomingWebhook())
	r.Register("slack", "bot_token", slack.Bot())
	r.Register("webhook", "signing", NewWebhook(g))
}
