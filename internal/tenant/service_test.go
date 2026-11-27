package tenant_test

import (
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ticatwolves/notifex/internal/crypto"
	"github.com/ticatwolves/notifex/internal/mail"
	"github.com/ticatwolves/notifex/internal/store"
	"github.com/ticatwolves/notifex/internal/store/entstore"
	"github.com/ticatwolves/notifex/internal/store/storetest"
	"github.com/ticatwolves/notifex/internal/tenant"
)

type env struct {
	st    *entstore.Store
	svc   *tenant.Service
	mail  *mail.Recorder
	now   time.Time
	mu    sync.Mutex
	shred []store.TenantID
}

func newEnv(t *testing.T) *env {
	t.Helper()
	mk := make([]byte, 32)
	_, _ = rand.Read(mk)
	kp, err := crypto.NewLocal(mk)
	require.NoError(t, err)
	e := &env{st: entstore.New(storetest.DB(t)), mail: &mail.Recorder{}, now: time.Now().UTC().Truncate(time.Microsecond)}
	e.svc = tenant.NewService(e.st, kp, e.mail, slog.New(slog.NewTextHandler(io.Discard, nil)), "https://console.test", tenant.DefaultPolicy)
	e.svc.SetClock(func() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return e.now })
	e.svc.OnShred = func(id store.TenantID) { e.mu.Lock(); e.shred = append(e.shred, id); e.mu.Unlock() }
	return e
}

func (e *env) advance(d time.Duration) { e.mu.Lock(); e.now = e.now.Add(d); e.mu.Unlock() }

// user creates a console user; recent means "re-authenticated just now".
func (e *env) user(t *testing.T, verified bool) tenant.User {
	t.Helper()
	in := store.NewUser{Email: "u" + strings.ReplaceAll(uuid.NewString()[:12], "-", "") + "@example.com"}
	if verified {
		in.EmailVerifiedAt = &e.now
	}
	u, err := e.st.Users().Create(context.Background(), in)
	require.NoError(t, err)
	return tenant.User{ID: u.ID, Email: u.Email, EmailVerified: verified, ReauthenticatedAt: e.now}
}

func (e *env) tenantFor(t *testing.T, owner tenant.User) store.Tenant {
	t.Helper()
	ten, err := e.svc.Create(context.Background(), owner, "Acme", "t-"+strings.ReplaceAll(uuid.NewString()[:10], "-", ""))
	require.NoError(t, err)
	return ten
}

// member adds u to the tenant with role (bypassing invitations) and returns
// u's actor there.
func (e *env) member(t *testing.T, ten store.Tenant, role store.Role) tenant.Actor {
	t.Helper()
	u := e.user(t, true)
	_, err := e.st.Memberships().Create(context.Background(), ten.ID, u.ID, role)
	require.NoError(t, err)
	return e.access(t, u, ten)
}

func (e *env) access(t *testing.T, u tenant.User, ten store.Tenant) tenant.Actor {
	t.Helper()
	a, _, err := e.svc.Access(context.Background(), u, ten.ID)
	require.NoError(t, err)
	return a
}

func TestCreateTenant(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()

	_, err := e.svc.Create(ctx, e.user(t, false), "Acme", "acme-"+uuid.NewString()[:6])
	require.ErrorIs(t, err, tenant.ErrEmailNotVerified)

	owner := e.user(t, true)
	ten := e.tenantFor(t, owner)
	assert.True(t, ten.Sandbox, "new tenants are sandboxed")
	a := e.access(t, owner, ten)
	assert.Equal(t, store.RoleOwner, a.Role, "creator becomes owner")

	_, err = e.svc.Create(ctx, owner, "Dup", ten.Slug)
	require.ErrorIs(t, err, store.ErrConflict, "slugs are unique")
	_, err = e.svc.Create(ctx, owner, "Bad", "Not A Slug")
	require.ErrorIs(t, err, store.ErrInvalid)
}

func TestTenantCapHoldsUnderConcurrency(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	u := e.user(t, true)
	for range 3 {
		e.tenantFor(t, u)
	}
	// 8 concurrent creates with 2 slots left: exactly 2 may succeed.
	var ok, limited atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			_, err := e.svc.Create(ctx, u, "Race", "r-"+strings.ReplaceAll(uuid.NewString()[:10], "-", ""))
			switch {
			case err == nil:
				ok.Add(1)
			case assert.ErrorIs(t, err, tenant.ErrTenantLimit):
				limited.Add(1)
			}
		})
	}
	wg.Wait()
	assert.Equal(t, int32(2), ok.Load())
	assert.Equal(t, int32(6), limited.Load())
}

