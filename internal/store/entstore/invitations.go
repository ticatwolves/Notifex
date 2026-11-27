package entstore

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ticatwolves/notifex/ent"
	"github.com/ticatwolves/notifex/ent/invitation"
	"github.com/ticatwolves/notifex/internal/store"
)

type invitations struct{ c *ent.Client }

func (i invitations) Create(ctx context.Context, tid store.TenantID, in store.NewInvitation) (store.Invitation, error) {
	if len(in.TokenHash) == 0 || !in.ExpiresAt.After(in.Now) {
		return store.Invitation{}, invalid("invitation requires a token hash and a future expiry")
	}
	row, err := i.c.Invitation.Create().
		SetTenantID(tid.UUID()).
		SetEmail(strings.ToLower(strings.TrimSpace(in.Email))).
		SetRole(invitation.Role(in.Role)).
		SetTokenHash(in.TokenHash).
		SetInvitedBy(in.InvitedBy).
		SetCreatedAt(in.Now).
		SetExpiresAt(in.ExpiresAt).
		Save(scoped(ctx, tid))
	if err != nil {
		return store.Invitation{}, fmt.Errorf("invitations.create: %w", mapErr(err, opCreate))
	}
	return toInvitation(row), nil
}

func (i invitations) ListPending(ctx context.Context, tid store.TenantID, now time.Time) ([]store.Invitation, error) {
	rows, err := i.c.Invitation.Query().
		Where(
			invitation.TenantID(tid.UUID()), invitation.AcceptedAtIsNil(), invitation.RevokedAtIsNil(),
			invitation.ExpiresAtGT(now),
		).
		Order(ent.Desc(invitation.FieldCreatedAt)).
		Limit(store.MaxPageSize).
		All(scoped(ctx, tid))
	if err != nil {
		return nil, fmt.Errorf("invitations.list_pending: %w", mapErr(err, opRead))
	}
	return mapSlice(rows, toInvitation), nil
}

func (i invitations) Revoke(ctx context.Context, tid store.TenantID, id uuid.UUID, now time.Time) error {
	n, err := i.c.Invitation.Update().
		Where(invitation.TenantID(tid.UUID()), invitation.ID(id), invitation.AcceptedAtIsNil(), invitation.RevokedAtIsNil()).
		SetRevokedAt(now).
		Save(scoped(ctx, tid))
	if err != nil {
		return fmt.Errorf("invitations.revoke: %w", mapErr(err, opWrite))
	}
	if n == 0 {
		return fmt.Errorf("invitations.revoke: %w", store.ErrNotFound)
	}
	return nil
}

func (i invitations) RevokeLiveForEmail(ctx context.Context, tid store.TenantID, email string, now time.Time) (int, error) {
	n, err := i.c.Invitation.Update().
		Where(
			invitation.TenantID(tid.UUID()), invitation.Email(strings.ToLower(strings.TrimSpace(email))),
			invitation.AcceptedAtIsNil(), invitation.RevokedAtIsNil(),
		).
		SetRevokedAt(now).
		Save(scoped(ctx, tid))
	if err != nil {
		return 0, fmt.Errorf("invitations.revoke_live_for_email: %w", mapErr(err, opWrite))
	}
	return n, nil
}

// Consume is one conditional UPDATE ... RETURNING: an invitation is accepted
// at most once even when the link is used concurrently.
func (i invitations) Consume(ctx context.Context, tokenHash []byte, userID uuid.UUID, now time.Time) (store.Invitation, error) {
	rows, err := i.c.QueryContext(ctx, `
		UPDATE invitations SET accepted_at = $1, accepted_by = $2
		WHERE token_hash = $3 AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > $1
		RETURNING id, tenant_id, email, role, invited_by, created_at, expires_at`,
		now, userID, tokenHash)
	if err != nil {
		return store.Invitation{}, fmt.Errorf("invitations.consume: %w", mapErr(err, opWrite))
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return store.Invitation{}, fmt.Errorf("invitations.consume: %w", err)
		}
		return store.Invitation{}, fmt.Errorf("invitations.consume: %w", store.ErrNotFound)
	}
	var inv store.Invitation
	var tid uuid.UUID
	var role string
	var by uuid.NullUUID
	if err := rows.Scan(&inv.ID, &tid, &inv.Email, &role, &by, &inv.CreatedAt, &inv.ExpiresAt); err != nil {
		return store.Invitation{}, fmt.Errorf("invitations.consume: %w", err)
	}
	inv.TenantID, inv.Role, inv.AcceptedAt = store.TenantID(tid), store.Role(role), &now
	if by.Valid {
		b := by.UUID
		inv.InvitedBy = &b
	}
	return inv, nil
}

func toInvitation(r *ent.Invitation) store.Invitation {
	return store.Invitation{
		ID: r.ID, TenantID: store.TenantID(r.TenantID), Email: r.Email, Role: store.Role(r.Role),
		InvitedBy: r.InvitedBy, CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt,
		AcceptedAt: r.AcceptedAt, RevokedAt: r.RevokedAt,
	}
}
