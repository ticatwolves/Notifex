package operator

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ticatwolves/notifex/internal/audit"
	"github.com/ticatwolves/notifex/internal/auth"
	"github.com/ticatwolves/notifex/internal/crypto"
	"github.com/ticatwolves/notifex/internal/observability"
	"github.com/ticatwolves/notifex/internal/store"
)

// ErrReasonRequired means an operator change was submitted without a reason.
var ErrReasonRequired = errors.New("operator: a reason is required for tenant changes")

// ErrNothingToUpdate means a tenant patch changes nothing.
var ErrNothingToUpdate = errors.New("operator: nothing to update")

// Policy holds ADR-011 §5 lifetimes and limits.
type Policy struct {
	Idle          time.Duration
	Absolute      time.Duration
	TouchInterval time.Duration
	Login         store.ThrottlePolicy
}

// DefaultPolicy is ADR-011 §5.
var DefaultPolicy = Policy{
	Idle: 30 * time.Minute, Absolute: 8 * time.Hour, TouchInterval: time.Minute,
	Login: store.ThrottlePolicy{FreeFailures: 5, Window: 15 * time.Minute, MaxBackoff: 15 * time.Minute},
}

// Principal is an authenticated operator.
type Principal struct {
	OperatorID uuid.UUID
	SessionID  uuid.UUID
	Email      string
}

// Service implements the operator realm.
type Service struct {
	st     store.Store
	keys   crypto.KeyProvider
	hasher *auth.PasswordHasher
	log    *slog.Logger
	policy Policy
	now    func() time.Time
}

// NewService builds the operator service.
func NewService(st store.Store, keys crypto.KeyProvider, h *auth.PasswordHasher, log *slog.Logger, p Policy) *Service {
	return &Service{st: st, keys: keys, hasher: h, log: log, policy: p, now: time.Now}
}

// SetClock overrides the clock (tests).
func (s *Service) SetClock(now func() time.Time) { s.now = now }

func totpContext(id uuid.UUID) crypto.EncryptionContext {
	return crypto.EncryptionContext{"purpose": "operator-totp", "operator_id": id.String()}
}

func actorOf(id uuid.UUID) audit.Actor {
	return audit.Actor{Type: store.ActorOperator, ID: id.String()}
}

// Create provisions an operator (CLI only) and returns the TOTP enrollment URI,
// which contains the secret and must be shown exactly once.
func (s *Service) Create(ctx context.Context, email, name, password string) (store.Operator, string, error) {
	addr, err := mail.ParseAddress(strings.TrimSpace(email))
	if err != nil || addr.Address != strings.TrimSpace(email) {
		return store.Operator{}, "", fmt.Errorf("%w: email", store.ErrInvalid)
	}
	email = strings.ToLower(addr.Address)
	if err := auth.ValidatePassword(password, email); err != nil {
		return store.Operator{}, "", fmt.Errorf("create operator: %w", err)
	}
	hash, err := s.hasher.Hash(ctx, password)
	if err != nil {
		return store.Operator{}, "", fmt.Errorf("create operator: %w", err)
	}
	id := uuid.Must(uuid.NewV7())
	secret := newTOTPSecret()
	defer clear(secret)
	sealed, err := crypto.Seal(ctx, s.keys, totpContext(id), secret)
	if err != nil {
		return store.Operator{}, "", fmt.Errorf("create operator: %w", err)
	}
	var op store.Operator
	err = s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		var err error
		op, err = tx.Operators().Create(ctx, store.NewOperator{
			ID: id, Email: email, Name: name, PasswordHash: hash,
			TOTPWrappedKey: sealed.WrappedKey, TOTPKeyID: sealed.KeyID, TOTPCiphertext: sealed.Ciphertext,
		})
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.System, audit.Event{
			Action: "operator.create", EntityType: "operator", EntityID: id.String(),
			After: map[string]any{"email": observability.MaskEmail(email), "via": "cli"},
		})
	})
	if err != nil {
		return store.Operator{}, "", fmt.Errorf("create operator: %w", err)
	}
	return op, otpauthURI(secret, email), nil
}