func TestAccessHidesOtherAndDeletedTenants(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	owner := e.user(t, true)
	ten := e.tenantFor(t, owner)

	_, _, err := e.svc.Access(ctx, e.user(t, true), ten.ID)
	require.ErrorIs(t, err, store.ErrNotFound, "non-members get not-found, not forbidden")
	_, _, err = e.svc.Access(ctx, owner, store.NewTenantID())
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestRenameAndDelete(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	owner := e.user(t, true)
	ten := e.tenantFor(t, owner)
	o := e.access(t, owner, ten)
	viewer, admin := e.member(t, ten, store.RoleViewer), e.member(t, ten, store.RoleAdmin)

	_, err := e.svc.Rename(ctx, viewer, "Nope")
	require.ErrorIs(t, err, tenant.ErrForbidden)
	got, err := e.svc.Rename(ctx, admin, "Acme Corp")
	require.NoError(t, err)
	assert.Equal(t, "Acme Corp", got.Name)

	// Data that must be gone after deletion: an API key and the DEK.
	app, err := e.st.Applications().Create(ctx, ten.ID, store.NewApplication{Name: "Shop", Key: "shop"})
	require.NoError(t, err)
	env, err := e.st.Environments().Create(ctx, ten.ID, app.ID, store.EnvProd)
	require.NoError(t, err)
	key, err := e.st.APIKeys().Create(ctx, ten.ID, store.NewAPIKey{ApplicationID: app.ID, EnvironmentID: env.ID, Name: "k", Prefix: "p" + uuid.NewString()[:11], Hash: []byte("h")})
	require.NoError(t, err)

	require.ErrorIs(t, e.svc.Delete(ctx, admin), tenant.ErrForbidden, "admins can't delete")
	e.advance(11 * time.Minute)
	o.ReauthenticatedAt = e.now.Add(-11 * time.Minute)
	require.ErrorIs(t, e.svc.Delete(ctx, o), tenant.ErrReauthRequired, "owner without recent re-auth")
	o.ReauthenticatedAt = e.now
	require.NoError(t, e.svc.Delete(ctx, o))

	_, err = e.st.Tenants().WrappedDEK(ctx, ten.ID.UUID())
	require.ErrorIs(t, err, crypto.ErrDataKeyDestroyed, "crypto-shredded")
	k, err := e.st.APIKeys().GetByPrefix(ctx, key.Prefix)
	require.NoError(t, err)
	assert.NotNil(t, k.RevokedAt, "API keys revoked")
	_, _, err = e.svc.Access(ctx, owner, ten.ID)
	require.ErrorIs(t, err, store.ErrNotFound, "a deleted tenant is gone for everyone")
	assert.Equal(t, []store.TenantID{ten.ID}, e.shred, "cached keys dropped")

	// A deleted tenant no longer counts toward the cap.
	n, err := e.st.Memberships().CountOwnedBy(ctx, owner.ID)
	require.NoError(t, err)
	assert.Zero(t, n)
}

func TestRoleChangeRules(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		actor, target store.Role
		to            store.Role
		staleAuth     bool
		want          error
	}{
		{"viewer can't manage", store.RoleViewer, store.RoleDeveloper, store.RoleViewer, false, tenant.ErrForbidden},
		{"developer can't manage", store.RoleDeveloper, store.RoleViewer, store.RoleDeveloper, false, tenant.ErrForbidden},
		{"admin promotes developer to admin", store.RoleAdmin, store.RoleDeveloper, store.RoleAdmin, false, nil},
		{"admin demotes admin", store.RoleAdmin, store.RoleAdmin, store.RoleViewer, false, nil},
		{"admin can't grant owner", store.RoleAdmin, store.RoleDeveloper, store.RoleOwner, false, tenant.ErrForbidden},
		{"admin can't touch an owner", store.RoleAdmin, store.RoleOwner, store.RoleAdmin, false, tenant.ErrForbidden},
		{"owner promotes to owner", store.RoleOwner, store.RoleAdmin, store.RoleOwner, false, nil},
		{"promoting to owner needs recent re-auth", store.RoleOwner, store.RoleAdmin, store.RoleOwner, true, tenant.ErrReauthRequired},
		{"owner demotes another owner", store.RoleOwner, store.RoleOwner, store.RoleAdmin, false, nil},
		{"unknown role", store.RoleOwner, store.RoleViewer, store.Role("god"), false, store.ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			ten := e.tenantFor(t, e.user(t, true)) // creator stays an owner, so "last owner" never triggers here
			a := e.member(t, ten, tt.actor)
			if tt.staleAuth {
				a.ReauthenticatedAt = e.now.Add(-time.Hour)
			}
			target := e.member(t, ten, tt.target)
			m, err := e.svc.UpdateRole(context.Background(), a, target.ID, tt.to)
			if tt.want != nil {
				require.ErrorIs(t, err, tt.want)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.to, m.Membership.Role)
		})
	}
}

