package vendor

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	sestypes "github.com/aws/aws-sdk-go-v2/service/sesv2/types"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	snstypes "github.com/aws/aws-sdk-go-v2/service/sns/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"

	"github.com/ticatwolves/notifex/internal/channel"
)

// SESAPI is the part of the SES v2 client the sender uses.
type SESAPI interface {
	SendEmail(ctx context.Context, in *sesv2.SendEmailInput, opts ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error)
}

// SNSAPI is the part of the SNS client the sender uses.
type SNSAPI interface {
	Publish(ctx context.Context, in *sns.PublishInput, opts ...func(*sns.Options)) (*sns.PublishOutput, error)
}

// SES sends email through Amazon SES v2: platform email with the
// dispatcher's own role, BYO SES by assuming the tenant's role with the
// credential's external_id (confused-deputy protection, ADR-013).
type SES struct {
	// Platform is the client for platform email (nil: platform email off).
	Platform SESAPI
	// PlatformFrom is the platform sender address (a verified identity).
	PlatformFrom string
	// ConfigurationSet, if set, is attached to every send (Phase 4 events).
	ConfigurationSet string
	// Base is the dispatcher's AWS config, used to assume tenant roles.
	Base aws.Config
	// ForRole builds a client for a tenant role (tests replace it).
	ForRole func(ctx context.Context, roleARN, externalID, region string) (SESAPI, error)

	mu    sync.Mutex
	roles map[string]SESAPI
}

// NewSES returns an SES sender. platform may be nil.
func NewSES(base aws.Config, platform SESAPI, platformFrom, configSet string) *SES {
	s := &SES{Platform: platform, PlatformFrom: platformFrom, ConfigurationSet: configSet, Base: base, roles: map[string]SESAPI{}}
	s.ForRole = s.assume
	return s
}

// assume builds an SES client whose credentials come from AssumeRole with
// the external id; the credentials cache refreshes them before expiry.
func (s *SES) assume(_ context.Context, roleARN, externalID, region string) (SESAPI, error) {
	prov := stscreds.NewAssumeRoleProvider(sts.NewFromConfig(s.Base), roleARN, func(o *stscreds.AssumeRoleOptions) {
		o.ExternalID = aws.String(externalID)
		o.RoleSessionName = "notifex-dispatch"
		o.Duration = 15 * time.Minute
	})
	cfg := s.Base.Copy()
	cfg.Region = region
	cfg.Credentials = aws.NewCredentialsCache(prov)
	return sesv2.NewFromConfig(cfg), nil
}

func (s *SES) client(ctx context.Context, c channel.Credential) (SESAPI, error) {
	role, ext, region := cfgStr(c.Config, "role_arn"), cfgStr(c.Config, "external_id"), cfgStr(c.Config, "region")
	if role == "" || ext == "" || region == "" {
		return nil, channel.Permanent("credential_invalid", "the SES credential is incomplete")
	}
	key := role + "|" + ext + "|" + region
	s.mu.Lock()
	defer s.mu.Unlock()
	if cl, ok := s.roles[key]; ok {
		return cl, nil
	}
	cl, err := s.ForRole(ctx, role, ext, region)
	if err != nil {
		return nil, fmt.Errorf("ses: client for role: %w", err)
	}
	if s.roles == nil || len(s.roles) >= 1000 {
		s.roles = map[string]SESAPI{}
	}
	s.roles[key] = cl
	return cl, nil
}

// PlatformSender is the sender for email/platform.
func (s *SES) PlatformSender() channel.Sender {
	return channel.SenderFunc(func(ctx context.Context, m channel.Message, c channel.Credential) (channel.Result, error) {
		if s.Platform == nil || s.PlatformFrom == "" {
			return channel.Result{}, channel.Retryable("platform_unavailable", "platform email is not configured")
		}
		replyTo := m.ReplyTo
		if r := cfgStr(c.Config, "reply_to"); r != "" {
			replyTo = r
		}
		return s.send(ctx, s.Platform, true, m, s.PlatformFrom, fromName(m, c), replyTo)
	})
}

// BYOSender is the sender for email/ses.
func (s *SES) BYOSender() channel.Sender {
	return channel.SenderFunc(func(ctx context.Context, m channel.Message, c channel.Credential) (channel.Result, error) {
		cl, err := s.client(ctx, c)
		if err != nil {
			return channel.Result{}, err
		}
		return s.send(ctx, cl, false, m, cfgStr(c.Config, "from_email"), fromName(m, c), m.ReplyTo)
	})
}

