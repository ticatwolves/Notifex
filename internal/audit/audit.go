// Package audit records control-plane mutations. Every create/update/delete of
// tenants, apps, environments, keys, members, policies, credentials and plan
// limits writes one event in the same transaction as the change.
package audit

import (
	"context"
	"fmt"

	"github.com/sbshobhit/notifex/internal/store"
)

// Actor identifies who performed an action.
type Actor struct {
	Type store.ActorType
	ID   string
}

// System is the actor for automated or bootstrap actions.
var System = Actor{Type: store.ActorSystem, ID: "system"}

// Event describes one mutation. Before/After must contain no secrets or PII.
type Event struct {
	TenantID   *store.TenantID
	Action     string
	EntityType string
	EntityID   string
	Before     map[string]any
	After      map[string]any
	RequestID  string
}

// Record appends e for actor using st, which should be the transaction-bound
// store of the mutation it describes.
func Record(ctx context.Context, st store.Store, actor Actor, e Event) error {
	_, err := st.Audit().Append(ctx, store.AuditEvent{
		TenantID: e.TenantID, ActorType: actor.Type, ActorID: actor.ID,
		Action: e.Action, EntityType: e.EntityType, EntityID: e.EntityID,
		Before: e.Before, After: e.After, RequestID: e.RequestID,
	})
	if err != nil {
		return fmt.Errorf("audit %s: %w", e.Action, err)
	}
	return nil
}