func TestLastOwnerIsProtected(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	u := e.user(t, true)
	ten := e.tenantFor(t, u)
	o := e.access(t, u, ten)

	_, err := e.svc.UpdateRole(ctx, o, u.ID, store.RoleAdmin)
	require.ErrorIs(t, err, tenant.ErrLastOwner, "can't demote yourself as the only owner")
	require.ErrorIs(t, e.svc.RemoveMember(ctx, o, u.ID), tenant.ErrLastOwner, "can't leave as the only owner")
	_, err = e.svc.UpdateRole(ctx, o, uuid.New(), store.RoleAdmin)
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestLastOwnerHoldsUnderConcurrentDemotion(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	ua := e.user(t, true)
	ten := e.tenantFor(t, ua)
	a := e.access(t, ua, ten)
	b := e.member(t, ten, store.RoleOwner)

	// Each owner demotes the other at the same moment. Without the owner
	// lock both would see two owners and the tenant would end with none.
	for range 10 {
		var wg sync.WaitGroup
		var ok atomic.Int32
		start := make(chan struct{})
		wg.Go(func() {
			<-start
			if _, err := e.svc.UpdateRole(ctx, a, b.ID, store.RoleAdmin); err == nil {
				ok.Add(1)
			}
		})
		wg.Go(func() {
			<-start
			if _, err := e.svc.UpdateRole(ctx, b, a.ID, store.RoleAdmin); err == nil {
				ok.Add(1)
			}
		})
		close(start)
		wg.Wait()
		require.Equal(t, int32(1), ok.Load(), "exactly one demotion wins")
		owners, err := countOwners(e, ten)
		require.NoError(t, err)
		require.Equal(t, 1, owners)
		// Reset: restore both as owners for the next round.
		for _, id := range []uuid.UUID{a.ID, b.ID} {
			_, err := e.st.Memberships().UpdateRole(ctx, ten.ID, id, store.RoleOwner)
			require.NoError(t, err)
		}
	}
}

func countOwners(e *env, ten store.Tenant) (int, error) {
	ms, err := e.st.Memberships().List(context.Background(), ten.ID, store.Page{Limit: store.MaxPageSize})
	n := 0
	for _, m := range ms {
		if m.Role == store.RoleOwner {
			n++
		}
	}
	return n, err
}

func TestRemoveMember(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	ten := e.tenantFor(t, e.user(t, true))
	admin, dev, viewer := e.member(t, ten, store.RoleAdmin), e.member(t, ten, store.RoleDeveloper), e.member(t, ten, store.RoleViewer)
	owner2 := e.member(t, ten, store.RoleOwner)

	require.ErrorIs(t, e.svc.RemoveMember(ctx, dev, viewer.ID), tenant.ErrForbidden, "developers can't remove others")
	require.ErrorIs(t, e.svc.RemoveMember(ctx, admin, owner2.ID), tenant.ErrForbidden, "admins can't remove owners")
	require.NoError(t, e.svc.RemoveMember(ctx, viewer, viewer.ID), "anyone may leave")
	require.NoError(t, e.svc.RemoveMember(ctx, admin, dev.ID))
	require.NoError(t, e.svc.RemoveMember(ctx, owner2, owner2.ID), "an owner may leave while another owner remains")
	_, _, err := e.svc.Access(ctx, dev.User, ten.ID)
	require.ErrorIs(t, err, store.ErrNotFound, "access ends immediately")
}

var tokenRE = regexp.MustCompile(`#token=([0-9A-Za-z]+)`)

func (e *env) inviteToken(t *testing.T, email string) string {
	t.Helper()
	m, ok := e.mail.Last(email)
	require.True(t, ok)
	match := tokenRE.FindStringSubmatch(m.Text)
	require.Len(t, match, 2)
	return match[1]
}

func TestInvitations(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	ten := e.tenantFor(t, e.user(t, true))
	admin, viewer := e.member(t, ten, store.RoleAdmin), e.member(t, ten, store.RoleViewer)

	_, err := e.svc.Invite(ctx, viewer, "x@example.com", store.RoleViewer)
	require.ErrorIs(t, err, tenant.ErrForbidden)
	_, err = e.svc.Invite(ctx, admin, "x@example.com", store.RoleOwner)
	require.ErrorIs(t, err, tenant.ErrForbidden, "admins can't invite owners")
	_, err = e.svc.Invite(ctx, admin, viewer.Email, store.RoleViewer)
	require.ErrorIs(t, err, tenant.ErrAlreadyMember)
	_, err = e.svc.Invite(ctx, admin, "not-an-email", store.RoleViewer)
	require.ErrorIs(t, err, store.ErrInvalid)

	invitee := e.user(t, false) // unverified: accepting proves the address
	_, err = e.svc.Invite(ctx, admin, strings.ToUpper(invitee.Email), store.RoleDeveloper)
	require.NoError(t, err)
	m, _ := e.mail.Last(invitee.Email)
	assert.Contains(t, m.Text, "https://console.test/invitations/accept#token=")
	first := e.inviteToken(t, invitee.Email)

	// Re-inviting replaces the pending invitation.
	_, err = e.svc.Invite(ctx, admin, invitee.Email, store.RoleDeveloper)
	require.NoError(t, err)
	second := e.inviteToken(t, invitee.Email)
	_, err = e.svc.Accept(ctx, invitee, first)
	require.ErrorIs(t, err, tenant.ErrInvalidToken, "the old link no longer works")
	pending, err := e.svc.ListInvitations(ctx, admin)
	require.NoError(t, err)
	require.Len(t, pending, 1)

	// Wrong account: rejected, and the link stays usable.
	_, err = e.svc.Accept(ctx, e.user(t, true), second)
	require.ErrorIs(t, err, tenant.ErrInviteEmail)
	mem, err := e.svc.Accept(ctx, invitee, second)
	require.NoError(t, err)
	assert.Equal(t, store.RoleDeveloper, mem.Membership.Role)
	assert.NotNil(t, mem.User.EmailVerifiedAt, "accepting verified the email")
	_, err = e.svc.Accept(ctx, invitee, second)
	require.ErrorIs(t, err, tenant.ErrInvalidToken, "single use")
}

func TestInvitationExpiryRevocationAndDeletedTenant(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	ownerU := e.user(t, true)
	ten := e.tenantFor(t, ownerU)
	o := e.access(t, ownerU, ten)

	a, b, c := e.user(t, true), e.user(t, true), e.user(t, true)
	invA, err := e.svc.Invite(ctx, o, a.Email, store.RoleViewer)
	require.NoError(t, err)
	tokA := e.inviteToken(t, a.Email)
	require.NoError(t, e.svc.RevokeInvitation(ctx, o, invA.ID))
	_, err = e.svc.Accept(ctx, a, tokA)
	require.ErrorIs(t, err, tenant.ErrInvalidToken, "revoked")
	require.ErrorIs(t, e.svc.RevokeInvitation(ctx, o, invA.ID), store.ErrNotFound)

	_, err = e.svc.Invite(ctx, o, b.Email, store.RoleViewer)
	require.NoError(t, err)
	tokB := e.inviteToken(t, b.Email)
	_, err = e.svc.Invite(ctx, o, c.Email, store.RoleViewer)
	require.NoError(t, err)
	tokC := e.inviteToken(t, c.Email)

	e.advance(7*24*time.Hour + time.Minute)
	_, err = e.svc.Accept(ctx, b, tokB)
	require.ErrorIs(t, err, tenant.ErrInvalidToken, "expired")

	o.ReauthenticatedAt = e.now
	require.NoError(t, e.svc.Delete(ctx, o))
	_, err = e.svc.Accept(ctx, c, tokC)
	require.ErrorIs(t, err, tenant.ErrInvalidToken, "no joining a deleted tenant")
}
