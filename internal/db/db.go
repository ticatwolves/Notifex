package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// PoolConfig bounds the connection pool. Defaults suit one Fargate task behind
// PgBouncer-free RDS at Stage A; tune with load tests (docs/07).
type PoolConfig struct {
	MaxOpen     int
	MaxIdle     int
	MaxLifetime time.Duration
	MaxIdleTime time.Duration
}

// DefaultPool is used when fields are zero.
var DefaultPool = PoolConfig{MaxOpen: 20, MaxIdle: 10, MaxLifetime: 30 * time.Minute, MaxIdleTime: 5 * time.Minute}

// Open returns a *sql.DB using pgx and verifies connectivity.
func Open(ctx context.Context, databaseURL string, pc PoolConfig) (*sql.DB, error) {
	cfg, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL (%s): %w", redactURL(databaseURL), err)
	}
	db := stdlib.OpenDB(*cfg)
	if pc == (PoolConfig{}) {
		pc = DefaultPool
	}
	db.SetMaxOpenConns(pc.MaxOpen)
	db.SetMaxIdleConns(pc.MaxIdle)
	db.SetConnMaxLifetime(pc.MaxLifetime)
	db.SetConnMaxIdleTime(pc.MaxIdleTime)

	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping postgres (%s): %w", redactURL(databaseURL), err)
	}
	return db, nil
}
