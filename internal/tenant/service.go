package tenant

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ticatwolves/notifex/internal/audit"
	"github.com/ticatwolves/notifex/internal/auth"
	"github.com/ticatwolves/notifex/internal/crypto"
	"github.com/ticatwolves/notifex/internal/mail"
	"github.com/ticatwolves/notifex/internal/observability"
	"github.com/ticatwolves/notifex/internal/rbac"
	"github.com/ticatwolves/notifex/internal/store"
)

// Errors. Transports map them to docs/03 codes (403 forbidden,
// reauth_required, email_not_verified, tenant_limit_reached; 409 conflict).
var (
	ErrForbidden        = errors.New("tenant: forbidden")
	ErrReauthRequired   = errors.New("tenant: re-authentication required")
	ErrEmailNotVerified = errors.New("tenant: email not verified")
	ErrTenantLimit      = errors.New("tenant: tenant limit reached")
	ErrLastOwner        = errors.New("tenant: a tenant must keep at least one owner")
	ErrAlreadyMember    = errors.New("tenant: already a member")
	ErrInvalidToken     = errors.New("tenant: invitation is invalid, expired or already used")
	ErrInviteEmail      = errors.New("tenant: this invitation was sent to a different email address")
)

// Policy holds the ADR-011 §4 limits.
type Policy struct {
	MaxOwnedTenants int
	ReauthWindow    time.Duration
	InvitationTTL   time.Duration
}

// DefaultPolicy is ADR-011 §4.
var DefaultPolicy = Policy{MaxOwnedTenants: 5, ReauthWindow: 10 * time.Minute, InvitationTTL: 7 * 24 * time.Hour}

// User is the signed-in console user (from the session).
type User struct {
	ID                uuid.UUID
	Email             string
	EmailVerified     bool
	ReauthenticatedAt time.Time
}

// Actor is a user acting inside one tenant, with their role there.
type Actor struct {
	User
	TenantID store.TenantID
	Role     store.Role
}

// Service implements tenant, member and invitation management.
type Service struct {
	st         store.Store
	keys       crypto.KeyProvider
	mailer     mail.Mailer
	log        *slog.Logger
	consoleURL string
	policy     Policy
	now        func() time.Time
	// OnShred is called after a tenant's DEK is destroyed (drop cached keys).
	OnShred func(store.TenantID)
	// KeyHasher hashes new API keys (required for key issue and rotation).
	KeyHasher *auth.Hasher
}

// NewService builds the service.
func NewService(st store.Store, keys crypto.KeyProvider, m mail.Mailer, log *slog.Logger, consoleURL string, p Policy) *Service {
	return &Service{st: st, keys: keys, mailer: m, log: log, consoleURL: strings.TrimRight(consoleURL, "/"), policy: p, now: time.Now}
}

// SetClock overrides the clock (tests).
func (s *Service) SetClock(now func() time.Time) { s.now = now }

func (s *Service) require(a Actor, c rbac.Capability) error {
	if !rbac.Allow(a.Role, c) {
		return fmt.Errorf("%w: %s requires %s", ErrForbidden, a.Role, c)
	}
	return nil
}

func (s *Service) requireRecentAuth(a Actor) error {
	if s.now().Sub(a.ReauthenticatedAt) >= s.policy.ReauthWindow {
		return ErrReauthRequired
	}
	return nil
}

func actorOf(u User) audit.Actor { return audit.Actor{Type: store.ActorUser, ID: u.ID.String()} }

// Access resolves u's role in tid. A non-member, or a deleted tenant, is
// ErrNotFound: callers must not learn whether the tenant exists.
func (s *Service) Access(ctx context.Context, u User, tid store.TenantID) (Actor, store.Tenant, error) {
	m, err := s.st.Memberships().Get(ctx, tid, u.ID)
	if err != nil {
		return Actor{}, store.Tenant{}, fmt.Errorf("tenant access: %w", err)
	}
	t, err := s.st.Tenants().Get(ctx, tid)
	if err != nil {
		return Actor{}, store.Tenant{}, fmt.Errorf("tenant access: %w", err)
	}
	if t.Status == store.TenantDeleted {
		return Actor{}, store.Tenant{}, fmt.Errorf("tenant access: %w", store.ErrNotFound)
	}
	return Actor{User: u, TenantID: tid, Role: m.Role}, t, nil
}

