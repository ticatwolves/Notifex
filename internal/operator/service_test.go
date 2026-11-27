package operator_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ticatwolves/notifex/internal/audit"
	"github.com/ticatwolves/notifex/internal/auth"
	"github.com/ticatwolves/notifex/internal/crypto"
	"github.com/ticatwolves/notifex/internal/operator"
	"github.com/ticatwolves/notifex/internal/store"
	"github.com/ticatwolves/notifex/internal/store/entstore"
	"github.com/ticatwolves/notifex/internal/store/storetest"
	"github.com/ticatwolves/notifex/internal/tenant"
)

const pw = "correct horse battery staple 42"

type env struct {
	db  *sql.DB
	st  *entstore.Store
	kp  crypto.KeyProvider
	svc *operator.Service
	mu  sync.Mutex
	now time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	mk := make([]byte, 32)
	_, _ = rand.Read(mk)
	kp, err := crypto.NewLocal(mk)
	require.NoError(t, err)
	db := storetest.DB(t)
	e := &env{db: db, st: entstore.New(db), kp: kp, now: time.Now().UTC().Truncate(time.Microsecond)}
	e.svc = operator.NewService(e.st, kp, auth.NewPasswordHasher(4), slog.New(slog.NewTextHandler(io.Discard, nil)), operator.DefaultPolicy)
	e.svc.SetClock(e.clock)
	return e
}

func (e *env) clock() time.Time        { e.mu.Lock(); defer e.mu.Unlock(); return e.now }
func (e *env) advance(d time.Duration) { e.mu.Lock(); e.now = e.now.Add(d); e.mu.Unlock() }

func newEmail() string {
	return "op" + strings.ReplaceAll(uuid.NewString()[:12], "-", "") + "@notifex.test"
}

// operatorAt creates an operator and returns its email and enrollment URI.
func (e *env) operatorAt(t *testing.T) (store.Operator, string) {
	t.Helper()
	op, uri, err := e.svc.Create(context.Background(), newEmail(), "Ada", pw)
	require.NoError(t, err)
	return op, uri
}

func (e *env) code(t *testing.T, uri string) string {
	t.Helper()
	c, err := operator.CodeFromURI(uri, e.clock())
	require.NoError(t, err)
	return c
}

func (e *env) login(t *testing.T, op store.Operator, uri string) (string, operator.Principal) {
	t.Helper()
	tok, _, err := e.svc.Login(context.Background(), op.Email, pw, e.code(t, uri), "test")
	require.NoError(t, err)
	p, err := e.svc.Authenticate(context.Background(), tok)
	require.NoError(t, err)
	return tok, p
}

// platformActions lists audit actions recorded for an operator (or entity).
func (e *env) actions(t *testing.T, actorOrEntity string) []string {
	t.Helper()
	rows, err := e.db.QueryContext(context.Background(),
		`SELECT action FROM audit_events WHERE actor_id = $1 OR entity_id = $1 ORDER BY created_at, id`, actorOrEntity)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var a string
		require.NoError(t, rows.Scan(&a))
		out = append(out, a)
	}
	require.NoError(t, rows.Err())
	return out
}

func TestCreateValidatesAndSealsSecret(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()

	_, _, err := e.svc.Create(ctx, "not-an-email", "x", pw)
	require.ErrorIs(t, err, store.ErrInvalid)
	_, _, err = e.svc.Create(ctx, newEmail(), "x", "short")
	require.ErrorIs(t, err, auth.ErrWeakPassword)

	op, uri := e.operatorAt(t)
	assert.True(t, strings.HasPrefix(uri, "otpauth://totp/"))
	assert.Equal(t, store.OperatorActive, op.Status)
	// The secret is never stored in plaintext.
	stored, err := e.st.Operators().Get(ctx, op.ID)
	require.NoError(t, err)
	assert.NotEmpty(t, stored.TOTPCiphertext)
	assert.NotContains(t, uri, string(stored.TOTPCiphertext))

	_, _, err = e.svc.Create(ctx, op.Email, "dup", pw)
	require.ErrorIs(t, err, store.ErrConflict)
	assert.Equal(t, []string{"operator.create"}, e.actions(t, op.ID.String()))
}

func TestLoginRequiresBothFactorsAndFailsUniformly(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	op, uri := e.operatorAt(t)
	good := e.code(t, uri)
	bad := "000000"
	if good == bad {
		bad = "111111"
	}

	cases := []struct {
		name, email, password, code string
	}{
		{"unknown email", newEmail(), pw, good},
		{"wrong password", op.Email, pw + "x", good},
		{"wrong code", op.Email, pw, bad},
		{"empty code", op.Email, pw, ""},
		{"code with letters", op.Email, pw, "12a456"},
	}
	for _, tc := range cases {
		_, _, err := e.svc.Login(ctx, tc.email, tc.password, tc.code, "ua")
		require.ErrorIs(t, err, auth.ErrInvalidCredentials, tc.name)
	}
	// A failed login never consumed the code: the right one still works.
	tok, sess, err := e.svc.Login(ctx, strings.ToUpper(op.Email), pw, good, "ua")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(tok, "nfo_"))
	assert.Equal(t, e.clock().Add(8*time.Hour), sess.AbsoluteExpiresAt)
}

