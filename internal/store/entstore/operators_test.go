package entstore

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ticatwolves/notifex/internal/store"
)

func newOperator(t *testing.T, f fixture) store.Operator {
	t.Helper()
	op, err := f.s.Operators().Create(context.Background(), store.NewOperator{
		ID: uuid.Must(uuid.NewV7()), Email: "op-" + uuid.NewString()[:8] + "@notifex.test", Name: "Ops",
		PasswordHash: "$argon2id$x", TOTPWrappedKey: []byte("w"), TOTPKeyID: "local", TOTPCiphertext: []byte("c"),
	})
	require.NoError(t, err)
	return op
}

func TestOperatorStore(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	op := newOperator(t, f)

	got, err := f.s.Operators().GetByEmail(ctx, op.Email)
	require.NoError(t, err)
	assert.Equal(t, op.ID, got.ID)
	_, err = f.s.Operators().GetByEmail(ctx, "missing@notifex.test")
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = f.s.Operators().Create(ctx, store.NewOperator{ID: uuid.Must(uuid.NewV7()), Email: op.Email, PasswordHash: "x",
		TOTPWrappedKey: []byte("w"), TOTPKeyID: "k", TOTPCiphertext: []byte("c")})
	require.ErrorIs(t, err, store.ErrConflict)

	// The TOTP step only moves forward.
	for _, tc := range []struct {
		step int64
		want bool
	}{{100, true}, {100, false}, {99, false}, {101, true}} {
		ok, err := f.s.Operators().AdvanceTOTPStep(ctx, op.ID, tc.step)
		require.NoError(t, err)
		assert.Equal(t, tc.want, ok, "step %d", tc.step)
	}
	ok, err := f.s.Operators().AdvanceTOTPStep(ctx, uuid.Must(uuid.NewV7()), 1)
	require.NoError(t, err)
	assert.False(t, ok, "unknown operator")

	require.NoError(t, f.s.Operators().SetStatus(ctx, op.ID, store.OperatorDisabled))
	got, err = f.s.Operators().Get(ctx, op.ID)
	require.NoError(t, err)
	assert.Equal(t, store.OperatorDisabled, got.Status)
	require.ErrorIs(t, f.s.Operators().SetStatus(ctx, uuid.Must(uuid.NewV7()), store.OperatorActive), store.ErrNotFound)
}

func TestOperatorSessionStore(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	op, other := newOperator(t, f), newOperator(t, f)
	now := time.Now().UTC().Truncate(time.Microsecond)
	type sess struct {
		store.Session
		TokenHash []byte
	}
	mk := func(id uuid.UUID) sess {
		h := sha256.Sum256([]byte(uuid.NewString()))
		s, err := f.s.OperatorSessions().Create(ctx, store.NewSession{UserID: id, TokenHash: h[:], Now: now, AbsoluteExpiresAt: now.Add(8 * time.Hour)})
		require.NoError(t, err)
		return sess{s, h[:]}
	}
	s1, s2, s3 := mk(op.ID), mk(op.ID), mk(other.ID)

	got, err := f.s.OperatorSessions().GetByTokenHash(ctx, s1.TokenHash)
	require.NoError(t, err)
	assert.Equal(t, s1.ID, got.ID)

	require.NoError(t, f.s.OperatorSessions().Touch(ctx, s1.ID, now.Add(30*time.Second), time.Minute))
	got, _ = f.s.OperatorSessions().GetByTokenHash(ctx, s1.TokenHash)
	assert.Equal(t, now, got.LastSeenAt.UTC(), "touch within the interval is skipped")
	require.NoError(t, f.s.OperatorSessions().Touch(ctx, s1.ID, now.Add(2*time.Minute), time.Minute))
	got, _ = f.s.OperatorSessions().GetByTokenHash(ctx, s1.TokenHash)
	assert.Equal(t, now.Add(2*time.Minute), got.LastSeenAt.UTC())

	// Revoke is bound to the owning operator.
	require.ErrorIs(t, f.s.OperatorSessions().Revoke(ctx, other.ID, s1.ID, now), store.ErrNotFound)
	got, _ = f.s.OperatorSessions().GetByTokenHash(ctx, s1.TokenHash)
	require.Nil(t, got.RevokedAt, "a foreign revoke changed nothing")
	require.NoError(t, f.s.OperatorSessions().Revoke(ctx, op.ID, s1.ID, now))
	require.NoError(t, f.s.OperatorSessions().Revoke(ctx, op.ID, s1.ID, now), "idempotent")
	require.ErrorIs(t, f.s.OperatorSessions().Revoke(ctx, op.ID, uuid.Must(uuid.NewV7()), now), store.ErrNotFound)
	got, _ = f.s.OperatorSessions().GetByTokenHash(ctx, s1.TokenHash)
	require.NotNil(t, got.RevokedAt)

	n, err := f.s.OperatorSessions().RevokeAllForOperator(ctx, op.ID, now)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "only s2 was still live")
	got, _ = f.s.OperatorSessions().GetByTokenHash(ctx, s2.TokenHash)
	assert.NotNil(t, got.RevokedAt)
	got, _ = f.s.OperatorSessions().GetByTokenHash(ctx, s3.TokenHash)
	assert.Nil(t, got.RevokedAt, "another operator's session is untouched")
}

func TestTenantSearch(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	tag := strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	var made []store.Tenant
	for i := range 3 {
		ten := f.tenant(t)
		name := "Search" + tag + " Co " + string(rune('A'+i))
		ten, err := f.s.Tenants().Update(ctx, ten.ID, store.TenantPatch{Name: &name})
		require.NoError(t, err)
		made = append(made, ten)
	}
	susp := store.TenantSuspended
	_, err := f.s.Tenants().Update(ctx, made[1].ID, store.TenantPatch{Status: &susp})
	require.NoError(t, err)

	all, err := f.s.Tenants().Search(ctx, "search"+tag, nil, store.Page{})
	require.NoError(t, err)
	assert.Len(t, all, 3, "name match is case-insensitive")

	bySlug, err := f.s.Tenants().Search(ctx, strings.ToUpper(made[0].Slug), nil, store.Page{})
	require.NoError(t, err)
	require.Len(t, bySlug, 1)
	assert.Equal(t, made[0].ID, bySlug[0].ID)

	onlySusp, err := f.s.Tenants().Search(ctx, "search"+tag, &susp, store.Page{})
	require.NoError(t, err)
	require.Len(t, onlySusp, 1)
	assert.Equal(t, made[1].ID, onlySusp[0].ID)

	// LIKE wildcards in q are literals.
	none, err := f.s.Tenants().Search(ctx, "%"+tag+"_", nil, store.Page{})
	require.NoError(t, err)
	assert.Empty(t, none)

	// Keyset pagination walks every match once.
	var seen []uuid.UUID
	p := store.Page{Limit: 2}
	for {
		page, err := f.s.Tenants().Search(ctx, "search"+tag, nil, p)
		require.NoError(t, err)
		for _, ten := range page {
			seen = append(seen, ten.ID.UUID())
		}
		if len(page) < p.Limit {
			break
		}
		p.After = page[len(page)-1].ID.UUID()
	}
	assert.Len(t, seen, 3)
}
