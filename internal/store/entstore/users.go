package entstore

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ticatwolves/notifex/ent"
	"github.com/ticatwolves/notifex/ent/user"
	"github.com/ticatwolves/notifex/internal/store"
)

type users struct{ c *ent.Client }

func normalizeEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func (u users) Create(ctx context.Context, in store.NewUser) (store.User, error) {
	email := normalizeEmail(in.Email)
	if at := strings.IndexByte(email, '@'); at <= 0 || at == len(email)-1 {
		return store.User{}, invalid("user email")
	}
	row, err := u.c.User.Create().
		SetEmail(email).
		SetName(in.Name).
		SetNillablePasswordHash(in.PasswordHash).
		Save(ctx)
	if err != nil {
		return store.User{}, fmt.Errorf("users.create: %w", mapErr(err, opCreate))
	}
	return toUser(row), nil
}

func (u users) Get(ctx context.Context, id uuid.UUID) (store.User, error) {
	row, err := u.c.User.Get(ctx, id)
	if err != nil {
		return store.User{}, fmt.Errorf("users.get: %w", mapErr(err, opRead))
	}
	return toUser(row), nil
}

func (u users) GetByEmail(ctx context.Context, email string) (store.User, error) {
	row, err := u.c.User.Query().Where(user.Email(normalizeEmail(email))).Only(ctx)
	if err != nil {
		return store.User{}, fmt.Errorf("users.get_by_email: %w", mapErr(err, opRead))
	}
	return toUser(row), nil
}

func (u users) SetPasswordHash(ctx context.Context, id uuid.UUID, hash string) error {
	if hash == "" {
		return invalid("password hash is empty")
	}
	if err := u.c.User.UpdateOneID(id).SetPasswordHash(hash).Exec(ctx); err != nil {
		return fmt.Errorf("users.set_password_hash: %w", mapErr(err, opWrite))
	}
	return nil
}

func (u users) MarkEmailVerified(ctx context.Context, id uuid.UUID, at time.Time) error {
	n, err := u.c.User.Update().Where(user.ID(id), user.EmailVerifiedAtIsNil()).SetEmailVerifiedAt(at).Save(ctx)
	if err != nil {
		return fmt.Errorf("users.mark_email_verified: %w", mapErr(err, opWrite))
	}
	if n == 0 {
		exists, err := u.c.User.Query().Where(user.ID(id)).Exist(ctx)
		if err != nil {
			return fmt.Errorf("users.mark_email_verified: %w", mapErr(err, opRead))
		}
		if !exists {
			return fmt.Errorf("users.mark_email_verified: %w", store.ErrNotFound)
		}
	}
	return nil
}

func toUser(r *ent.User) store.User {
	return store.User{
		ID: r.ID, Email: r.Email, Name: r.Name, PasswordHash: r.PasswordHash,
		Status: store.UserStatus(r.Status), EmailVerifiedAt: r.EmailVerifiedAt,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}
