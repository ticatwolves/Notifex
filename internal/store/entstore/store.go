// Package entstore implements store.Store on ent + PostgreSQL.
package entstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sbshobhit/notifex/ent"
	_ "github.com/sbshobhit/notifex/ent/runtime" // registers schema hooks, interceptors and defaults
	"github.com/sbshobhit/notifex/internal/store"
	"github.com/sbshobhit/notifex/internal/store/tenantscope"
)

// Store implements store.Store.
type Store struct {
	client *ent.Client
	inTx   bool
}

var _ store.Store = (*Store)(nil)

// New wraps an open *sql.DB (pgx). The caller owns db's lifecycle.
func New(db *sql.DB) *Store {
	return &Store{client: ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))}
}

// Tenants implements store.Store.
func (s *Store) Tenants() store.TenantStore { return tenants{s.client} }

// Users implements store.Store.
func (s *Store) Users() store.UserStore { return users{s.client} }

// Memberships implements store.Store.
func (s *Store) Memberships() store.MembershipStore { return memberships{s.client} }

// Applications implements store.Store.
func (s *Store) Applications() store.ApplicationStore { return applications{s.client} }

// Environments implements store.Store.
func (s *Store) Environments() store.EnvironmentStore { return environments{s.client} }

// APIKeys implements store.Store.
func (s *Store) APIKeys() store.APIKeyStore { return apiKeys{s.client} }

// Audit implements store.Store.
func (s *Store) Audit() store.AuditStore { return audit{s.client} }

// WithTx implements store.Store. Nested calls join the outer transaction.
func (s *Store) WithTx(ctx context.Context, fn func(ctx context.Context, tx store.Store) error) (err error) {
	if s.inTx {
		return fn(ctx, s)
	}
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(ctx, &Store{client: tx.Client(), inTx: true}); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			return errors.Join(err, fmt.Errorf("rollback: %w", rbErr))
		}
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", mapErr(err, opWrite))
	}
	return nil
}

// scoped returns ctx carrying the tenant scope enforced by the ent hooks.
func scoped(ctx context.Context, tid store.TenantID) context.Context {
	return tenantscope.With(ctx, tid.UUID())
}

type op int

const (
	opRead op = iota
	opCreate
	opWrite
	opDelete
)

// Postgres SQLSTATE codes we translate.
const (
	pgUniqueViolation     = "23505"
	pgForeignKeyViolation = "23503"
	pgRestrictViolation   = "23001" // ON DELETE RESTRICT
	pgCheckViolation      = "23514"
	pgNotNullViolation    = "23502"
)

// mapErr translates ent/pg errors into store sentinels, keeping the original
// for diagnostics. pgconn.PgError.Error() omits DETAIL, so key values (which
// may be PII such as emails) never end up in error strings.
func mapErr(err error, o op) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, tenantscope.ErrMissing) {
		return err
	}
	if ent.IsNotFound(err) {
		return fmt.Errorf("%w: %w", store.ErrNotFound, err)
	}
	if ent.IsValidationError(err) {
		return fmt.Errorf("%w: %w", store.ErrInvalid, err)
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case pgUniqueViolation:
			return fmt.Errorf("%w: %w", store.ErrConflict, err)
		case pgRestrictViolation:
			return fmt.Errorf("%w: still referenced: %w", store.ErrConflict, err)
		case pgForeignKeyViolation:
			if o == opDelete {
				return fmt.Errorf("%w: still referenced: %w", store.ErrConflict, err)
			}
			// Referenced parent is missing in this tenant: report not found so
			// another tenant's ids are indistinguishable from unknown ids.
			return fmt.Errorf("%w: %w", store.ErrNotFound, err)
		case pgCheckViolation, pgNotNullViolation:
			return fmt.Errorf("%w: %w", store.ErrInvalid, err)
		}
	}
	return err
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{store.ErrInvalid}, args...)...)
}

// slugRE matches DNS-label-style slugs used for tenant slugs and app keys.
var slugRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