// Create provisions a tenant owned by u (ADR-011 §4: verified email, at most
// MaxOwnedTenants owned, sandboxed).
func (s *Service) Create(ctx context.Context, u User, name, slug string) (store.Tenant, error) {
	if !u.EmailVerified {
		return store.Tenant{}, ErrEmailNotVerified
	}
	var t store.Tenant
	err := s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		// Serialise this user's creates so the cap can't be raced.
		if err := tx.Users().Lock(ctx, u.ID); err != nil {
			return err
		}
		n, err := tx.Memberships().CountOwnedBy(ctx, u.ID)
		if err != nil {
			return err
		}
		if n >= s.policy.MaxOwnedTenants {
			return ErrTenantLimit
		}
		t, err = Provision(ctx, tx, s.keys, actorOf(u), ProvisionInput{Name: name, Slug: slug})
		if err != nil {
			return err
		}
		m, err := tx.Memberships().Create(ctx, t.ID, u.ID, store.RoleOwner)
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, actorOf(u), audit.Event{
			TenantID: &t.ID, Action: "membership.create", EntityType: "membership", EntityID: m.ID.String(),
			After: map[string]any{"user_id": u.ID.String(), "role": string(store.RoleOwner), "via": "tenant.create"},
		})
	})
	if err != nil {
		return store.Tenant{}, fmt.Errorf("create tenant: %w", err)
	}
	return t, nil
}

// Rename changes the tenant's display name.
func (s *Service) Rename(ctx context.Context, a Actor, name string) (store.Tenant, error) {
	if err := s.require(a, rbac.UpdateTenant); err != nil {
		return store.Tenant{}, err
	}
	name = strings.TrimSpace(name)
	var t store.Tenant
	err := s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		before, err := tx.Tenants().Get(ctx, a.TenantID)
		if err != nil {
			return err
		}
		t, err = tx.Tenants().Update(ctx, a.TenantID, store.TenantPatch{Name: &name})
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, actorOf(a.User), audit.Event{
			TenantID: &a.TenantID, Action: "tenant.update", EntityType: "tenant", EntityID: a.TenantID.String(),
			Before: map[string]any{"name": before.Name}, After: map[string]any{"name": t.Name},
		})
	})
	if err != nil {
		return store.Tenant{}, fmt.Errorf("rename tenant: %w", err)
	}
	return t, nil
}

// Delete marks the tenant deleted, revokes every API key and crypto-shreds its
// DEK (ADR-006), in one transaction. Owner only, with recent re-auth.
func (s *Service) Delete(ctx context.Context, a Actor) error {
	if err := s.require(a, rbac.DeleteTenant); err != nil {
		return err
	}
	if err := s.requireRecentAuth(a); err != nil {
		return err
	}
	now := s.now()
	err := s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		deleted := store.TenantDeleted
		if _, err := tx.Tenants().Update(ctx, a.TenantID, store.TenantPatch{Status: &deleted}); err != nil {
			return err
		}
		keys, err := tx.APIKeys().RevokeAllForTenant(ctx, a.TenantID, now)
		if err != nil {
			return err
		}
		if err := tx.Tenants().ShredDEK(ctx, a.TenantID); err != nil {
			return err
		}
		return audit.Record(ctx, tx, actorOf(a.User), audit.Event{
			TenantID: &a.TenantID, Action: "tenant.delete", EntityType: "tenant", EntityID: a.TenantID.String(),
			After: map[string]any{"status": string(deleted), "api_keys_revoked": keys, "dek": "shredded"},
		})
	})
	if err != nil {
		return fmt.Errorf("delete tenant: %w", err)
	}
	if s.OnShred != nil {
		s.OnShred(a.TenantID)
	}
	return nil
}

// Member is a membership with its user.
type Member struct {
	Membership store.Membership
	User       store.User
}

// ListAuditEvents returns the tenant's audit trail, newest first.
func (s *Service) ListAuditEvents(ctx context.Context, a Actor, p store.Page) ([]store.AuditEvent, error) {
	if err := s.require(a, rbac.ViewAudit); err != nil {
		return nil, err
	}
	evs, err := s.st.Audit().List(ctx, a.TenantID, p)
	if err != nil {
		return nil, fmt.Errorf("list audit events: %w", err)
	}
	return evs, nil
}

// ListMembers returns a page of members.
func (s *Service) ListMembers(ctx context.Context, a Actor, p store.Page) ([]Member, error) {
	if err := s.require(a, rbac.ViewTenant); err != nil {
		return nil, err
	}
	ms, err := s.st.Memberships().List(ctx, a.TenantID, p)
	if err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}
	out := make([]Member, 0, len(ms))
	for _, m := range ms {
		u, err := s.st.Users().Get(ctx, m.UserID)
		if err != nil {
			return nil, fmt.Errorf("list members: %w", err)
		}
		out = append(out, Member{Membership: m, User: u})
	}
	return out, nil
}

