package entstore

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/ticatwolves/notifex/ent"
	"github.com/ticatwolves/notifex/ent/emailtoken"
	"github.com/ticatwolves/notifex/internal/store"
)

type emailTokens struct{ c *ent.Client }

func (e emailTokens) Create(ctx context.Context, userID uuid.UUID, purpose store.EmailTokenPurpose, hash []byte, now, expiresAt time.Time) error {
	if len(hash) == 0 || !expiresAt.After(now) {
		return invalid("email token requires a hash and a future expiry")
	}
	err := e.c.EmailToken.Create().
		SetUserID(userID).SetPurpose(emailtoken.Purpose(purpose)).SetTokenHash(hash).
		SetCreatedAt(now).SetExpiresAt(expiresAt).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("email_tokens.create: %w", mapErr(err, opCreate))
	}
	return nil
}

// Consume is one conditional UPDATE ... RETURNING, so two concurrent uses of
// the same token can never both succeed.
func (e emailTokens) Consume(ctx context.Context, purpose store.EmailTokenPurpose, hash []byte, now time.Time) (uuid.UUID, error) {
	rows, err := e.c.QueryContext(ctx, `
		UPDATE email_tokens SET used_at = $1
		WHERE token_hash = $2 AND purpose = $3 AND used_at IS NULL AND expires_at > $1
		RETURNING user_id`, now, hash, string(purpose))
	if err != nil {
		return uuid.Nil, fmt.Errorf("email_tokens.consume: %w", mapErr(err, opWrite))
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return uuid.Nil, fmt.Errorf("email_tokens.consume: %w", mapErr(err, opWrite))
		}
		return uuid.Nil, fmt.Errorf("email_tokens.consume: %w", store.ErrNotFound)
	}
	var id uuid.UUID
	if err := rows.Scan(&id); err != nil {
		return uuid.Nil, fmt.Errorf("email_tokens.consume: %w", err)
	}
	return id, nil
}

func (e emailTokens) InvalidateForUser(ctx context.Context, userID uuid.UUID, purpose store.EmailTokenPurpose, now time.Time) error {
	_, err := e.c.EmailToken.Update().
		Where(emailtoken.UserID(userID), emailtoken.PurposeEQ(emailtoken.Purpose(purpose)), emailtoken.UsedAtIsNil()).
		SetUsedAt(now).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("email_tokens.invalidate_for_user: %w", mapErr(err, opWrite))
	}
	return nil
}
