package entstore

import (
	"context"
	"fmt"
	"strings"

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

func toUser(r *ent.User) store.User {
	return store.User{
		ID: r.ID, Email: r.Email, Name: r.Name, PasswordHash: r.PasswordHash,
		Status: store.UserStatus(r.Status), EmailVerifiedAt: r.EmailVerifiedAt,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}