// UpdateRole changes a member's role (ADR-011 §4 rules).
func (s *Service) UpdateRole(ctx context.Context, a Actor, userID uuid.UUID, role store.Role) (Member, error) {
	if !rbac.Valid(role) {
		return Member{}, fmt.Errorf("%w: unknown role %q", store.ErrInvalid, role)
	}
	if err := s.require(a, rbac.ManageMembers); err != nil {
		return Member{}, err
	}
	if rbac.Rank(role) > rbac.Rank(a.Role) {
		return Member{}, fmt.Errorf("%w: cannot grant a role above your own", ErrForbidden)
	}
	if role == store.RoleOwner {
		if err := s.require(a, rbac.TransferOwnership); err != nil {
			return Member{}, err
		}
		if err := s.requireRecentAuth(a); err != nil {
			return Member{}, err
		}
	}
	var out Member
	err := s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		owners, err := tx.Memberships().LockOwners(ctx, a.TenantID)
		if err != nil {
			return err
		}
		target, err := tx.Memberships().Get(ctx, a.TenantID, userID)
		if err != nil {
			return err
		}
		if target.Role == store.RoleOwner && a.Role != store.RoleOwner {
			return fmt.Errorf("%w: only owners can change an owner", ErrForbidden)
		}
		if target.Role == store.RoleOwner && role != store.RoleOwner && len(owners) <= 1 {
			return ErrLastOwner
		}
		if target.Role == role {
			out.Membership = target
		} else {
			if out.Membership, err = tx.Memberships().UpdateRole(ctx, a.TenantID, userID, role); err != nil {
				return err
			}
			if err := audit.Record(ctx, tx, actorOf(a.User), audit.Event{
				TenantID: &a.TenantID, Action: "membership.update_role", EntityType: "membership", EntityID: target.ID.String(),
				Before: map[string]any{"role": string(target.Role)}, After: map[string]any{"role": string(role)},
			}); err != nil {
				return err
			}
		}
		out.User, err = tx.Users().Get(ctx, userID)
		return err
	})
	if err != nil {
		return Member{}, fmt.Errorf("update role: %w", err)
	}
	return out, nil
}

// RemoveMember removes a member, or lets a member leave (userID == self).
func (s *Service) RemoveMember(ctx context.Context, a Actor, userID uuid.UUID) error {
	self := userID == a.ID
	if !self {
		if err := s.require(a, rbac.ManageMembers); err != nil {
			return err
		}
	}
	err := s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		owners, err := tx.Memberships().LockOwners(ctx, a.TenantID)
		if err != nil {
			return err
		}
		target, err := tx.Memberships().Get(ctx, a.TenantID, userID)
		if err != nil {
			return err
		}
		if !self && target.Role == store.RoleOwner && a.Role != store.RoleOwner {
			return fmt.Errorf("%w: only owners can remove an owner", ErrForbidden)
		}
		if target.Role == store.RoleOwner && len(owners) <= 1 {
			return ErrLastOwner
		}
		if err := tx.Memberships().Delete(ctx, a.TenantID, userID); err != nil {
			return err
		}
		action := "membership.delete"
		if self {
			action = "membership.leave"
		}
		return audit.Record(ctx, tx, actorOf(a.User), audit.Event{
			TenantID: &a.TenantID, Action: action, EntityType: "membership", EntityID: target.ID.String(),
			Before: map[string]any{"user_id": userID.String(), "role": string(target.Role)},
		})
	})
	if err != nil {
		return fmt.Errorf("remove member: %w", err)
	}
	return nil
}

// Invite emails an invitation (ADR-011 §4). Re-inviting an email replaces
// its live invitation; the old link stops working.
func (s *Service) Invite(ctx context.Context, a Actor, email string, role store.Role) (store.Invitation, error) {
	if !rbac.Valid(role) {
		return store.Invitation{}, fmt.Errorf("%w: unknown role %q", store.ErrInvalid, role)
	}
	if err := s.require(a, rbac.ManageMembers); err != nil {
		return store.Invitation{}, err
	}
	if rbac.Rank(role) > rbac.Rank(a.Role) {
		return store.Invitation{}, fmt.Errorf("%w: cannot invite with a role above your own", ErrForbidden)
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if at := strings.IndexByte(email, '@'); at <= 0 || at == len(email)-1 || strings.ContainsAny(email, " \r\n<>") {
		return store.Invitation{}, fmt.Errorf("%w: email", store.ErrInvalid)
	}
	now := s.now()
	token, hash := newToken()
	var inv store.Invitation
	var tenantName string
	err := s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if u, err := tx.Users().GetByEmail(ctx, email); err == nil {
			if _, err := tx.Memberships().Get(ctx, a.TenantID, u.ID); err == nil {
				return ErrAlreadyMember
			} else if !errors.Is(err, store.ErrNotFound) {
				return err
			}
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if _, err := tx.Invitations().RevokeLiveForEmail(ctx, a.TenantID, email, now); err != nil {
			return err
		}
		var err error
		inv, err = tx.Invitations().Create(ctx, a.TenantID, store.NewInvitation{
			Email: email, Role: role, TokenHash: hash, InvitedBy: a.ID, Now: now, ExpiresAt: now.Add(s.policy.InvitationTTL),
		})
		if err != nil {
			return err
		}
		t, err := tx.Tenants().Get(ctx, a.TenantID)
		if err != nil {
			return err
		}
		tenantName = t.Name
		return audit.Record(ctx, tx, actorOf(a.User), audit.Event{
			TenantID: &a.TenantID, Action: "invitation.create", EntityType: "invitation", EntityID: inv.ID.String(),
			After: map[string]any{"email": observability.MaskEmail(email), "role": string(role)},
		})
	})
	if err != nil {
		return store.Invitation{}, fmt.Errorf("invite: %w", err)
	}
	if err := s.mailer.Send(ctx, mail.Message{
		To:      email,
		Subject: "You're invited to " + oneLine(tenantName) + " on Notifex",
		Text: "You've been invited to join " + oneLine(tenantName) + " on Notifex as " + string(role) + ".\n\n" +
			"Accept the invitation (sign in or create an account with this email address):\n\n" +
			s.consoleURL + "/invitations/accept#token=" + token + "\n\nThe link expires in 7 days.\n",
	}); err != nil {
		s.log.ErrorContext(ctx, "invitation mail not sent", slog.String("to", observability.MaskEmail(email)), slog.Any("error", err))
	}
	return inv, nil
}

