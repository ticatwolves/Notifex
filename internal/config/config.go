// Package config loads process configuration from the environment (12-factor).
// Secrets are injected as env vars from Secrets Manager/SSM in AWS and from a
// local .env in development; they are never read from files in the repo.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
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

	AWSRegion string `env:"AWS_REGION" envDefault:"ap-south-1"`
	// SQSEndpoint overrides the SQS endpoint (ElasticMQ locally). Empty in AWS.
	SQSEndpoint string `env:"SQS_ENDPOINT"`

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
	LocalDevAPIKeyPepper   = "bG9jYWwtZGV2LW9ubHktcGVwcGVyLU5PVC1BLVNFQ1JFVA==" //nolint:gosec // gitleaks:allow — public dev value, see above
	LocalDevLocalMasterKey = "bG9jYWwtZGV2LW9ubHktbWFzdGVyLWtleS1OT1RTRUM="     // gitleaks:allow — public dev value, see above
)

// Load parses the environment into a Config for the named service.
func Load(service, version string) (Config, error) {
	cfg, err := env.ParseAs[Config]()
	if err != nil {
		return Config{}, fmt.Errorf("parse env: %w", err)
	}
	cfg.Service = service
	cfg.Version = version
	if !cfg.Deployed() {
		if cfg.APIKeyPepper == "" {
			cfg.APIKeyPepper = LocalDevAPIKeyPepper
		}
		if cfg.LocalMasterKey == "" {
			cfg.LocalMasterKey = LocalDevLocalMasterKey
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
	if c.Deployed() && c.LocalMasterKey != "" {
		errs = append(errs, errors.New("NOTIFEX_LOCAL_MASTER_KEY must not be set outside local (use KMS)"))
	}
	if c.Deployed() && c.SQSEndpoint != "" {
		errs = append(errs, errors.New("SQS_ENDPOINT must be empty outside local (ElasticMQ is local-only)"))
	}
	return errors.Join(errs...)
}

// Deployed reports whether this process runs in a shared AWS environment.
func (c Config) Deployed() bool { return c.Env != EnvLocal }