// Disable blocks an operator and revokes their sessions at once (CLI).
func (s *Service) Disable(ctx context.Context, email string) error {
	op, err := s.st.Operators().GetByEmail(ctx, email)
	if err != nil {
		return fmt.Errorf("disable operator: %w", err)
	}
	return wrap("disable operator", s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.Operators().SetStatus(ctx, op.ID, store.OperatorDisabled); err != nil {
			return err
		}
		n, err := tx.OperatorSessions().RevokeAllForOperator(ctx, op.ID, s.now())
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.System, audit.Event{
			Action: "operator.disable", EntityType: "operator", EntityID: op.ID.String(),
			After: map[string]any{"sessions_revoked": n, "via": "cli"},
		})
	}))
}

// Login checks password and TOTP together. Every failure is
// auth.ErrInvalidCredentials (which factor failed is never revealed);
// repeated failures return *auth.LockedError. A TOTP code is accepted once.
func (s *Service) Login(ctx context.Context, email, password, code, userAgent string) (string, store.Session, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	key := "operator-login:" + sha256Hex(email)
	now := s.now()
	if until, err := s.st.Throttle().BlockedUntil(ctx, key, now); err != nil {
		return "", store.Session{}, fmt.Errorf("operator login: %w", err)
	} else if !until.IsZero() {
		return "", store.Session{}, &auth.LockedError{RetryAfter: until.Sub(now)}
	}
	fail := func(opID *uuid.UUID) error {
		if _, err := s.st.Throttle().RecordFailure(ctx, key, now, s.policy.Login); err != nil {
			return fmt.Errorf("operator login: %w", err)
		}
		entity := observability.MaskEmail(email)
		if opID != nil {
			entity = opID.String()
		}
		if err := audit.Record(ctx, s.st, audit.System, audit.Event{Action: "operator.login_failed", EntityType: "operator", EntityID: entity}); err != nil {
			s.log.WarnContext(ctx, "audit operator login failure", slog.Any("error", err))
		}
		return auth.ErrInvalidCredentials
	}

	op, err := s.st.Operators().GetByEmail(ctx, email)
	if errors.Is(err, store.ErrNotFound) {
		s.hasher.VerifyDummy(ctx, password)
		return "", store.Session{}, fail(nil)
	}
	if err != nil {
		return "", store.Session{}, fmt.Errorf("operator login: %w", err)
	}
	ok, _, err := s.hasher.Verify(ctx, op.PasswordHash, password)
	if err != nil {
		return "", store.Session{}, fmt.Errorf("operator login: %w", err)
	}
	if !ok || op.Status != store.OperatorActive {
		return "", store.Session{}, fail(&op.ID)
	}
	secret, err := crypto.Open(ctx, s.keys, totpContext(op.ID), crypto.Sealed{
		WrappedKey: op.TOTPWrappedKey, KeyID: op.TOTPKeyID, Ciphertext: op.TOTPCiphertext,
	})
	if err != nil {
		return "", store.Session{}, fmt.Errorf("operator login: totp secret: %w", err)
	}
	step := matchStep(secret, strings.TrimSpace(code), now)
	clear(secret)
	if step < 0 {
		return "", store.Session{}, fail(&op.ID)
	}
	if fresh, err := s.st.Operators().AdvanceTOTPStep(ctx, op.ID, step); err != nil {
		return "", store.Session{}, fmt.Errorf("operator login: %w", err)
	} else if !fresh {
		return "", store.Session{}, fail(&op.ID) // replayed (or older) code
	}

	token, hash := newOperatorToken()
	var sess store.Session
	err = s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.Throttle().Reset(ctx, key); err != nil {
			return err
		}
		var err error
		sess, err = tx.OperatorSessions().Create(ctx, store.NewSession{
			UserID: op.ID, TokenHash: hash, Now: now, AbsoluteExpiresAt: now.Add(s.policy.Absolute), UserAgent: userAgent,
		})
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, actorOf(op.ID), audit.Event{Action: "operator.login", EntityType: "operator_session", EntityID: sess.ID.String()})
	})
	if err != nil {
		return "", store.Session{}, fmt.Errorf("operator login: %w", err)
	}
	return token, sess, nil
}