// ListInvitations returns live invitations.
func (s *Service) ListInvitations(ctx context.Context, a Actor) ([]store.Invitation, error) {
	if err := s.require(a, rbac.ManageMembers); err != nil {
		return nil, err
	}
	invs, err := s.st.Invitations().ListPending(ctx, a.TenantID, s.now())
	if err != nil {
		return nil, fmt.Errorf("list invitations: %w", err)
	}
	return invs, nil
}

// RevokeInvitation revokes a live invitation.
func (s *Service) RevokeInvitation(ctx context.Context, a Actor, id uuid.UUID) error {
	if err := s.require(a, rbac.ManageMembers); err != nil {
		return err
	}
	return wrap("revoke invitation", s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.Invitations().Revoke(ctx, a.TenantID, id, s.now()); err != nil {
			return err
		}
		return audit.Record(ctx, tx, actorOf(a.User), audit.Event{
			TenantID: &a.TenantID, Action: "invitation.revoke", EntityType: "invitation", EntityID: id.String(),
		})
	}))
}

// Accept joins u to the invitation's tenant. The session's email must match
// the invited address; a mismatch (or any failure) leaves the link usable.
func (s *Service) Accept(ctx context.Context, u User, token string) (Member, error) {
	now := s.now()
	var out Member
	err := s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		inv, err := tx.Invitations().Consume(ctx, hashToken(token), u.ID, now)
		if errors.Is(err, store.ErrNotFound) {
			return ErrInvalidToken
		}
		if err != nil {
			return err
		}
		if !strings.EqualFold(inv.Email, u.Email) {
			return ErrInviteEmail // rolls back: the invitation stays valid
		}
		t, err := tx.Tenants().Get(ctx, inv.TenantID)
		if err != nil {
			return err
		}
		if t.Status == store.TenantDeleted {
			return ErrInvalidToken
		}
		if _, err := tx.Memberships().Get(ctx, inv.TenantID, u.ID); err == nil {
			return ErrAlreadyMember
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		m, err := tx.Memberships().Create(ctx, inv.TenantID, u.ID, inv.Role)
		if err != nil {
			return err
		}
		// The link was delivered to this address: that proves it.
		if err := tx.Users().MarkEmailVerified(ctx, u.ID, now); err != nil {
			return err
		}
		if out.User, err = tx.Users().Get(ctx, u.ID); err != nil {
			return err
		}
		out.Membership = m
		return audit.Record(ctx, tx, actorOf(u), audit.Event{
			TenantID: &inv.TenantID, Action: "invitation.accept", EntityType: "membership", EntityID: m.ID.String(),
			After: map[string]any{"invitation_id": inv.ID.String(), "role": string(inv.Role)},
		})
	})
	if err != nil {
		return Member{}, fmt.Errorf("accept invitation: %w", err)
	}
	return out, nil
}

func wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", op, err)
}

// oneLine strips characters that could break an email subject line.
func oneLine(s string) string { return strings.NewReplacer("\r", " ", "\n", " ").Replace(s) }

const tokenAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

func newToken() (string, []byte) {
	b := make([]byte, 43)
	max := big.NewInt(int64(len(tokenAlphabet)))
	for i := range b {
		n, _ := rand.Int(rand.Reader, max)
		b[i] = tokenAlphabet[n.Int64()]
	}
	t := string(b)
	return t, hashToken(t)
}

func hashToken(t string) []byte {
	sum := sha256.Sum256([]byte(t))
	return sum[:]
}
