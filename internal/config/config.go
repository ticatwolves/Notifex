// Package config loads process configuration from the environment (12-factor).
// Secrets are injected as env vars from Secrets Manager/SSM in AWS and from a
// local .env in development; they are never read from files in the repo.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/mail"
	"slices"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

// Environment names accepted in NOTIFEX_ENV.
const (
	EnvLocal   = "local"
	EnvDev     = "dev"
	EnvStaging = "staging"
	EnvProd    = "prod"
)

// Config is shared by every Notifex Go binary. Service-specific fields are
// ignored by services that do not use them.
type Config struct {
	// Service is set by the binary (api, dispatcher, scheduler), not by env.
	Service string `env:"-"`
	// Version is the build version, injected via -ldflags.
	Version string `env:"-"`

	Env      string     `env:"NOTIFEX_ENV" envDefault:"local"`
	LogLevel slog.Level `env:"LOG_LEVEL" envDefault:"info"`
	// LogFormat is "json" (default, all deployed envs) or "text" (local readability).
	LogFormat string `env:"LOG_FORMAT" envDefault:"json"`

	// HTTPAddr is the public REST listener (api only).
	HTTPAddr string `env:"HTTP_ADDR" envDefault:":8080"`
	// GRPCAddr is the public gRPC listener (api only).
	GRPCAddr string `env:"GRPC_ADDR" envDefault:":9090"`
	// OpsAddr serves /metrics, /healthz and /readyz on every service. It is never
	// exposed through the load balancer.
	OpsAddr string `env:"OPS_ADDR" envDefault:":9100"`

	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" envDefault:"20s"`

	// DatabaseURL is the Postgres DSN (required by api, migrate, seed).
	DatabaseURL string `env:"DATABASE_URL"`

	// APIKeyPepper is the base64 HMAC key for stored API key hashes (>= 32
	// bytes). Secrets Manager in AWS. Rotating it invalidates every key.
	APIKeyPepper string `env:"API_KEY_PEPPER"`
	// LocalMasterKey is the base64 32-byte master key for the local
	// KeyProvider. Local only; deployed environments use KMS.
	LocalMasterKey string `env:"NOTIFEX_LOCAL_MASTER_KEY"`
	// RecipientHashKey is the base64 HMAC key (>= 32 bytes) for
	// notifications.recipient_hash (ADR-014 §4). Separate from the API key
	// pepper so the two rotate independently.
	RecipientHashKey string `env:"RECIPIENT_HASH_KEY"`

	// PlatformSMSDailyCap is the platform-wide daily platform-SMS cap, set
	// below the AWS account's SMS spend quota (ADR-005).
	PlatformSMSDailyCap int `env:"PLATFORM_SMS_DAILY_CAP" envDefault:"10000"`
	// TriggerRatePerMinute is the per-API-key trigger limit per process
	// (ADR-014 §8: effective limit is this times the number of tasks).
	TriggerRatePerMinute int `env:"TRIGGER_RATE_PER_MINUTE" envDefault:"6000"`

	// ConsoleURL is the web console's base URL, used in emailed links.
	ConsoleURL string `env:"CONSOLE_BASE_URL" envDefault:"http://localhost:3000"`
	// SMTPAddr is the system-mail relay (Mailpit locally). Empty disables
	// sending outside local; in local, mail is then only logged as dropped.
	SMTPAddr string `env:"SMTP_ADDR"`
	MailFrom string `env:"MAIL_FROM" envDefault:"Notifex <noreply@notifex.local>"`
	// PasswordHashConcurrency bounds concurrent argon2id hashes (~46 MiB each).
	PasswordHashConcurrency int `env:"PASSWORD_HASH_CONCURRENCY" envDefault:"4"`
	// OAuth clients (ADR-011 §3). A provider is enabled when its client id is
	// set. Secrets come from Secrets Manager in AWS.
	GoogleClientID        string `env:"OAUTH_GOOGLE_CLIENT_ID"`
	GoogleClientSecret    string `env:"OAUTH_GOOGLE_CLIENT_SECRET"`
	MicrosoftClientID     string `env:"OAUTH_MICROSOFT_CLIENT_ID"`
	MicrosoftClientSecret string `env:"OAUTH_MICROSOFT_CLIENT_SECRET"`
	GitHubClientID        string `env:"OAUTH_GITHUB_CLIENT_ID"`
	GitHubClientSecret    string `env:"OAUTH_GITHUB_CLIENT_SECRET"`
	// OAuthRedirectURIs is the exact-match allowlist of console callback URLs.
	OAuthRedirectURIs []string `env:"OAUTH_REDIRECT_URIS" envSeparator:","`

	// TrustedProxies are CIDRs whose X-Forwarded-For is believed (the ALB).
	TrustedProxies []string `env:"TRUSTED_PROXIES" envSeparator:","`

	AWSRegion string `env:"AWS_REGION" envDefault:"ap-south-1"`
	// SQSEndpoint overrides the SQS endpoint (ElasticMQ locally). Empty in AWS.
	SQSEndpoint string `env:"SQS_ENDPOINT"`
	// QueuePrefix names the dispatch queues: <prefix><channel> (ADR-004).
	// Default notifex-<env>-dispatch-.
	QueuePrefix string `env:"QUEUE_PREFIX"`
	// SQSFairQueues sets MessageGroupId = tenant_id (SQS fair queues).
	SQSFairQueues bool `env:"SQS_FAIR_QUEUES" envDefault:"true"`
	// DispatchEnabled turns the dispatcher's consumers on (dark launch).
	DispatchEnabled bool `env:"DISPATCH_ENABLED" envDefault:"false"`
	// DispatchVendors picks the senders: "mock" (record only; local
	// default, refused outside local) or "live" (real vendors; default
	// when deployed).
	DispatchVendors string `env:"DISPATCH_VENDORS"`
	// PlatformEmailFrom is the platform email sender (a verified SES
	// identity). Required for a deployed dispatcher with dispatch enabled.
	PlatformEmailFrom string `env:"PLATFORM_EMAIL_FROM"`
	// SESConfigurationSet is attached to every SES send (optional).
	SESConfigurationSet string `env:"SES_CONFIGURATION_SET"`
	// SMTPEHLOName is the EHLO name for tenant SMTP servers.
	SMTPEHLOName string `env:"SMTP_EHLO_NAME" envDefault:"notifex"`
	// DispatchConcurrency is the in-flight handlers per channel per task.
	DispatchConcurrency int `env:"DISPATCH_CONCURRENCY" envDefault:"10"`

	// Tracing: OTEL_EXPORTER_OTLP_ENDPOINT and friends are read by the OTel SDK
	// directly. We only read it here to decide whether to install an exporter.
	OTLPEndpoint string `env:"OTEL_EXPORTER_OTLP_ENDPOINT"`
	// TraceSampleRatio is the parent-based sampling ratio for root spans.
	TraceSampleRatio float64 `env:"OTEL_TRACES_SAMPLER_RATIO" envDefault:"1.0"`
}