// Logout revokes the caller's operator session.
func (s *Service) Logout(ctx context.Context, p Principal) error {
	return wrap("operator logout", s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.OperatorSessions().Revoke(ctx, p.OperatorID, p.SessionID, s.now()); err != nil {
			return err
		}
		return audit.Record(ctx, tx, actorOf(p.OperatorID), audit.Event{Action: "operator.logout", EntityType: "operator_session", EntityID: p.SessionID.String()})
	}))
}

const tokenPrefix = "nfo_"

// Authenticate resolves an operator session token.
func (s *Service) Authenticate(ctx context.Context, token string) (Principal, error) {
	if !strings.HasPrefix(token, tokenPrefix) || len(token) != len(tokenPrefix)+43 {
		return Principal{}, auth.ErrUnauthenticated
	}
	sum := sha256.Sum256([]byte(token))
	sess, err := s.st.OperatorSessions().GetByTokenHash(ctx, sum[:])
	if errors.Is(err, store.ErrNotFound) {
		return Principal{}, auth.ErrUnauthenticated
	}
	if err != nil {
		return Principal{}, fmt.Errorf("%w: %w", auth.ErrUnavailable, err)
	}
	now := s.now()
	if sess.RevokedAt != nil || !now.Before(sess.AbsoluteExpiresAt) || now.Sub(sess.LastSeenAt) >= s.policy.Idle {
		return Principal{}, auth.ErrUnauthenticated
	}
	op, err := s.st.Operators().Get(ctx, sess.UserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Principal{}, auth.ErrUnauthenticated
		}
		return Principal{}, fmt.Errorf("%w: %w", auth.ErrUnavailable, err)
	}
	if op.Status != store.OperatorActive {
		return Principal{}, auth.ErrUnauthenticated
	}
	if err := s.st.OperatorSessions().Touch(ctx, sess.ID, now, s.policy.TouchInterval); err != nil {
		s.log.WarnContext(ctx, "operator session touch failed", slog.Any("error", err))
	}
	return Principal{OperatorID: op.ID, SessionID: sess.ID, Email: op.Email}, nil
}

// SearchTenants lists tenants (audited, like every operator request).
func (s *Service) SearchTenants(ctx context.Context, p Principal, q string, status *store.TenantStatus, page store.Page) ([]store.Tenant, error) {
	ts, err := s.st.Tenants().Search(ctx, q, status, page)
	if err != nil {
		return nil, fmt.Errorf("search tenants: %w", err)
	}
	st := ""
	if status != nil {
		st = string(*status)
	}
	if err := audit.Record(ctx, s.st, actorOf(p.OperatorID), audit.Event{
		Action: "operator.tenant.search", EntityType: "tenant", EntityID: "*",
		After: map[string]any{"q": q, "status": st, "results": len(ts)},
	}); err != nil {
		return nil, fmt.Errorf("search tenants: %w", err) // no audit, no data
	}
	return ts, nil
}

// GetTenant returns a tenant (audited).
func (s *Service) GetTenant(ctx context.Context, p Principal, tid store.TenantID) (store.Tenant, error) {
	t, err := s.st.Tenants().Get(ctx, tid)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return store.Tenant{}, fmt.Errorf("get tenant: %w", err)
	}
	found := err == nil
	ev := audit.Event{Action: "operator.tenant.view", EntityType: "tenant", EntityID: tid.String(), After: map[string]any{"found": found}}
	if found {
		ev.TenantID = &tid // misses stay platform-level: the id may not exist
	}
	if aerr := audit.Record(ctx, s.st, actorOf(p.OperatorID), ev); aerr != nil {
		return store.Tenant{}, fmt.Errorf("get tenant: %w", aerr)
	}
	if !found {
		return store.Tenant{}, fmt.Errorf("get tenant: %w", err)
	}
	return t, nil
}

