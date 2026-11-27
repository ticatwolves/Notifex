package tenant

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ticatwolves/notifex/internal/audit"
	"github.com/ticatwolves/notifex/internal/auth"
	"github.com/ticatwolves/notifex/internal/rbac"
	"github.com/ticatwolves/notifex/internal/store"
)

// API key lifecycle limits.
const (
	DefaultRotationGrace = 24 * time.Hour
	MaxRotationGrace     = 7 * 24 * time.Hour
	MaxKeyLifetime       = 2 * 365 * 24 * time.Hour
	maxScopes            = 32
)

// ErrKeyInactive means the key is already revoked (or scheduled to be).
var ErrKeyInactive = errors.New("tenant: api key is already revoked or rotated")

var scopeRE = regexp.MustCompile(`^[a-z][a-z0-9_.:-]{0,63}$`)

// ---- applications ----

// ListApplications returns a page of the tenant's applications.
func (s *Service) ListApplications(ctx context.Context, a Actor, p store.Page) ([]store.Application, error) {
	if err := s.require(a, rbac.ViewTenant); err != nil {
		return nil, err
	}
	apps, err := s.st.Applications().List(ctx, a.TenantID, p)
	return apps, wrap("list applications", err)
}

// GetApplication returns one application.
func (s *Service) GetApplication(ctx context.Context, a Actor, id uuid.UUID) (store.Application, error) {
	if err := s.require(a, rbac.ViewTenant); err != nil {
		return store.Application{}, err
	}
	app, err := s.st.Applications().Get(ctx, a.TenantID, id)
	return app, wrap("get application", err)
}

// CreateApplication creates an application.
func (s *Service) CreateApplication(ctx context.Context, a Actor, in store.NewApplication) (store.Application, error) {
	if err := s.require(a, rbac.ManageApplications); err != nil {
		return store.Application{}, err
	}
	in.Name = strings.TrimSpace(in.Name)
	var app store.Application
	err := s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		var err error
		if app, err = tx.Applications().Create(ctx, a.TenantID, in); err != nil {
			return err
		}
		return audit.Record(ctx, tx, actorOf(a.User), audit.Event{
			TenantID: &a.TenantID, Action: "application.create", EntityType: "application", EntityID: app.ID.String(),
			After: map[string]any{"name": app.Name, "key": app.Key},
		})
	})
	return app, wrap("create application", err)
}

// UpdateApplication renames or re-describes an application.
func (s *Service) UpdateApplication(ctx context.Context, a Actor, id uuid.UUID, p store.ApplicationPatch) (store.Application, error) {
	if err := s.require(a, rbac.ManageApplications); err != nil {
		return store.Application{}, err
	}
	var app store.Application
	err := s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		before, err := tx.Applications().Get(ctx, a.TenantID, id)
		if err != nil {
			return err
		}
		if app, err = tx.Applications().Update(ctx, a.TenantID, id, p); err != nil {
			return err
		}
		return audit.Record(ctx, tx, actorOf(a.User), audit.Event{
			TenantID: &a.TenantID, Action: "application.update", EntityType: "application", EntityID: id.String(),
			Before: map[string]any{"name": before.Name, "description": before.Description},
			After:  map[string]any{"name": app.Name, "description": app.Description},
		})
	})
	return app, wrap("update application", err)
}

// DeleteApplication deletes an application with no environments left
// (store.ErrConflict otherwise).
func (s *Service) DeleteApplication(ctx context.Context, a Actor, id uuid.UUID) error {
	if err := s.require(a, rbac.DeleteApplications); err != nil {
		return err
	}
	return wrap("delete application", s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		app, err := tx.Applications().Get(ctx, a.TenantID, id)
		if err != nil {
			return err
		}
		if err := tx.Applications().Delete(ctx, a.TenantID, id); err != nil {
			return err
		}
		return audit.Record(ctx, tx, actorOf(a.User), audit.Event{
			TenantID: &a.TenantID, Action: "application.delete", EntityType: "application", EntityID: id.String(),
			Before: map[string]any{"name": app.Name, "key": app.Key},
		})
	}))
}

// ---- environments ----

// ListEnvironments returns an application's environments.
func (s *Service) ListEnvironments(ctx context.Context, a Actor, appID uuid.UUID) ([]store.Environment, error) {
	if err := s.require(a, rbac.ViewTenant); err != nil {
		return nil, err
	}
	if _, err := s.st.Applications().Get(ctx, a.TenantID, appID); err != nil {
		return nil, wrap("list environments", err)
	}
	envs, err := s.st.Environments().List(ctx, a.TenantID, appID)
	return envs, wrap("list environments", err)
}

