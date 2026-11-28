package vendor_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/smithy-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ticatwolves/notifex/internal/channel"
	"github.com/ticatwolves/notifex/internal/channel/vendor"
)

type fakeSES struct {
	mu  sync.Mutex
	in  []*sesv2.SendEmailInput
	err error
}

func (f *fakeSES) SendEmail(_ context.Context, in *sesv2.SendEmailInput, _ ...func(*sesv2.Options)) (*sesv2.SendEmailOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.in = append(f.in, in)
	if f.err != nil {
		return nil, f.err
	}
	return &sesv2.SendEmailOutput{MessageId: aws.String("ses-1")}, nil
}

type fakeSNS struct {
	in  *sns.PublishInput
	err error
}

func (f *fakeSNS) Publish(_ context.Context, in *sns.PublishInput, _ ...func(*sns.Options)) (*sns.PublishOutput, error) {
	f.in = in
	if f.err != nil {
		return nil, f.err
	}
	return &sns.PublishOutput{MessageId: aws.String("sns-1")}, nil
}

func apiErr(code string, fault smithy.ErrorFault) error {
	return &smithy.GenericAPIError{Code: code, Message: "details with user@example.com", Fault: fault}
}

func TestSESPlatform(t *testing.T) {
	t.Parallel()
	f := &fakeSES{}
	s := vendor.NewSES(aws.Config{}, f, "notify@mail.notifex.test", "notifex-events")
	m := msg("email")
	m.FromName, m.ReplyTo = "Acme\r\nBcc: victim@x.test", "help@acme.test"
	res, err := s.PlatformSender().Send(context.Background(), m, channel.Credential{Mode: "platform", Provider: "platform"})
	require.NoError(t, err)
	assert.Equal(t, "ses-1", res.VendorMessageID)
	assert.False(t, res.Delivered)
	in := f.in[0]
	from := aws.ToString(in.FromEmailAddress)
	assert.True(t, strings.HasSuffix(from, "<notify@mail.notifex.test>"), from)
	assert.NotContains(t, from, "\n", "display name is RFC 2047-encoded: no header injection")
	assert.Equal(t, []string{"user@example.com"}, in.Destination.ToAddresses)
	assert.Equal(t, []string{"help@acme.test"}, in.ReplyToAddresses)
	assert.Equal(t, "Order A1", aws.ToString(in.Content.Simple.Subject.Data))
	assert.Equal(t, "Hi", aws.ToString(in.Content.Simple.Body.Text.Data))
	assert.Equal(t, "notifex-events", aws.ToString(in.ConfigurationSetName))
	assert.Equal(t, m.NotificationID.String(), aws.ToString(in.EmailTags[0].Value))

	// The platform credential's reply_to and from_name override the brand.
	_, err = s.PlatformSender().Send(context.Background(), m, channel.Credential{Config: map[string]any{"reply_to": "ops@acme.test", "from_name": "Ops"}})
	require.NoError(t, err)
	assert.Equal(t, []string{"ops@acme.test"}, f.in[1].ReplyToAddresses)
	assert.Equal(t, `"Ops" <notify@mail.notifex.test>`, aws.ToString(f.in[1].FromEmailAddress))

	off := vendor.NewSES(aws.Config{}, nil, "", "")
	_, err = off.PlatformSender().Send(context.Background(), m, channel.Credential{})
	assert.True(t, classify(t, err).Retryable, "platform email unconfigured: retry while ops fix it")
}

func TestSESBYORoleCache(t *testing.T) {
	t.Parallel()
	f := &fakeSES{}
	s := vendor.NewSES(aws.Config{}, nil, "", "")
	var calls []string
	s.ForRole = func(_ context.Context, role, ext, region string) (vendor.SESAPI, error) {
		calls = append(calls, role+"|"+ext+"|"+region)
		return f, nil
	}
	cred := channel.Credential{Mode: "byo", Provider: "ses", Config: map[string]any{
		"role_arn": "arn:aws:iam::123456789012:role/notifex", "external_id": "nfx-abc", "region": "eu-west-1", "from_email": "orders@acme.test"}}
	for range 3 {
		_, err := s.BYOSender().Send(context.Background(), msg("email"), cred)
		require.NoError(t, err)
	}
	assert.Equal(t, []string{"arn:aws:iam::123456789012:role/notifex|nfx-abc|eu-west-1"}, calls, "one client per role, reused")
	assert.Equal(t, "orders@acme.test", aws.ToString(f.in[0].FromEmailAddress))

	_, err := s.BYOSender().Send(context.Background(), msg("email"), channel.Credential{Config: map[string]any{"role_arn": "arn:aws:iam::1:role/x"}})
	assert.Equal(t, "credential_invalid", classify(t, err).Code, "no external_id: never assume without it")
}