func TestTOTPCodeIsSingleUse(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	op, uri := e.operatorAt(t)
	code := e.code(t, uri)
	_, _, err := e.svc.Login(ctx, op.Email, pw, code, "ua")
	require.NoError(t, err)

	// Same code again (still within the window): replay.
	_, _, err = e.svc.Login(ctx, op.Email, pw, code, "ua")
	require.ErrorIs(t, err, auth.ErrInvalidCredentials)
	// The previous step's code is also dead once a later step was used.
	prev, err := operator.CodeFromURI(uri, e.clock().Add(-30*time.Second))
	require.NoError(t, err)
	_, _, err = e.svc.Login(ctx, op.Email, pw, prev, "ua")
	require.ErrorIs(t, err, auth.ErrInvalidCredentials)

	// The next step's code works.
	e.advance(30 * time.Second)
	_, _, err = e.svc.Login(ctx, op.Email, pw, e.code(t, uri), "ua")
	require.NoError(t, err)
}

// TestTOTPReplayUnderConcurrency: N concurrent logins with one code yield
// exactly one session (the conditional step update is the claim).
func TestTOTPReplayUnderConcurrency(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	op, uri := e.operatorAt(t)
	code := e.code(t, uri)
	var ok, denied atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := e.svc.Login(context.Background(), op.Email, pw, code, "ua")
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, auth.ErrInvalidCredentials):
				denied.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, int32(1), ok.Load())
	assert.Equal(t, int32(7), denied.Load())
}

func TestLoginBackoff(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	op, uri := e.operatorAt(t)
	for range operator.DefaultPolicy.Login.FreeFailures + 1 {
		_, _, _ = e.svc.Login(ctx, op.Email, "wrong password!!", "123456", "ua")
	}
	_, _, err := e.svc.Login(ctx, op.Email, pw, e.code(t, uri), "ua")
	var locked *auth.LockedError
	require.ErrorAs(t, err, &locked)
	assert.Positive(t, locked.RetryAfter)
}

func TestSessionLifetimes(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	op, uri := e.operatorAt(t)

	t.Run("idle", func(t *testing.T) {
		tok, _ := e.login(t, op, uri)
		e.advance(29 * time.Minute)
		_, err := e.svc.Authenticate(ctx, tok) // touches
		require.NoError(t, err)
		e.advance(30 * time.Minute)
		_, err = e.svc.Authenticate(ctx, tok)
		require.ErrorIs(t, err, auth.ErrUnauthenticated)
	})
	t.Run("absolute", func(t *testing.T) {
		e.advance(time.Minute) // fresh TOTP step
		tok, _ := e.login(t, op, uri)
		for range 20 { // 20×29m > 8h
			e.advance(29 * time.Minute)
			if _, err := e.svc.Authenticate(ctx, tok); err != nil {
				require.ErrorIs(t, err, auth.ErrUnauthenticated)
				return
			}
		}
		t.Fatal("session outlived the 8h absolute limit")
	})
	t.Run("logout", func(t *testing.T) {
		e.advance(time.Minute)
		tok, p := e.login(t, op, uri)
		require.NoError(t, e.svc.Logout(ctx, p))
		_, err := e.svc.Authenticate(ctx, tok)
		require.ErrorIs(t, err, auth.ErrUnauthenticated)
	})
	t.Run("other realms' tokens", func(t *testing.T) {
		for _, tok := range []string{"nfs_" + strings.Repeat("a", 43), "nfx_live_abc_def", "", "nfo_short"} {
			_, err := e.svc.Authenticate(ctx, tok)
			require.ErrorIs(t, err, auth.ErrUnauthenticated, tok)
		}
	})
}

func TestDisableRevokesAndBlocks(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	op, uri := e.operatorAt(t)
	tok, _ := e.login(t, op, uri)

	require.NoError(t, e.svc.Disable(ctx, op.Email))
	_, err := e.svc.Authenticate(ctx, tok)
	require.ErrorIs(t, err, auth.ErrUnauthenticated)
	e.advance(time.Minute)
	_, _, err = e.svc.Login(ctx, op.Email, pw, e.code(t, uri), "ua")
	require.ErrorIs(t, err, auth.ErrInvalidCredentials)
	require.ErrorIs(t, e.svc.Disable(ctx, newEmail()), store.ErrNotFound)
}

