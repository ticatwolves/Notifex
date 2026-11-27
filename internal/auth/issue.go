package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/ticatwolves/notifex/internal/audit"
	"github.com/ticatwolves/notifex/internal/store"
)

// IssueInput describes a new API key.
type IssueInput struct {
	ApplicationID uuid.UUID
	EnvironmentID uuid.UUID
	Name          string
	Scopes        []string
	ExpiresAt     *time.Time
}

// Issued is a newly created key. Plaintext is returned exactly once and must
// never be logged or stored.
type Issued struct {
	Key       store.APIKey
	Plaintext string
}

// IssueAPIKey creates a key for an environment and records the audit event in
// the same transaction. Role checks (e.g. developers only for non-prod) belong
// to the caller's authorization layer (Phase 1).
func IssueAPIKey(ctx context.Context, st store.Store, h *Hasher, actor audit.Actor, tid store.TenantID, in IssueInput) (Issued, error) {
	var out Issued
	err := st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		env, err := tx.Environments().Get(ctx, tid, in.EnvironmentID)
		if err != nil {
			return fmt.Errorf("issue api key: %w", err)
		}
		if env.ApplicationID != in.ApplicationID {
			return fmt.Errorf("issue api key: %w", store.ErrNotFound)
		}
		// A prefix collision (~1 in 2^62 per pair) just retries.
		for attempt := 0; ; attempt++ {
			plaintext, parsed, err := Generate(string(env.Name))
			if err != nil {
				return err
			}
			key, err := tx.APIKeys().Create(ctx, tid, store.NewAPIKey{
				ApplicationID: in.ApplicationID, EnvironmentID: in.EnvironmentID, Name: in.Name,
				Prefix: parsed.Prefix, Hash: h.Hash(parsed), Scopes: in.Scopes, ExpiresAt: in.ExpiresAt,
			})
			if errors.Is(err, store.ErrConflict) && attempt < 2 {
				continue
			}
			if err != nil {
				return fmt.Errorf("issue api key: %w", err)
			}
			out = Issued{Key: key, Plaintext: plaintext}
			break
		}
		return audit.Record(ctx, tx, actor, audit.Event{
			TenantID: &tid, Action: "api_key.create", EntityType: "api_key", EntityID: out.Key.ID.String(),
			After: map[string]any{
				"name": out.Key.Name, "prefix": out.Key.Prefix, "environment": string(env.Name),
				"application_id": in.ApplicationID.String(), "scopes": out.Key.Scopes,
			},
		})
	})
	if err != nil {
		return Issued{}, err
	}
	return out, nil
}