func (s *SES) send(ctx context.Context, cl SESAPI, platform bool, m channel.Message, from, name, replyTo string) (channel.Result, error) {
	if m.Email == nil {
		return channel.Result{}, channel.Permanent("render_failed", "no email content")
	}
	if m.To == "" {
		return channel.Result{}, channel.Permanent("recipient_missing", "no email address")
	}
	body := &sestypes.Body{}
	if m.Email.Text != "" {
		body.Text = &sestypes.Content{Data: aws.String(m.Email.Text), Charset: aws.String("UTF-8")}
	}
	if m.Email.HTML != "" {
		body.Html = &sestypes.Content{Data: aws.String(m.Email.HTML), Charset: aws.String("UTF-8")}
	}
	// mail.Address encodes the display name (RFC 2047): no header injection.
	fromHdr := from
	if name != "" {
		fromHdr = (&mail.Address{Name: name, Address: from}).String()
	}
	in := &sesv2.SendEmailInput{
		FromEmailAddress: aws.String(fromHdr),
		Destination:      &sestypes.Destination{ToAddresses: []string{m.To}},
		Content: &sestypes.EmailContent{Simple: &sestypes.Message{
			Subject: &sestypes.Content{Data: aws.String(m.Email.Subject), Charset: aws.String("UTF-8")},
			Body:    body,
		}},
		EmailTags: []sestypes.MessageTag{
			{Name: aws.String("notification_id"), Value: aws.String(m.NotificationID.String())},
			{Name: aws.String("tenant_id"), Value: aws.String(m.TenantID.String())},
		},
	}
	if replyTo != "" {
		in.ReplyToAddresses = []string{replyTo}
	}
	if s.ConfigurationSet != "" {
		in.ConfigurationSetName = aws.String(s.ConfigurationSet)
	}
	out, err := cl.SendEmail(ctx, in)
	if err != nil {
		return channel.Result{}, awsError(err, platform)
	}
	return channel.Result{VendorMessageID: short(aws.ToString(out.MessageId), 128)}, nil
}

// SNS sends platform SMS through Amazon SNS (ADR-005). Acceptance is
// `sent`, which is also the billing point for platform SMS.
type SNS struct {
	API SNSAPI
}

// Send implements channel.Sender.
func (s *SNS) Send(ctx context.Context, m channel.Message, _ channel.Credential) (channel.Result, error) {
	if s.API == nil {
		return channel.Result{}, channel.Retryable("platform_unavailable", "platform SMS is not configured")
	}
	if m.SMS == nil {
		return channel.Result{}, channel.Permanent("render_failed", "no SMS content")
	}
	if m.To == "" {
		return channel.Result{}, channel.Permanent("recipient_missing", "no phone number")
	}
	out, err := s.API.Publish(ctx, &sns.PublishInput{
		PhoneNumber: aws.String(m.To),
		Message:     aws.String(m.SMS.Text),
		MessageAttributes: map[string]snstypes.MessageAttributeValue{
			"AWS.SNS.SMS.SMSType": {DataType: aws.String("String"), StringValue: aws.String("Transactional")},
		},
	})
	if err != nil {
		return channel.Result{}, awsError(err, true)
	}
	return channel.Result{VendorMessageID: short(aws.ToString(out.MessageId), 128)}, nil
}

// awsError classifies SES, SNS and STS errors. For platform sends, our own
// misconfiguration (auth, paused sending) is retryable: operators fix it
// within the 48 h window and the messages go out. For BYO it's permanent:
// the tenant has to fix their account.
func awsError(err error, platform bool) *channel.SendError {
	var ae smithy.APIError
	if !errors.As(err, &ae) {
		return netError(err)
	}
	switch ae.ErrorCode() {
	case "Throttling", "ThrottlingException", "TooManyRequestsException", "LimitExceededException", "ThrottledException", "KMSThrottlingException":
		return &channel.SendError{Code: "rate_limited", Message: "AWS is throttling sends", Retryable: true, RetryAfter: 30 * time.Second}
	case "MessageRejected", "BadRequestException":
		return channel.Permanent("email_rejected", "SES rejected the message")
	case "MailFromDomainNotVerifiedException", "NotFoundException":
		if platform {
			return channel.Retryable("platform_unavailable", "the platform sender identity is not verified")
		}
		return channel.Permanent("sender_not_verified", "the sender identity is not verified in SES")
	case "AccountSuspendedException", "SendingPausedException":
		if platform {
			return channel.Retryable("platform_unavailable", "platform sending is paused")
		}
		return channel.Permanent("sending_paused", "sending is paused for the SES account")
	case "AccessDenied", "AccessDeniedException", "AuthorizationError", "AuthorizationErrorException", "InvalidClientTokenId", "ExpiredToken":
		if platform {
			return channel.Retryable("platform_unavailable", "the dispatcher is not authorized to send")
		}
		return channel.Permanent("auth_failed", "Notifex could not assume the SES role or was denied (check the trust policy and external_id)")
	case "InvalidParameter", "InvalidParameterException", "InvalidParameterValue", "InvalidParameterValueException":
		return channel.Permanent("invalid_recipient", "AWS rejected the destination")
	case "OptedOut", "OptedOutException":
		return channel.Permanent("recipient_opted_out", "the number opted out of SMS")
	}
	if ae.ErrorFault() == smithy.FaultServer {
		return channel.Retryable("vendor_unavailable", "AWS answered with a server error")
	}
	return channel.Permanent("vendor_rejected", "AWS rejected the request ("+short(ae.ErrorCode(), 60)+")")
}