// Well-known local development values, used only when NOTIFEX_ENV=local and
// the variable is unset. They decode to "local-dev-only-…-NOT-A-SECRET" text,
// are deliberately public, and are rejected outside local.
const (
	LocalDevAPIKeyPepper     = "bG9jYWwtZGV2LW9ubHktcGVwcGVyLU5PVC1BLVNFQ1JFVA=="         //nolint:gosec // gitleaks:allow — public dev value, see above
	LocalDevLocalMasterKey   = "bG9jYWwtZGV2LW9ubHktbWFzdGVyLWtleS1OT1RTRUM="             // gitleaks:allow — public dev value, see above
	LocalDevRecipientHashKey = "bG9jYWwtZGV2LW9ubHktcmVjaXBpZW50LWhhc2gtTk9ULUEtU0VDUkVU" // gitleaks:allow — public dev value, see above
)

// Load parses the environment into a Config for the named service.
func Load(service, version string) (Config, error) {
	cfg, err := env.ParseAs[Config]()
	if err != nil {
		return Config{}, fmt.Errorf("parse env: %w", err)
	}
	cfg.Service = service
	cfg.Version = version
	if cfg.DispatchVendors == "" {
		cfg.DispatchVendors = "live"
		if !cfg.Deployed() {
			cfg.DispatchVendors = "mock"
		}
	}
	if cfg.QueuePrefix == "" {
		cfg.QueuePrefix = "notifex-" + cfg.Env + "-dispatch-"
	}
	if !cfg.Deployed() {
		if cfg.APIKeyPepper == "" {
			cfg.APIKeyPepper = LocalDevAPIKeyPepper
		}
		if cfg.LocalMasterKey == "" {
			cfg.LocalMasterKey = LocalDevLocalMasterKey
		}
		if cfg.RecipientHashKey == "" {
			cfg.RecipientHashKey = LocalDevRecipientHashKey
		}
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate checks invariants that env tags cannot express.
func (c Config) Validate() error {
	var errs []error
	if !slices.Contains([]string{EnvLocal, EnvDev, EnvStaging, EnvProd}, c.Env) {
		errs = append(errs, fmt.Errorf("NOTIFEX_ENV %q: must be one of local, dev, staging, prod", c.Env))
	}
	if c.LogFormat != "json" && c.LogFormat != "text" {
		errs = append(errs, fmt.Errorf("LOG_FORMAT %q: must be json or text", c.LogFormat))
	}
	if c.ShutdownTimeout <= 0 {
		errs = append(errs, errors.New("SHUTDOWN_TIMEOUT must be positive"))
	}
	if c.TraceSampleRatio < 0 || c.TraceSampleRatio > 1 {
		errs = append(errs, fmt.Errorf("OTEL_TRACES_SAMPLER_RATIO %v: must be within [0, 1]", c.TraceSampleRatio))
	}
	if c.Deployed() && c.APIKeyPepper == LocalDevAPIKeyPepper {
		errs = append(errs, errors.New("API_KEY_PEPPER is the public local-dev value; set a real secret"))
	}
	if c.Deployed() && c.RecipientHashKey == LocalDevRecipientHashKey {
		errs = append(errs, errors.New("RECIPIENT_HASH_KEY is the public local-dev value; set a real secret"))
	}
	if c.PlatformSMSDailyCap < 0 {
		errs = append(errs, errors.New("PLATFORM_SMS_DAILY_CAP must be >= 0"))
	}
	if c.TriggerRatePerMinute < 1 {
		errs = append(errs, errors.New("TRIGGER_RATE_PER_MINUTE must be >= 1"))
	}
	if c.Deployed() && c.LocalMasterKey != "" {
		errs = append(errs, errors.New("NOTIFEX_LOCAL_MASTER_KEY must not be set outside local (use KMS)"))
	}
	if c.DispatchConcurrency < 1 || c.DispatchConcurrency > 500 {
		errs = append(errs, fmt.Errorf("DISPATCH_CONCURRENCY %d: must be within [1, 500]", c.DispatchConcurrency))
	}
	switch c.DispatchVendors {
	case "", "live":
	case "mock":
		if c.Deployed() {
			errs = append(errs, errors.New("DISPATCH_VENDORS=mock is local-only"))
		}
	default:
		errs = append(errs, fmt.Errorf("DISPATCH_VENDORS %q: must be mock or live", c.DispatchVendors))
	}
	if c.PlatformEmailFrom != "" {
		if a, err := mail.ParseAddress(c.PlatformEmailFrom); err != nil || a.Address != c.PlatformEmailFrom {
			errs = append(errs, errors.New("PLATFORM_EMAIL_FROM must be a plain email address"))
		}
	}
	if c.Deployed() && c.Service == "dispatcher" && c.DispatchEnabled && c.PlatformEmailFrom == "" {
		errs = append(errs, errors.New("PLATFORM_EMAIL_FROM is required for a deployed dispatcher"))
	}
	if c.PasswordHashConcurrency < 1 || c.PasswordHashConcurrency > 64 {
		errs = append(errs, fmt.Errorf("PASSWORD_HASH_CONCURRENCY %d: must be within [1, 64]", c.PasswordHashConcurrency))
	}
	for _, pair := range [][3]string{
		{"OAUTH_GOOGLE", c.GoogleClientID, c.GoogleClientSecret},
		{"OAUTH_MICROSOFT", c.MicrosoftClientID, c.MicrosoftClientSecret},
		{"OAUTH_GITHUB", c.GitHubClientID, c.GitHubClientSecret},
	} {
		if (pair[1] == "") != (pair[2] == "") {
			errs = append(errs, fmt.Errorf("%s_CLIENT_ID and %s_CLIENT_SECRET must be set together", pair[0], pair[0]))
		}
	}
	if (c.GoogleClientID != "" || c.MicrosoftClientID != "" || c.GitHubClientID != "") && len(c.OAuthRedirectURIs) == 0 {
		errs = append(errs, errors.New("OAUTH_REDIRECT_URIS is required when an OAuth provider is enabled"))
	}
	for _, cidr := range c.TrustedProxies {
		if _, _, err := net.ParseCIDR(strings.TrimSpace(cidr)); err != nil {
			errs = append(errs, fmt.Errorf("TRUSTED_PROXIES %q: not a CIDR", cidr))
		}
	}
	if c.Deployed() && c.SQSEndpoint != "" {
		errs = append(errs, errors.New("SQS_ENDPOINT must be empty outside local (ElasticMQ is local-only)"))
	}
	return errors.Join(errs...)
}

// Deployed reports whether this process runs in a shared AWS environment.
func (c Config) Deployed() bool { return c.Env != EnvLocal }
