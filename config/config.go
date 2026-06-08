package config

import (
	"time"

	"github.com/caarlos0/env/v11"
)

type Config struct {
	// Server
	Port         string        `env:"PORT"          envDefault:"5000"`
	ReadTimeout  time.Duration `env:"READ_TIMEOUT"  envDefault:"10s"`
	WriteTimeout time.Duration `env:"WRITE_TIMEOUT" envDefault:"30s"`
	GracePeriod  time.Duration `env:"GRACE_PERIOD"  envDefault:"30s"`
	Environment  string        `env:"APP_ENV"       envDefault:"production"`

	// Database
	DatabaseURL     string        `env:"DATABASE_URL"      required:"true"`
	MaxOpenConns    int           `env:"DB_MAX_OPEN_CONNS" envDefault:"25"`
	MaxIdleConns    int           `env:"DB_MAX_IDLE_CONNS" envDefault:"5"`
	ConnMaxLifetime time.Duration `env:"DB_CONN_MAX_LIFE"  envDefault:"5m"`

	// Redis
	RedisURL string `env:"REDIS_URL" envDefault:"redis://localhost:6379"`

	// Auth
	JWTSecret string        `env:"JWT_SECRET"      required:"true"`
	JWTExpiry time.Duration `env:"JWT_EXPIRY"      envDefault:"24h"`

	// Observability
	LogLevel string `env:"LOG_LEVEL"       envDefault:"info"`
}

func Load() (*Config, error) {
	cfg := &Config{}
	return cfg, env.Parse(cfg)
}
