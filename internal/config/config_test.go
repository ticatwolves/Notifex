package config

import (
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load("api", "test")
	require.NoError(t, err)
	assert.Equal(t, "api", cfg.Service)
	assert.Equal(t, EnvLocal, cfg.Env)
	assert.Equal(t, slog.LevelInfo, cfg.LogLevel)
	assert.Equal(t, ":8080", cfg.HTTPAddr)
	assert.Equal(t, ":9100", cfg.OpsAddr)
	assert.Equal(t, 20*time.Second, cfg.ShutdownTimeout)
	assert.False(t, cfg.Deployed())
	assert.Equal(t, LocalDevAPIKeyPepper, cfg.APIKeyPepper, "local falls back to the public dev pepper")
	assert.Equal(t, LocalDevLocalMasterKey, cfg.LocalMasterKey)
}

func TestNoDevSecretFallbackOutsideLocal(t *testing.T) {
	t.Setenv("NOTIFEX_ENV", "staging")
	cfg, err := Load("api", "test")
	require.NoError(t, err)
	assert.Empty(t, cfg.APIKeyPepper, "deployed envs never get the dev pepper; api startup then fails on the empty value")
	assert.Empty(t, cfg.LocalMasterKey)
}

func TestLoadFromEnv(t *testing.T) {
	t.Setenv("NOTIFEX_ENV", "staging")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("SHUTDOWN_TIMEOUT", "5s")
	cfg, err := Load("dispatcher", "v1")
	require.NoError(t, err)
	assert.Equal(t, EnvStaging, cfg.Env)
	assert.Equal(t, slog.LevelDebug, cfg.LogLevel)
	assert.Equal(t, 5*time.Second, cfg.ShutdownTimeout)
	assert.True(t, cfg.Deployed())
}

func TestValidate(t *testing.T) {
	valid := Config{Env: EnvLocal, LogFormat: "json", ShutdownTimeout: time.Second, TraceSampleRatio: 1, PasswordHashConcurrency: 4}
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{name: "valid", mutate: func(*Config) {}},
		{name: "unknown env", mutate: func(c *Config) { c.Env = "production" }, wantErr: "NOTIFEX_ENV"},
		{name: "bad log format", mutate: func(c *Config) { c.LogFormat = "xml" }, wantErr: "LOG_FORMAT"},
		{name: "zero shutdown", mutate: func(c *Config) { c.ShutdownTimeout = 0 }, wantErr: "SHUTDOWN_TIMEOUT"},
		{name: "sample ratio > 1", mutate: func(c *Config) { c.TraceSampleRatio = 1.5 }, wantErr: "SAMPLER_RATIO"},
		{name: "sqs endpoint in prod", mutate: func(c *Config) { c.Env = EnvProd; c.SQSEndpoint = "http://x" }, wantErr: "SQS_ENDPOINT"},
		{name: "local pepper in staging", mutate: func(c *Config) { c.Env = EnvStaging; c.APIKeyPepper = LocalDevAPIKeyPepper }, wantErr: "API_KEY_PEPPER"},
		{name: "local pepper locally ok", mutate: func(c *Config) { c.APIKeyPepper = LocalDevAPIKeyPepper }},
		{name: "local master key in prod", mutate: func(c *Config) { c.Env = EnvProd; c.LocalMasterKey = LocalDevLocalMasterKey }, wantErr: "NOTIFEX_LOCAL_MASTER_KEY"},
		{name: "hash concurrency 0", mutate: func(c *Config) { c.PasswordHashConcurrency = 0 }, wantErr: "PASSWORD_HASH_CONCURRENCY"},
		{name: "bad trusted proxy", mutate: func(c *Config) { c.TrustedProxies = []string{"10.0.0.0"} }, wantErr: "TRUSTED_PROXIES"},
		{name: "trusted proxy cidr ok", mutate: func(c *Config) { c.TrustedProxies = []string{"10.0.0.0/16"} }},
		{name: "sqs endpoint local ok", mutate: func(c *Config) { c.SQSEndpoint = "http://elasticmq:9324" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := valid
			tt.mutate(&c)
			err := c.Validate()
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}