// TestSESAssumeRoleWire: the real STS and SES clients against a fake AWS
// endpoint. AssumeRole carries the credential's external id, and the send
// is signed with the assumed role's keys, not the dispatcher's.
func TestSESAssumeRoleWire(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var stsForm url.Values
	var sesAuth, sesBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/v2/email/outbound-emails" {
			sesAuth, sesBody = r.Header.Get("Authorization"), string(b)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"MessageId":"ses-wire"}`)
			return
		}
		stsForm, _ = url.ParseQuery(string(b))
		w.Header().Set("Content-Type", "text/xml")
		_, _ = io.WriteString(w, `<AssumeRoleResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><AssumeRoleResult>`+
			`<Credentials><AccessKeyId>ASSUMEDKEY</AccessKeyId><SecretAccessKey>assumed</SecretAccessKey><SessionToken>tok</SessionToken>`+
			`<Expiration>2099-01-01T00:00:00Z</Expiration></Credentials>`+
			`<AssumedRoleUser><Arn>arn:aws:sts::123456789012:assumed-role/notifex/notifex-dispatch</Arn><AssumedRoleId>AROA:x</AssumedRoleId></AssumedRoleUser>`+
			`</AssumeRoleResult><ResponseMetadata><RequestId>1</RequestId></ResponseMetadata></AssumeRoleResponse>`)
	}))
	defer srv.Close()
	base := aws.Config{Region: "ap-south-1", BaseEndpoint: aws.String(srv.URL), HTTPClient: srv.Client(), RetryMaxAttempts: 1,
		Credentials: credentials.NewStaticCredentialsProvider("DISPATCHERKEY", "dispatcher", "")}
	s := vendor.NewSES(base, nil, "", "")
	res, err := s.BYOSender().Send(context.Background(), msg("email"), channel.Credential{Config: map[string]any{
		"role_arn": "arn:aws:iam::123456789012:role/notifex", "external_id": "nfx-ext-1", "region": "eu-west-1", "from_email": "orders@acme.test"}})
	require.NoError(t, err)
	assert.Equal(t, "ses-wire", res.VendorMessageID)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "AssumeRole", stsForm.Get("Action"))
	assert.Equal(t, "nfx-ext-1", stsForm.Get("ExternalId"))
	assert.Equal(t, "arn:aws:iam::123456789012:role/notifex", stsForm.Get("RoleArn"))
	assert.Contains(t, sesAuth, "Credential=ASSUMEDKEY/", "signed with the assumed role")
	assert.Contains(t, sesAuth, "/eu-west-1/ses/", "in the credential's region")
	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(sesBody), &body))
	assert.Equal(t, "orders@acme.test", body["FromEmailAddress"])
}

func TestSNS(t *testing.T) {
	t.Parallel()
	f := &fakeSNS{}
	s := &vendor.SNS{API: f}
	res, err := s.Send(context.Background(), msg("sms"), channel.Credential{Mode: "platform"})
	require.NoError(t, err)
	assert.Equal(t, "sns-1", res.VendorMessageID)
	assert.Equal(t, "+919876543210", aws.ToString(f.in.PhoneNumber))
	assert.Equal(t, "Order A1", aws.ToString(f.in.Message))
	assert.Equal(t, "Transactional", aws.ToString(f.in.MessageAttributes["AWS.SNS.SMS.SMSType"].StringValue))

	_, err = (&vendor.SNS{}).Send(context.Background(), msg("sms"), channel.Credential{})
	assert.Equal(t, "platform_unavailable", classify(t, err).Code)
}

func TestAWSErrorClassification(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		err       error
		platform  bool
		code      string
		retryable bool
	}{
		"throttled":              {apiErr("Throttling", smithy.FaultClient), false, "rate_limited", true},
		"rejected":               {apiErr("MessageRejected", smithy.FaultClient), false, "email_rejected", false},
		"byo access denied":      {apiErr("AccessDenied", smithy.FaultClient), false, "auth_failed", false},
		"platform access denied": {apiErr("AccessDenied", smithy.FaultClient), true, "platform_unavailable", true},
		"byo paused":             {apiErr("SendingPausedException", smithy.FaultClient), false, "sending_paused", false},
		"platform paused":        {apiErr("SendingPausedException", smithy.FaultClient), true, "platform_unavailable", true},
		"byo unverified sender":  {apiErr("MailFromDomainNotVerifiedException", smithy.FaultClient), false, "sender_not_verified", false},
		"invalid number":         {apiErr("InvalidParameter", smithy.FaultClient), true, "invalid_recipient", false},
		"server fault":           {apiErr("InternalFailure", smithy.FaultServer), false, "vendor_unavailable", true},
		"unknown client fault":   {apiErr("SomethingNew", smithy.FaultClient), false, "vendor_rejected", false},
		"transport":              {errors.New("dial tcp: connection refused"), false, "vendor_unreachable", true},
		"deadline":               {context.DeadlineExceeded, false, "vendor_timeout", true},
	} {
		var f fakeSES
		f.err = c.err
		s := vendor.NewSES(aws.Config{}, &f, "n@mail.notifex.test", "")
		s.ForRole = func(context.Context, string, string, string) (vendor.SESAPI, error) { return &f, nil }
		var err error
		if c.platform {
			_, err = (&vendor.SNS{API: &fakeSNS{err: c.err}}).Send(context.Background(), msg("sms"), channel.Credential{})
			if strings.Contains(name, "access") || strings.Contains(name, "paused") {
				_, err = s.PlatformSender().Send(context.Background(), msg("email"), channel.Credential{})
			}
		} else {
			_, err = s.BYOSender().Send(context.Background(), msg("email"), channel.Credential{Config: map[string]any{
				"role_arn": "arn:aws:iam::123456789012:role/r", "external_id": "nfx-1", "region": "ap-south-1", "from_email": "a@acme.test"}})
		}
		se := classify(t, err)
		assert.Equal(t, c.code, se.Code, name)
		assert.Equal(t, c.retryable, se.Retryable, name)
		assert.NotContains(t, se.Message, "user@example.com", "%s: AWS error text is not kept", name)
	}
}
