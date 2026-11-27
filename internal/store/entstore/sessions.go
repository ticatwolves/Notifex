package entstore

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/ticatwolves/notifex/ent"
	entsession "github.com/ticatwolves/notifex/ent/session"
	"github.com/ticatwolves/notifex/internal/store"
)

type sessions struct{ c *ent.Client }

func (s sessions) Create(ctx context.Context, in store.NewSession) (store.Session, error) {
	if in.UserID == uuid.Nil || len(in.TokenHash) == 0 || !in.AbsoluteExpiresAt.After(in.Now) {
		return store.Session{}, invalid("session requires user, token hash and a future expiry")
	}
	ua := in.UserAgent
	if len(ua) > 256 {
		ua = ua[:256]
	}
	row, err := s.c.Session.Create().
		SetUserID(in.UserID).
		SetTokenHash(in.TokenHash).
		SetCreatedAt(in.Now).
		SetLastSeenAt(in.Now).
		SetReauthenticatedAt(in.Now).
		SetAbsoluteExpiresAt(in.AbsoluteExpiresAt).
		SetUserAgent(ua).
		Save(ctx)
	if err != nil {
		return store.Session{}, fmt.Errorf("sessions.create: %w", mapErr(err, opCreate))
	}
	return toSession(row), nil
}

func (s sessions) GetByTokenHash(ctx context.Context, hash []byte) (store.Session, error) {
	row, err := s.c.Session.Query().Where(entsession.TokenHash(hash)).Only(ctx)
	if err != nil {
		return store.Session{}, fmt.Errorf("sessions.get_by_token_hash: %w", mapErr(err, opRead))
	}
	return toSession(row), nil
}

func (s sessions) Touch(ctx context.Context, id uuid.UUID, now time.Time, minInterval time.Duration) error {
	_, err := s.c.Session.Update().
		Where(entsession.ID(id), entsession.RevokedAtIsNil(), entsession.LastSeenAtLT(now.Add(-minInterval))).
		SetLastSeenAt(now).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("sessions.touch: %w", mapErr(err, opWrite))
	}
	return nil
}

func (s sessions) MarkReauthenticated(ctx context.Context, id uuid.UUID, now time.Time) error {
	n, err := s.c.Session.Update().
		Where(entsession.ID(id), entsession.RevokedAtIsNil()).
		SetReauthenticatedAt(now).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("sessions.mark_reauthenticated: %w", mapErr(err, opWrite))
	}
	if n == 0 {
		return fmt.Errorf("sessions.mark_reauthenticated: %w", store.ErrNotFound)
	}
	return nil
}

func (s sessions) Revoke(ctx context.Context, userID, id uuid.UUID, now time.Time) error {
	n, err := s.c.Session.Update().
		Where(entsession.ID(id), entsession.UserID(userID), entsession.RevokedAtIsNil()).
		SetRevokedAt(now).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("sessions.revoke: %w", mapErr(err, opWrite))
	}
	if n == 0 {
		exists, err := s.c.Session.Query().Where(entsession.ID(id), entsession.UserID(userID)).Exist(ctx)
		if err != nil {
			return fmt.Errorf("sessions.revoke: %w", mapErr(err, opRead))
		}
		if !exists {
			return fmt.Errorf("sessions.revoke: %w", store.ErrNotFound)
		}
	}
	return nil
}

func (s sessions) RevokeAllForUser(ctx context.Context, userID, keep uuid.UUID, now time.Time) (int, error) {
	u := s.c.Session.Update().Where(entsession.UserID(userID), entsession.RevokedAtIsNil())
	if keep != uuid.Nil {
		u.Where(entsession.IDNEQ(keep))
	}
	n, err := u.SetRevokedAt(now).Save(ctx)
	if err != nil {
		return 0, fmt.Errorf("sessions.revoke_all_for_user: %w", mapErr(err, opWrite))
	}
	return n, nil
}

func (s sessions) ListActive(ctx context.Context, userID uuid.UUID, now time.Time, idle time.Duration) ([]store.Session, error) {
	rows, err := s.c.Session.Query().
		Where(
			entsession.UserID(userID), entsession.RevokedAtIsNil(),
			entsession.AbsoluteExpiresAtGT(now), entsession.LastSeenAtGT(now.Add(-idle)),
		).
		Order(ent.Desc(entsession.FieldLastSeenAt)).
		Limit(store.MaxPageSize).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("sessions.list_active: %w", mapErr(err, opRead))
	}
	return mapSlice(rows, toSession), nil
}

func toSession(r *ent.Session) store.Session {
	return store.Session{
		ID: r.ID, UserID: r.UserID, CreatedAt: r.CreatedAt, LastSeenAt: r.LastSeenAt,
		AbsoluteExpiresAt: r.AbsoluteExpiresAt, ReauthenticatedAt: r.ReauthenticatedAt,
		RevokedAt: r.RevokedAt, UserAgent: r.UserAgent,
	}
}
