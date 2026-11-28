package entstore

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/ticatwolves/notifex/ent"
	"github.com/ticatwolves/notifex/ent/channelcredential"
	"github.com/ticatwolves/notifex/internal/store"
)

type credentials struct{ c *ent.Client }

func toCredential(r *ent.ChannelCredential) store.ChannelCredential {
	cfg := r.Config
	if cfg == nil {
		cfg = map[string]any{}
	}
	var secret []byte
	if r.SecretCiphertext != nil {
		secret = *r.SecretCiphertext
	}
	return store.ChannelCredential{
		ID: r.ID, TenantID: store.TenantID(r.TenantID), ApplicationID: r.ApplicationID, EnvironmentID: r.EnvironmentID,
		Channel: string(r.Channel), Provider: r.Provider, Mode: string(r.Mode), Name: r.Name, Config: cfg,
		SecretCiphertext: secret, SecretHint: r.SecretHint, Status: string(r.Status),
		VerifiedAt: r.VerifiedAt, RotatedAt: r.RotatedAt, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

func (s credentials) Create(ctx context.Context, tid store.TenantID, in store.NewChannelCredential) (store.ChannelCredential, error) {
	if in.ID == uuid.Nil {
		return store.ChannelCredential{}, invalid("credential id is required")
	}
	if in.EnvironmentID != nil && in.ApplicationID == nil {
		return store.ChannelCredential{}, invalid("an environment scope needs its application")
	}
	cfg := in.Config
	if cfg == nil {
		cfg = map[string]any{}
	}
	c := s.c.ChannelCredential.Create().SetID(in.ID).SetTenantID(tid.UUID()).
		SetNillableApplicationID(in.ApplicationID).SetNillableEnvironmentID(in.EnvironmentID).
		SetChannel(channelcredential.Channel(in.Channel)).SetProvider(in.Provider).
		SetMode(channelcredential.Mode(in.Mode)).SetName(in.Name).SetConfig(cfg).SetSecretHint(in.SecretHint)
	if in.SecretCiphertext != nil {
		c.SetSecretCiphertext(in.SecretCiphertext)
	}
	row, err := c.Save(scoped(ctx, tid))
	if err != nil {
		return store.ChannelCredential{}, fmt.Errorf("credentials.create: %w", mapErr(err, opCreate))
	}
	return toCredential(row), nil
}

func (s credentials) Get(ctx context.Context, tid store.TenantID, id uuid.UUID) (store.ChannelCredential, error) {
	row, err := s.c.ChannelCredential.Query().
		Where(channelcredential.TenantID(tid.UUID()), channelcredential.ID(id)).Only(scoped(ctx, tid))
	if err != nil {
		return store.ChannelCredential{}, fmt.Errorf("credentials.get: %w", mapErr(err, opRead))
	}
	return toCredential(row), nil
}

func (s credentials) List(ctx context.Context, tid store.TenantID, channel *string, appID *uuid.UUID, p store.Page) ([]store.ChannelCredential, error) {
	p = p.Normalize()
	q := s.c.ChannelCredential.Query().Where(channelcredential.TenantID(tid.UUID()))
	if channel != nil {
		q.Where(channelcredential.ChannelEQ(channelcredential.Channel(*channel)))
	}
	if appID != nil {
		q.Where(channelcredential.ApplicationID(*appID))
	}
	if p.After != uuid.Nil {
		q.Where(channelcredential.IDGT(p.After))
	}
	rows, err := q.Order(ent.Asc(channelcredential.FieldID)).Limit(p.Limit).All(scoped(ctx, tid))
	if err != nil {
		return nil, fmt.Errorf("credentials.list: %w", mapErr(err, opRead))
	}
	return mapSlice(rows, toCredential), nil
}

func (s credentials) Update(ctx context.Context, tid store.TenantID, id uuid.UUID, p store.CredentialPatch) (store.ChannelCredential, error) {
	u := s.c.ChannelCredential.UpdateOneID(id).Where(channelcredential.TenantID(tid.UUID()))
	if p.Name != nil {
		u.SetName(*p.Name)
	}
	if p.Config != nil {
		u.SetConfig(p.Config)
	}
	if p.Status != nil {
		u.SetStatus(channelcredential.Status(*p.Status))
	}
	row, err := u.Save(scoped(ctx, tid))
	if err != nil {
		return store.ChannelCredential{}, fmt.Errorf("credentials.update: %w", mapErr(err, opWrite))
	}
	return toCredential(row), nil
}

func (s credentials) Rotate(ctx context.Context, tid store.TenantID, id uuid.UUID, ciphertext []byte, hint string, now time.Time) (store.ChannelCredential, error) {
	row, err := s.c.ChannelCredential.UpdateOneID(id).Where(channelcredential.TenantID(tid.UUID())).
		SetSecretCiphertext(ciphertext).SetSecretHint(hint).SetRotatedAt(now).
		ClearVerifiedAt().Save(scoped(ctx, tid))
	if err != nil {
		return store.ChannelCredential{}, fmt.Errorf("credentials.rotate: %w", mapErr(err, opWrite))
	}
	return toCredential(row), nil
}

func (s credentials) Delete(ctx context.Context, tid store.TenantID, id uuid.UUID) error {
	n, err := s.c.ChannelCredential.Delete().
		Where(channelcredential.TenantID(tid.UUID()), channelcredential.ID(id)).Exec(scoped(ctx, tid))
	if err != nil {
		return fmt.Errorf("credentials.delete: %w", mapErr(err, opDelete))
	}
	if n == 0 {
		return fmt.Errorf("credentials.delete: %w", store.ErrNotFound)
	}
	return nil
}

func (s credentials) ActiveForScope(ctx context.Context, tid store.TenantID, channel string, appID, envID uuid.UUID) ([]store.ChannelCredential, error) {
	rows, err := s.c.ChannelCredential.Query().Where(
		channelcredential.TenantID(tid.UUID()),
		channelcredential.ChannelEQ(channelcredential.Channel(channel)),
		channelcredential.StatusNEQ(channelcredential.StatusDisabled),
		channelcredential.Or(
			channelcredential.ApplicationIDIsNil(),
			channelcredential.And(channelcredential.ApplicationID(appID), channelcredential.EnvironmentIDIsNil()),
			channelcredential.And(channelcredential.ApplicationID(appID), channelcredential.EnvironmentID(envID)),
		),
	).All(scoped(ctx, tid))
	if err != nil {
		return nil, fmt.Errorf("credentials.active_for_scope: %w", mapErr(err, opRead))
	}
	return mapSlice(rows, toCredential), nil
}
