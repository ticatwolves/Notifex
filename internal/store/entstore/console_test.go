package entstore

import (
	"context"
	"crypto/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ticatwolves/notifex/internal/store"
)

func randHash() []byte {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return b
}

func (f fixture) user(t *testing.T) store.User {
	t.Helper()
	u, err := f.s.Users().Create(context.Background(), store.NewUser{Email: uuid.NewString()[:10] + "@example.com"})
	require.NoError(t, err)
	return u
}

func TestUserPasswordAndVerification(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	u := f.user(t)

	require.NoError(t, f.s.Users().SetPasswordHash(ctx, u.ID, "$argon2id$v=19$m=47104,t=1,p=1$c2FsdA$aGFzaA"))
	require.ErrorIs(t, f.s.Users().SetPasswordHash(ctx, u.ID, ""), store.ErrInvalid)
	require.ErrorIs(t, f.s.Users().SetPasswordHash(ctx, uuid.New(), "x"), store.ErrNotFound)

	t1 := time.Now().UTC().Truncate(time.Microsecond)
	require.NoError(t, f.s.Users().MarkEmailVerified(ctx, u.ID, t1))
	require.NoError(t, f.s.Users().MarkEmailVerified(ctx, u.ID, t1.Add(time.Hour)), "idempotent")
	got, err := f.s.Users().Get(ctx, u.ID)
	require.NoError(t, err)
	require.NotNil(t, got.EmailVerifiedAt)
	assert.True(t, got.EmailVerifiedAt.Equal(t1), "first verification time kept")
	require.NotNil(t, got.PasswordHash)
	require.ErrorIs(t, f.s.Users().MarkEmailVerified(ctx, uuid.New(), t1), store.ErrNotFound)
}

func TestSessions(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	alice, bob := f.user(t), f.user(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	mk := func(u store.User, at time.Time) (store.Session, []byte) {
		h := randHash()
		s, err := f.s.Sessions().Create(ctx, store.NewSession{UserID: u.ID, TokenHash: h, Now: at, AbsoluteExpiresAt: at.Add(7 * 24 * time.Hour), UserAgent: "test"})
		require.NoError(t, err)
		return s, h
	}

	s1, h1 := mk(alice, now)
	got, err := f.s.Sessions().GetByTokenHash(ctx, h1)
	require.NoError(t, err)
	assert.Equal(t, s1.ID, got.ID)
	assert.True(t, got.ReauthenticatedAt.Equal(now), "login counts as authentication")
	_, err = f.s.Sessions().GetByTokenHash(ctx, randHash())
	require.ErrorIs(t, err, store.ErrNotFound)

	_, err = f.s.Sessions().Create(ctx, store.NewSession{UserID: alice.ID, TokenHash: h1, Now: now, AbsoluteExpiresAt: now.Add(time.Hour)})
	require.ErrorIs(t, err, store.ErrConflict, "token hashes are unique")
	_, err = f.s.Sessions().Create(ctx, store.NewSession{UserID: alice.ID, TokenHash: randHash(), Now: now, AbsoluteExpiresAt: now})
	require.ErrorIs(t, err, store.ErrInvalid)

	// Touch is throttled.
	require.NoError(t, f.s.Sessions().Touch(ctx, s1.ID, now.Add(30*time.Second), time.Minute))
	got, _ = f.s.Sessions().GetByTokenHash(ctx, h1)
	assert.True(t, got.LastSeenAt.Equal(now), "within a minute: not written")
	require.NoError(t, f.s.Sessions().Touch(ctx, s1.ID, now.Add(2*time.Minute), time.Minute))
	got, _ = f.s.Sessions().GetByTokenHash(ctx, h1)
	assert.True(t, got.LastSeenAt.Equal(now.Add(2*time.Minute)))

	require.NoError(t, f.s.Sessions().MarkReauthenticated(ctx, s1.ID, now.Add(5*time.Minute)))
	got, _ = f.s.Sessions().GetByTokenHash(ctx, h1)
	assert.True(t, got.ReauthenticatedAt.Equal(now.Add(5*time.Minute)))

	// Users can't revoke each other's sessions.
	bobSess, _ := mk(bob, now)
	require.ErrorIs(t, f.s.Sessions().Revoke(ctx, alice.ID, bobSess.ID, now), store.ErrNotFound)
	require.NoError(t, f.s.Sessions().Revoke(ctx, alice.ID, s1.ID, now))
	require.NoError(t, f.s.Sessions().Revoke(ctx, alice.ID, s1.ID, now), "idempotent")
	require.ErrorIs(t, f.s.Sessions().MarkReauthenticated(ctx, s1.ID, now), store.ErrNotFound, "revoked")

	// ListActive excludes revoked, idle and absolutely expired sessions.
	keep, _ := mk(alice, now)
	_, _ = mk(alice, now.Add(-13*time.Hour)) // idle beyond 12h
	expired := store.NewSession{UserID: alice.ID, TokenHash: randHash(), Now: now.Add(-8 * 24 * time.Hour), AbsoluteExpiresAt: now.Add(-time.Hour)}
	_, err = f.s.Sessions().Create(ctx, expired)
	require.NoError(t, err)
	other, _ := mk(alice, now)
	active, err := f.s.Sessions().ListActive(ctx, alice.ID, now.Add(time.Second), 12*time.Hour)
	require.NoError(t, err)
	assert.ElementsMatch(t, []uuid.UUID{keep.ID, other.ID}, ids(active))

	n, err := f.s.Sessions().RevokeAllForUser(ctx, alice.ID, keep.ID, now)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, 1)
	active, err = f.s.Sessions().ListActive(ctx, alice.ID, now.Add(time.Second), 12*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, []uuid.UUID{keep.ID}, ids(active), "only the kept session survives")
	bobActive, err := f.s.Sessions().ListActive(ctx, bob.ID, now.Add(time.Second), 12*time.Hour)
	require.NoError(t, err)
	assert.Len(t, bobActive, 1, "other users untouched")
}