func (e *env) tenant(t *testing.T, name string) store.Tenant {
	t.Helper()
	ten, err := tenant.Provision(context.Background(), e.st, e.kp, audit.System, tenant.ProvisionInput{
		Name: name, Slug: "t-" + strings.ReplaceAll(uuid.NewString()[:10], "-", ""), Plan: "free",
	})
	require.NoError(t, err)
	return ten
}

func TestTenantReadsAreAudited(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	op, uri := e.operatorAt(t)
	_, p := e.login(t, op, uri)
	ten := e.tenant(t, "Audited Reads Co")

	got, err := e.svc.GetTenant(ctx, p, ten.ID)
	require.NoError(t, err)
	assert.Equal(t, ten.ID, got.ID)
	list, err := e.svc.SearchTenants(ctx, p, ten.Slug, nil, store.Page{})
	require.NoError(t, err)
	require.Len(t, list, 1)
	_, err = e.svc.GetTenant(ctx, p, store.NewTenantID())
	require.ErrorIs(t, err, store.ErrNotFound)

	assert.Equal(t, []string{"operator.create", "operator.login", "operator.tenant.view", "operator.tenant.search", "operator.tenant.view"},
		e.actions(t, op.ID.String()), "every operator read is audited, misses included")
}

func TestUpdateTenant(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	op, uri := e.operatorAt(t)
	_, p := e.login(t, op, uri)
	ten := e.tenant(t, "Limits Co")
	ptr := func(s store.TenantStatus) *store.TenantStatus { return &s }
	intp := func(n int) *int { return &n }
	boolp := func(b bool) *bool { return &b }
	strp := func(s string) *string { return &s }

	invalid := []struct {
		name  string
		patch operator.TenantPatch
		err   error
	}{
		{"no reason", operator.TenantPatch{Sandbox: boolp(false)}, operator.ErrReasonRequired},
		{"blank reason", operator.TenantPatch{Sandbox: boolp(false), Reason: "  "}, operator.ErrReasonRequired},
		{"nothing to change", operator.TenantPatch{Reason: "x"}, operator.ErrNothingToUpdate},
		{"delete via status", operator.TenantPatch{Status: ptr(store.TenantDeleted), Reason: "x"}, store.ErrInvalid},
		{"negative sms limit", operator.TenantPatch{PlatformSMSDailyLimit: intp(-1), Reason: "x"}, store.ErrInvalid},
		{"negative quota", operator.TenantPatch{Quotas: map[string]int64{"apps": -1}, Reason: "x"}, store.ErrInvalid},
		{"empty plan", operator.TenantPatch{Plan: strp(" "), Reason: "x"}, store.ErrInvalid},
	}
	for _, tc := range invalid {
		_, err := e.svc.UpdateTenant(ctx, p, ten.ID, tc.patch)
		require.ErrorIs(t, err, tc.err, tc.name)
	}

	got, err := e.svc.UpdateTenant(ctx, p, ten.ID, operator.TenantPatch{
		Status: ptr(store.TenantSuspended), Plan: strp("growth"), Sandbox: boolp(false),
		PlatformSMSDailyLimit: intp(500), Quotas: map[string]int64{"apps": 10}, Reason: "design partner onboarding",
	})
	require.NoError(t, err)
	assert.Equal(t, store.TenantSuspended, got.Status)
	assert.Equal(t, "growth", got.Plan)
	assert.False(t, got.Sandbox)
	assert.Equal(t, 500, got.PlatformSMSDailyLimit)
	assert.Equal(t, map[string]int64{"apps": 10}, got.Quotas)

	// The audit event carries before, after and the reason.
	var before, after string
	require.NoError(t, e.db.QueryRowContext(ctx,
		`SELECT before::text, after::text FROM audit_events WHERE action = 'operator.tenant.update' AND entity_id = $1`,
		ten.ID.String()).Scan(&before, &after))
	assert.Contains(t, before, `"sandbox": true`)
	assert.Contains(t, after, `"reason": "design partner onboarding"`)
	assert.Contains(t, after, `"status": "suspended"`)

	// Deleted tenants can't be changed; unknown ones are 404.
	require.NoError(t, e.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		_, err := tx.Tenants().Update(ctx, ten.ID, store.TenantPatch{Status: ptr(store.TenantDeleted)})
		return err
	}))
	_, err = e.svc.UpdateTenant(ctx, p, ten.ID, operator.TenantPatch{Status: ptr(store.TenantActive), Reason: "undo"})
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = e.svc.UpdateTenant(ctx, p, store.NewTenantID(), operator.TenantPatch{Sandbox: boolp(true), Reason: "x"})
	require.ErrorIs(t, err, store.ErrNotFound)
}
