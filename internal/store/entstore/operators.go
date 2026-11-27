package entstore

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ticatwolves/notifex/ent"
	entop "github.com/ticatwolves/notifex/ent/operator"
	"github.com/ticatwolves/notifex/ent/operatorsession"
	"github.com/ticatwolves/notifex/internal/store"
)

type operators struct{ c *ent.Client }

func (o operators) Create(ctx context.Context, in store.NewOperator) (store.Operator, error) {
	if in.ID == uuid.Nil || in.PasswordHash == "" || len(in.TOTPCiphertext) == 0 {
		return store.Operator{}, invalid("operator requires id, password hash and TOTP secret")
	}
	row, err := o.c.Operator.Create().
		SetID(in.ID).SetEmail(strings.ToLower(strings.TrimSpace(in.Email))).SetName(in.Name).
		SetPasswordHash(in.PasswordHash).
		SetTotpWrappedKey(in.TOTPWrappedKey).SetTotpKeyID(in.TOTPKeyID).SetTotpCiphertext(in.TOTPCiphertext).
		Save(ctx)
	if err != nil {
		return store.Operator{}, fmt.Errorf("operators.create: %w", mapErr(err, opCreate))
	}
	return toOperator(row), nil
}

func (o operators) Get(ctx context.Context, id uuid.UUID) (store.Operator, error) {
	row, err := o.c.Operator.Get(ctx, id)
	if err != nil {
		return store.Operator{}, fmt.Errorf("operators.get: %w", mapErr(err, opRead))
	}
	return toOperator(row), nil
}

func (o operators) GetByEmail(ctx context.Context, email string) (store.Operator, error) {
	row, err := o.c.Operator.Query().Where(entop.Email(strings.ToLower(strings.TrimSpace(email)))).Only(ctx)
	if err != nil {
		return store.Operator{}, fmt.Errorf("operators.get_by_email: %w", mapErr(err, opRead))
	}
	return toOperator(row), nil
}

// AdvanceTOTPStep is one conditional UPDATE: two logins racing with the same
// code can't both succeed.
func (o operators) AdvanceTOTPStep(ctx context.Context, id uuid.UUID, step int64) (bool, error) {
	n, err := o.c.Operator.Update().
		Where(entop.ID(id), entop.TotpLastStepLT(step)).
		SetTotpLastStep(step).
		Save(ctx)
	if err != nil {
		return false, fmt.Errorf("operators.advance_totp_step: %w", mapErr(err, opWrite))
	}
	return n == 1, nil
}

func (o operators) SetStatus(ctx context.Context, id uuid.UUID, s store.OperatorStatus) error {
	if err := o.c.Operator.UpdateOneID(id).SetStatus(entop.Status(s)).Exec(ctx); err != nil {
		return fmt.Errorf("operators.set_status: %w", mapErr(err, opWrite))
	}
	return nil
}

func toOperator(r *ent.Operator) store.Operator {
	return store.Operator{
		ID: r.ID, Email: r.Email, Name: r.Name, PasswordHash: r.PasswordHash,
		TOTPWrappedKey: r.TotpWrappedKey, TOTPKeyID: r.TotpKeyID, TOTPCiphertext: r.TotpCiphertext,
		Status: store.OperatorStatus(r.Status), CreatedAt: r.CreatedAt,
	}
}

type operatorSessions struct{ c *ent.Client }

func (s operatorSessions) Create(ctx context.Context, in store.NewSession) (store.Session, error) {
	if in.UserID == uuid.Nil || len(in.TokenHash) == 0 || !in.AbsoluteExpiresAt.After(in.Now) {
		return store.Session{}, invalid("operator session requires operator, token hash and a future expiry")
	}
	ua := in.UserAgent
	if len(ua) > 256 {
		ua = ua[:256]
	}
	row, err := s.c.OperatorSession.Create().
		SetOperatorID(in.UserID).SetTokenHash(in.TokenHash).SetCreatedAt(in.Now).SetLastSeenAt(in.Now).
		SetAbsoluteExpiresAt(in.AbsoluteExpiresAt).SetUserAgent(ua).
		Save(ctx)
	if err != nil {
		return store.Session{}, fmt.Errorf("operator_sessions.create: %w", mapErr(err, opCreate))
	}
	return toOpSession(row), nil
}

func (s operatorSessions) GetByTokenHash(ctx context.Context, hash []byte) (store.Session, error) {
	row, err := s.c.OperatorSession.Query().Where(operatorsession.TokenHash(hash)).Only(ctx)
	if err != nil {
		return store.Session{}, fmt.Errorf("operator_sessions.get_by_token_hash: %w", mapErr(err, opRead))
	}
	return toOpSession(row), nil
}

func (s operatorSessions) Touch(ctx context.Context, id uuid.UUID, now time.Time, minInterval time.Duration) error {
	_, err := s.c.OperatorSession.Update().
		Where(operatorsession.ID(id), operatorsession.RevokedAtIsNil(), operatorsession.LastSeenAtLT(now.Add(-minInterval))).
		SetLastSeenAt(now).Save(ctx)
	if err != nil {
		return fmt.Errorf("operator_sessions.touch: %w", mapErr(err, opWrite))
	}
	return nil
}

func (s operatorSessions) Revoke(ctx context.Context, operatorID, id uuid.UUID, now time.Time) error {
	n, err := s.c.OperatorSession.Update().
		Where(operatorsession.ID(id), operatorsession.OperatorID(operatorID), operatorsession.RevokedAtIsNil()).
		SetRevokedAt(now).Save(ctx)
	if err != nil {
		return fmt.Errorf("operator_sessions.revoke: %w", mapErr(err, opWrite))
	}
	if n == 0 { // already revoked is fine; another operator's or unknown is not found
		exists, err := s.c.OperatorSession.Query().Where(operatorsession.ID(id), operatorsession.OperatorID(operatorID)).Exist(ctx)
		if err != nil {
			return fmt.Errorf("operator_sessions.revoke: %w", mapErr(err, opRead))
		}
		if !exists {
			return fmt.Errorf("operator_sessions.revoke: %w", store.ErrNotFound)
		}
	}
	return nil
}

func (s operatorSessions) RevokeAllForOperator(ctx context.Context, operatorID uuid.UUID, now time.Time) (int, error) {
	n, err := s.c.OperatorSession.Update().
		Where(operatorsession.OperatorID(operatorID), operatorsession.RevokedAtIsNil()).
		SetRevokedAt(now).Save(ctx)
	if err != nil {
		return 0, fmt.Errorf("operator_sessions.revoke_all: %w", mapErr(err, opWrite))
	}
	return n, nil
}

func toOpSession(r *ent.OperatorSession) store.Session {
	return store.Session{
		ID: r.ID, UserID: r.OperatorID, CreatedAt: r.CreatedAt, LastSeenAt: r.LastSeenAt,
		AbsoluteExpiresAt: r.AbsoluteExpiresAt, ReauthenticatedAt: r.CreatedAt, RevokedAt: r.RevokedAt, UserAgent: r.UserAgent,
	}
}