// TenantPatch is an operator change to a tenant. Reason is required and
// recorded in the audit event.
type TenantPatch struct {
	Status                *store.TenantStatus
	Plan                  *string
	Sandbox               *bool
	PlatformSMSDailyLimit *int
	Quotas                map[string]int64
	Reason                string
}

// UpdateTenant applies an operator change. Deletion is not an operator
// action (owner-only crypto-shred), and deleted tenants can't be changed.
func (s *Service) UpdateTenant(ctx context.Context, p Principal, tid store.TenantID, patch TenantPatch) (store.Tenant, error) {
	patch.Reason = strings.TrimSpace(patch.Reason)
	if patch.Status == nil && patch.Plan == nil && patch.Sandbox == nil && patch.PlatformSMSDailyLimit == nil && patch.Quotas == nil {
		return store.Tenant{}, ErrNothingToUpdate
	}
	if patch.Reason == "" || len(patch.Reason) > 500 {
		return store.Tenant{}, ErrReasonRequired
	}
	if patch.Status != nil && *patch.Status != store.TenantActive && *patch.Status != store.TenantSuspended {
		return store.Tenant{}, fmt.Errorf("%w: status must be active or suspended", store.ErrInvalid)
	}
	if patch.Plan != nil && (strings.TrimSpace(*patch.Plan) == "" || len(*patch.Plan) > 64) {
		return store.Tenant{}, fmt.Errorf("%w: plan must be 1-64 characters", store.ErrInvalid)
	}
	if patch.PlatformSMSDailyLimit != nil && *patch.PlatformSMSDailyLimit < 0 {
		return store.Tenant{}, fmt.Errorf("%w: platform_sms_daily_limit must be >= 0", store.ErrInvalid)
	}
	for k, v := range patch.Quotas {
		if v < 0 || k == "" {
			return store.Tenant{}, fmt.Errorf("%w: quotas must be non-negative", store.ErrInvalid)
		}
	}
	var t store.Tenant
	err := s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		before, err := tx.Tenants().Get(ctx, tid)
		if err != nil {
			return err
		}
		if before.Status == store.TenantDeleted {
			return store.ErrNotFound
		}
		t, err = tx.Tenants().Update(ctx, tid, store.TenantPatch{
			Status: patch.Status, Plan: patch.Plan, Sandbox: patch.Sandbox,
			PlatformSMSDailyLimit: patch.PlatformSMSDailyLimit, Quotas: patch.Quotas,
		})
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, actorOf(p.OperatorID), audit.Event{
			TenantID: &tid, Action: "operator.tenant.update", EntityType: "tenant", EntityID: tid.String(),
			Before: limitsOf(before), After: map[string]any{"limits": limitsOf(t), "reason": patch.Reason},
		})
	})
	if err != nil {
		return store.Tenant{}, fmt.Errorf("update tenant: %w", err)
	}
	return t, nil
}

func limitsOf(t store.Tenant) map[string]any {
	return map[string]any{
		"status": string(t.Status), "plan": t.Plan, "sandbox": t.Sandbox,
		"platform_sms_daily_limit": t.PlatformSMSDailyLimit, "quotas": t.Quotas,
	}
}

func wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", op, err)
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

func newOperatorToken() (string, []byte) {
	b := make([]byte, 43)
	max := big.NewInt(int64(len(alphabet)))
	for i := range b {
		n, _ := rand.Int(rand.Reader, max)
		b[i] = alphabet[n.Int64()]
	}
	t := tokenPrefix + string(b)
	sum := sha256.Sum256([]byte(t))
	return t, sum[:]
}