// CreateEnvironment adds an environment to an application.
func (s *Service) CreateEnvironment(ctx context.Context, a Actor, appID uuid.UUID, name store.EnvironmentName) (store.Environment, error) {
	if err := s.require(a, rbac.ManageApplications); err != nil {
		return store.Environment{}, err
	}
	var env store.Environment
	err := s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		var err error
		if env, err = tx.Environments().Create(ctx, a.TenantID, appID, name); err != nil {
			return err
		}
		return audit.Record(ctx, tx, actorOf(a.User), audit.Event{
			TenantID: &a.TenantID, Action: "environment.create", EntityType: "environment", EntityID: env.ID.String(),
			After: map[string]any{"application_id": appID.String(), "name": string(name)},
		})
	})
	return env, wrap("create environment", err)
}

// DeleteEnvironment deletes an environment with no active API keys. Revoked
// and expired key rows are removed with it (their history is in the audit log).
func (s *Service) DeleteEnvironment(ctx context.Context, a Actor, appID, envID uuid.UUID) error {
	if err := s.require(a, rbac.DeleteApplications); err != nil {
		return err
	}
	now := s.now()
	return wrap("delete environment", s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		env, err := s.envOf(ctx, tx, a, appID, envID)
		if err != nil {
			return err
		}
		active, err := tx.APIKeys().CountActiveForEnvironment(ctx, a.TenantID, envID, now)
		if err != nil {
			return err
		}
		if active > 0 {
			return fmt.Errorf("%w: environment has %d active API key(s); revoke them first", store.ErrConflict, active)
		}
		removed, err := tx.APIKeys().DeleteInactiveForEnvironment(ctx, a.TenantID, envID, now)
		if err != nil {
			return err
		}
		if err := tx.Environments().Delete(ctx, a.TenantID, envID); err != nil {
			return err
		}
		return audit.Record(ctx, tx, actorOf(a.User), audit.Event{
			TenantID: &a.TenantID, Action: "environment.delete", EntityType: "environment", EntityID: envID.String(),
			Before: map[string]any{"application_id": appID.String(), "name": string(env.Name), "inactive_keys_removed": removed},
		})
	}))
}

// envOf loads an environment and checks it belongs to appID (ErrNotFound if not).
func (s *Service) envOf(ctx context.Context, st store.Store, a Actor, appID, envID uuid.UUID) (store.Environment, error) {
	env, err := st.Environments().Get(ctx, a.TenantID, envID)
	if err != nil {
		return store.Environment{}, err
	}
	if env.ApplicationID != appID {
		return store.Environment{}, store.ErrNotFound
	}
	return env, nil
}

// ---- API keys ----

// keyCapability: developers manage non-prod keys only (docs/03 matrix).
func keyCapability(env store.EnvironmentName) rbac.Capability {
	if env == store.EnvProd {
		return rbac.ManageAPIKeysProd
	}
	return rbac.ManageAPIKeysNonProd
}

// ListAPIKeys lists an application's keys (metadata only; developer+).
func (s *Service) ListAPIKeys(ctx context.Context, a Actor, appID uuid.UUID, p store.Page) ([]store.APIKey, error) {
	if err := s.require(a, rbac.ManageAPIKeysNonProd); err != nil {
		return nil, err
	}
	if _, err := s.st.Applications().Get(ctx, a.TenantID, appID); err != nil {
		return nil, wrap("list api keys", err)
	}
	keys, err := s.st.APIKeys().List(ctx, a.TenantID, appID, p)
	return keys, wrap("list api keys", err)
}

// KeyInput describes a new API key.
type KeyInput struct {
	EnvironmentID uuid.UUID
	Name          string
	Scopes        []string
	ExpiresAt     *time.Time
}

// CreateAPIKey issues a key; the plaintext is in the result, once.
func (s *Service) CreateAPIKey(ctx context.Context, a Actor, appID uuid.UUID, in KeyInput) (auth.Issued, error) {
	if err := s.validateKeyInput(in); err != nil {
		return auth.Issued{}, err
	}
	env, err := s.envOf(ctx, s.st, a, appID, in.EnvironmentID)
	if err != nil {
		return auth.Issued{}, wrap("create api key", err)
	}
	if err := s.require(a, keyCapability(env.Name)); err != nil {
		return auth.Issued{}, err
	}
	k, err := auth.IssueAPIKey(ctx, s.st, s.KeyHasher, actorOf(a.User), a.TenantID, auth.IssueInput{
		ApplicationID: appID, EnvironmentID: env.ID, Name: strings.TrimSpace(in.Name), Scopes: in.Scopes, ExpiresAt: in.ExpiresAt,
	})
	return k, wrap("create api key", err)
}

