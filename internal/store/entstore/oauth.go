package entstore

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/ticatwolves/notifex/ent"
	"github.com/ticatwolves/notifex/ent/oauthstate"
	"github.com/ticatwolves/notifex/ent/useridentity"
	"github.com/ticatwolves/notifex/internal/store"
)

type identities struct{ c *ent.Client }

func (i identities) Create(ctx context.Context, in store.NewIdentity) (store.Identity, error) {
	if in.UserID == uuid.Nil || in.Subject == "" {
		return store.Identity{}, invalid("identity requires user and subject")
	}
	email := in.Email
	if len(email) > 320 {
		email = ""
	}
	row, err := i.c.UserIdentity.Create().
		SetUserID(in.UserID).SetProvider(useridentity.Provider(in.Provider)).SetSubject(in.Subject).
		SetEmail(email).SetCreatedAt(in.Now).SetLastUsedAt(in.Now).
		Save(ctx)
	if err != nil {
		return store.Identity{}, fmt.Errorf("identities.create: %w", mapErr(err, opCreate))
	}
	return toIdentity(row), nil
}

func (i identities) GetBySubject(ctx context.Context, p store.OAuthProvider, subject string) (store.Identity, error) {
	row, err := i.c.UserIdentity.Query().
		Where(useridentity.ProviderEQ(useridentity.Provider(p)), useridentity.Subject(subject)).
		Only(ctx)
	if err != nil {
		return store.Identity{}, fmt.Errorf("identities.get_by_subject: %w", mapErr(err, opRead))
	}
	return toIdentity(row), nil
}

func (i identities) ListForUser(ctx context.Context, userID uuid.UUID) ([]store.Identity, error) {
	rows, err := i.c.UserIdentity.Query().
		Where(useridentity.UserID(userID)).
		Order(ent.Asc(useridentity.FieldProvider)).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("identities.list_for_user: %w", mapErr(err, opRead))
	}
	return mapSlice(rows, toIdentity), nil
}

func (i identities) Delete(ctx context.Context, userID uuid.UUID, p store.OAuthProvider) error {
	n, err := i.c.UserIdentity.Delete().
		Where(useridentity.UserID(userID), useridentity.ProviderEQ(useridentity.Provider(p))).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("identities.delete: %w", mapErr(err, opDelete))
	}
	if n == 0 {
		return fmt.Errorf("identities.delete: %w", store.ErrNotFound)
	}
	return nil
}

func (i identities) Touch(ctx context.Context, id uuid.UUID, now time.Time) error {
	if err := i.c.UserIdentity.UpdateOneID(id).SetLastUsedAt(now).Exec(ctx); err != nil {
		return fmt.Errorf("identities.touch: %w", mapErr(err, opWrite))
	}
	return nil
}

func toIdentity(r *ent.UserIdentity) store.Identity {
	return store.Identity{
		ID: r.ID, UserID: r.UserID, Provider: store.OAuthProvider(r.Provider), Subject: r.Subject,
		Email: r.Email, CreatedAt: r.CreatedAt, LastUsedAt: r.LastUsedAt,
	}
}

type oauthStates struct{ c *ent.Client }

func (o oauthStates) Create(ctx context.Context, in store.NewOAuthState) error {
	if len(in.StateHash) == 0 || in.CodeVerifier == "" || in.Nonce == "" || in.RedirectURI == "" || !in.ExpiresAt.After(in.Now) {
		return invalid("oauth state requires hash, verifier, nonce, redirect and a future expiry")
	}
	err := o.c.OAuthState.Create().
		SetStateHash(in.StateHash).SetProvider(oauthstate.Provider(in.Provider)).SetRedirectURI(in.RedirectURI).
		SetCodeVerifier(in.CodeVerifier).SetNonce(in.Nonce).SetNillableLinkUserID(in.LinkUserID).
		SetCreatedAt(in.Now).SetExpiresAt(in.ExpiresAt).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("oauth_states.create: %w", mapErr(err, opCreate))
	}
	return nil
}

// Consume is one conditional UPDATE ... RETURNING: a state can complete at
// most one login, even when the callback is replayed concurrently.
func (o oauthStates) Consume(ctx context.Context, stateHash []byte, p store.OAuthProvider, now time.Time) (store.OAuthState, error) {
	rows, err := o.c.QueryContext(ctx, `
		UPDATE oauth_states SET used_at = $1
		WHERE state_hash = $2 AND provider = $3 AND used_at IS NULL AND expires_at > $1
		RETURNING id, provider, redirect_uri, code_verifier, nonce, link_user_id, expires_at`,
		now, stateHash, string(p))
	if err != nil {
		return store.OAuthState{}, fmt.Errorf("oauth_states.consume: %w", mapErr(err, opWrite))
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return store.OAuthState{}, fmt.Errorf("oauth_states.consume: %w", err)
		}
		return store.OAuthState{}, fmt.Errorf("oauth_states.consume: %w", store.ErrNotFound)
	}
	var st store.OAuthState
	var prov string
	var link uuid.NullUUID
	var exp sql.NullTime
	if err := rows.Scan(&st.ID, &prov, &st.RedirectURI, &st.CodeVerifier, &st.Nonce, &link, &exp); err != nil {
		return store.OAuthState{}, fmt.Errorf("oauth_states.consume: %w", err)
	}
	st.Provider = store.OAuthProvider(prov)
	st.ExpiresAt = exp.Time
	if link.Valid {
		id := link.UUID
		st.LinkUserID = &id
	}
	return st, nil
}
