package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/ticatwolves/notifex/internal/store"
)

var (
	// ErrUnauthenticated is the single error for every credential failure, so
	// callers learn nothing about which check failed.
	ErrUnauthenticated = errors.New("auth: unauthenticated")
	// ErrUnavailable means authentication could not be decided (e.g. database
	// down). Transports map it to 503, never 401, so clients retry.
	ErrUnavailable = errors.New("auth: unavailable")
)

// Principal is an authenticated API key caller.
type Principal struct {
	TenantID      store.TenantID
	ApplicationID uuid.UUID
	EnvironmentID uuid.UUID
	Environment   store.EnvironmentName
	APIKeyID      uuid.UUID
	APIKeyPrefix  string
	Scopes        []string
}

// APIKeyAuthenticator resolves API keys to principals.
type APIKeyAuthenticator struct {
	store  store.Store
	hasher *Hasher
	log    *slog.Logger
	now    func() time.Time
	// dummyHash is compared against when the prefix is unknown, so response
	// time does not reveal whether a prefix exists.
	dummyHash []byte
	results   *prometheus.CounterVec
}

// NewAPIKeyAuthenticator builds an authenticator. reg may be nil.
func NewAPIKeyAuthenticator(st store.Store, h *Hasher, log *slog.Logger, reg prometheus.Registerer) *APIKeyAuthenticator {
	results := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "notifex_auth_api_key_total",
		Help: "API key authentication attempts by result (bounded set of reasons).",
	}, []string{"result"})
	if reg != nil {
		reg.MustRegister(results)
	}
	return &APIKeyAuthenticator{
		store: st, hasher: h, log: log, now: time.Now,
		dummyHash: h.Hash(ParsedKey{Prefix: "000000000000", secret: "dummy"}),
		results:   results,
	}
}

// SetClock overrides the clock (tests).
func (a *APIKeyAuthenticator) SetClock(now func() time.Time) { a.now = now }

// Authenticate verifies raw and returns its principal. It returns
// ErrUnauthenticated for any credential problem and ErrUnavailable when the
// answer can't be determined.
func (a *APIKeyAuthenticator) Authenticate(ctx context.Context, raw string) (Principal, error) {
	k, err := Parse(raw)
	if err != nil {
		return a.deny(ctx, "malformed")
	}
	key, err := a.store.APIKeys().GetByPrefix(ctx, k.Prefix)
	switch {
	case errors.Is(err, store.ErrNotFound):
		a.hasher.Verify(k, a.dummyHash) // equalise timing with the found path
		return a.deny(ctx, "unknown_prefix")
	case err != nil:
		return a.unavailable(ctx, err)
	}
	if !a.hasher.Verify(k, key.Hash) {
		return a.deny(ctx, "bad_secret")
	}
	now := a.now()
	if key.RevokedAt != nil && !key.RevokedAt.After(now) {
		return a.deny(ctx, "revoked")
	}
	if key.ExpiresAt != nil && !key.ExpiresAt.After(now) {
		return a.deny(ctx, "expired")
	}

	env, err := a.store.Environments().Get(ctx, key.TenantID, key.EnvironmentID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return a.deny(ctx, "environment_missing")
		}
		return a.unavailable(ctx, err)
	}
	if string(env.Name) != k.Env {
		return a.deny(ctx, "environment_mismatch") // key text says prod, record says dev
	}
	tenant, err := a.store.Tenants().Get(ctx, key.TenantID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return a.deny(ctx, "tenant_missing")
		}
		return a.unavailable(ctx, err)
	}
	if tenant.Status != store.TenantActive {
		return a.deny(ctx, "tenant_inactive")
	}

	// Best effort and throttled in the store; never fails the request.
	if err := a.store.APIKeys().TouchLastUsed(ctx, key.TenantID, key.ID, now); err != nil {
		a.log.WarnContext(ctx, "api key last_used_at update failed", slog.Any("error", err))
	}
	a.results.WithLabelValues("ok").Inc()
	return Principal{
		TenantID: key.TenantID, ApplicationID: key.ApplicationID,
		EnvironmentID: key.EnvironmentID, Environment: env.Name,
		APIKeyID: key.ID, APIKeyPrefix: key.Prefix, Scopes: key.Scopes,
	}, nil
}

func (a *APIKeyAuthenticator) deny(ctx context.Context, reason string) (Principal, error) {
	a.results.WithLabelValues(reason).Inc()
	// Never log the key; the reason is internal only.
	a.log.InfoContext(ctx, "api key rejected", slog.String("reason", reason))
	return Principal{}, ErrUnauthenticated
}

func (a *APIKeyAuthenticator) unavailable(ctx context.Context, err error) (Principal, error) {
	a.results.WithLabelValues("error").Inc()
	a.log.ErrorContext(ctx, "api key authentication failed", slog.Any("error", err))
	return Principal{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
}