func (s *Service) validateKeyInput(in KeyInput) error {
	if strings.TrimSpace(in.Name) == "" || len(in.Name) > 200 {
		return fmt.Errorf("%w: name must be 1-200 characters", store.ErrInvalid)
	}
	if len(in.Scopes) > maxScopes {
		return fmt.Errorf("%w: at most %d scopes", store.ErrInvalid, maxScopes)
	}
	for _, sc := range in.Scopes {
		if !scopeRE.MatchString(sc) {
			return fmt.Errorf("%w: invalid scope %q", store.ErrInvalid, sc)
		}
	}
	if in.ExpiresAt != nil {
		now := s.now()
		if !in.ExpiresAt.After(now) || in.ExpiresAt.After(now.Add(MaxKeyLifetime)) {
			return fmt.Errorf("%w: expires_at must be in the future and within 2 years", store.ErrInvalid)
		}
	}
	return nil
}

// keyOf loads a key, checks it belongs to appID and that a may manage it.
func (s *Service) keyOf(ctx context.Context, a Actor, appID, keyID uuid.UUID) (store.APIKey, store.Environment, error) {
	k, err := s.st.APIKeys().Get(ctx, a.TenantID, keyID)
	if err != nil {
		return store.APIKey{}, store.Environment{}, err
	}
	if k.ApplicationID != appID {
		return store.APIKey{}, store.Environment{}, store.ErrNotFound
	}
	env, err := s.st.Environments().Get(ctx, a.TenantID, k.EnvironmentID)
	if err != nil {
		return store.APIKey{}, store.Environment{}, err
	}
	if err := s.require(a, keyCapability(env.Name)); err != nil {
		return store.APIKey{}, store.Environment{}, err
	}
	return k, env, nil
}

// RevokeAPIKey revokes a key now (also cutting short a rotation grace period).
func (s *Service) RevokeAPIKey(ctx context.Context, a Actor, appID, keyID uuid.UUID) error {
	k, _, err := s.keyOf(ctx, a, appID, keyID)
	if err != nil {
		return wrap("revoke api key", err)
	}
	now := s.now()
	return wrap("revoke api key", s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		if err := tx.APIKeys().Revoke(ctx, a.TenantID, keyID, now); err != nil {
			return err
		}
		return audit.Record(ctx, tx, actorOf(a.User), audit.Event{
			TenantID: &a.TenantID, Action: "api_key.revoke", EntityType: "api_key", EntityID: keyID.String(),
			After: map[string]any{"prefix": k.Prefix},
		})
	}))
}

// Rotated is the result of RotateAPIKey.
type Rotated struct {
	New             auth.Issued
	OldKeyRevokedAt time.Time
}

// RotateAPIKey issues a replacement key with the same environment, scopes and
// expiry, and schedules the old key's revocation after grace (0–7 days).
func (s *Service) RotateAPIKey(ctx context.Context, a Actor, appID, keyID uuid.UUID, grace time.Duration) (Rotated, error) {
	if grace < 0 || grace > MaxRotationGrace {
		return Rotated{}, fmt.Errorf("%w: grace period must be between 0 and 7 days", store.ErrInvalid)
	}
	old, env, err := s.keyOf(ctx, a, appID, keyID)
	if err != nil {
		return Rotated{}, wrap("rotate api key", err)
	}
	if old.RevokedAt != nil {
		return Rotated{}, ErrKeyInactive
	}
	now := s.now()
	revokeAt := now.Add(grace)
	if old.ExpiresAt != nil && !old.ExpiresAt.After(now) {
		return Rotated{}, ErrKeyInactive
	}
	var out Rotated
	err = s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		var err error
		out.New, err = auth.IssueAPIKey(ctx, tx, s.KeyHasher, actorOf(a.User), a.TenantID, auth.IssueInput{
			ApplicationID: appID, EnvironmentID: env.ID, Name: old.Name, Scopes: old.Scopes, ExpiresAt: old.ExpiresAt,
		})
		if err != nil {
			return err
		}
		if err := tx.APIKeys().Revoke(ctx, a.TenantID, keyID, revokeAt); err != nil {
			return err
		}
		out.OldKeyRevokedAt = revokeAt
		return audit.Record(ctx, tx, actorOf(a.User), audit.Event{
			TenantID: &a.TenantID, Action: "api_key.rotate", EntityType: "api_key", EntityID: keyID.String(),
			After: map[string]any{"replaced_by": out.New.Key.ID.String(), "old_key_revoked_at": revokeAt.UTC().Format(time.RFC3339)},
		})
	})
	return out, wrap("rotate api key", err)
}