func ids(ss []store.Session) []uuid.UUID {
	out := make([]uuid.UUID, len(ss))
	for i, s := range ss {
		out[i] = s.ID
	}
	return out
}

func TestEmailTokensAreSingleUse(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	u := f.user(t)
	now := time.Now().UTC()
	h := randHash()
	require.NoError(t, f.s.EmailTokens().Create(ctx, u.ID, store.TokenResetPassword, h, now, now.Add(time.Hour)))

	_, err := f.s.EmailTokens().Consume(ctx, store.TokenVerifyEmail, h, now)
	require.ErrorIs(t, err, store.ErrNotFound, "wrong purpose")

	// Racing consumers: exactly one wins.
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			if id, err := f.s.EmailTokens().Consume(ctx, store.TokenResetPassword, h, now); err == nil {
				assert.Equal(t, u.ID, id)
				wins.Add(1)
			}
		})
	}
	wg.Wait()
	assert.Equal(t, int32(1), wins.Load())

	expired := randHash()
	require.NoError(t, f.s.EmailTokens().Create(ctx, u.ID, store.TokenVerifyEmail, expired, now.Add(-2*time.Hour), now.Add(-time.Hour)))
	_, err = f.s.EmailTokens().Consume(ctx, store.TokenVerifyEmail, expired, now)
	require.ErrorIs(t, err, store.ErrNotFound, "expired")

	live := randHash()
	require.NoError(t, f.s.EmailTokens().Create(ctx, u.ID, store.TokenVerifyEmail, live, now, now.Add(time.Hour)))
	require.NoError(t, f.s.EmailTokens().InvalidateForUser(ctx, u.ID, store.TokenVerifyEmail, now))
	_, err = f.s.EmailTokens().Consume(ctx, store.TokenVerifyEmail, live, now)
	require.ErrorIs(t, err, store.ErrNotFound, "invalidated")
}

func TestThrottleBackoff(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	p := store.ThrottlePolicy{FreeFailures: 5, Window: 15 * time.Minute, MaxBackoff: 15 * time.Minute}
	key := "login:" + uuid.NewString()
	now := time.Now().UTC().Truncate(time.Microsecond)

	until, err := f.s.Throttle().BlockedUntil(ctx, key, now)
	require.NoError(t, err)
	assert.True(t, until.IsZero())

	want := map[int]time.Duration{1: 0, 4: 0, 5: 2 * time.Second, 6: 4 * time.Second, 7: 8 * time.Second}
	for i := 1; i <= 7; i++ {
		st, err := f.s.Throttle().RecordFailure(ctx, key, now, p)
		require.NoError(t, err)
		assert.Equal(t, i, st.Failures)
		if d, ok := want[i]; ok {
			assert.WithinDuration(t, now.Add(d), st.BlockedUntil, time.Millisecond, "failure %d", i)
		}
	}
	until, err = f.s.Throttle().BlockedUntil(ctx, key, now)
	require.NoError(t, err)
	assert.False(t, until.IsZero())

	// Capped.
	for range 20 {
		_, err = f.s.Throttle().RecordFailure(ctx, key, now, p)
		require.NoError(t, err)
	}
	st, err := f.s.Throttle().RecordFailure(ctx, key, now, p)
	require.NoError(t, err)
	assert.WithinDuration(t, now.Add(15*time.Minute), st.BlockedUntil, time.Millisecond)

	// Window: failures older than 15 minutes start a new count.
	st, err = f.s.Throttle().RecordFailure(ctx, key, now.Add(16*time.Minute), p)
	require.NoError(t, err)
	assert.Equal(t, 1, st.Failures)

	require.NoError(t, f.s.Throttle().Reset(ctx, key))
	until, err = f.s.Throttle().BlockedUntil(ctx, key, now)
	require.NoError(t, err)
	assert.True(t, until.IsZero())
}

func TestThrottleCountsConcurrentFailuresExactly(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	p := store.ThrottlePolicy{FreeFailures: 5, Window: time.Hour, MaxBackoff: time.Hour}
	key := "login:" + uuid.NewString()
	now := time.Now().UTC()
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			_, err := f.s.Throttle().RecordFailure(ctx, key, now, p)
			assert.NoError(t, err)
		})
	}
	wg.Wait()
	st, err := f.s.Throttle().RecordFailure(ctx, key, now, p)
	require.NoError(t, err)
	assert.Equal(t, 21, st.Failures, "no lost updates across racing requests")
}
