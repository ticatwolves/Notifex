// Package rbac is the tenant console permission model (docs/03 matrix,
// ADR-011 §4). It is pure data: services call Allow and enforce business
// rules (last owner, no granting above your own role) themselves.
package rbac

import "github.com/ticatwolves/notifex/internal/store"

// Capability is something a tenant member may be allowed to do.
type Capability string

// Capabilities. Each maps to a row of the docs/03 matrix or an endpoint in
// api/openapi.yaml; adding one means adding it to the matrix test.
const (
	ViewTenant           Capability = "tenant.view"           // tenant, apps, workflows, runs, metrics, usage
	UpdateTenant         Capability = "tenant.update"         // rename
	DeleteTenant         Capability = "tenant.delete"         // crypto-shred (also needs recent re-auth)
	ManageBilling        Capability = "tenant.billing"        // billing and plan
	TransferOwnership    Capability = "tenant.transfer_owner" // promote to owner (also needs recent re-auth)
	ManageMembers        Capability = "members.manage"        // roles, removal, invitations (admins: not owners)
	EditWorkflows        Capability = "workflows.edit"
	ManageApplications   Capability = "applications.manage" // create, update; add environments
	DeleteApplications   Capability = "applications.delete" // delete apps and environments
	ManageAPIKeysNonProd Capability = "api_keys.manage_nonprod"
	ManageAPIKeysProd    Capability = "api_keys.manage_prod"
	ViewCredentialMeta   Capability = "credentials.view_metadata" // never secrets
	ManageSettings       Capability = "settings.manage"           // credentials, brand, policy, retention
	OperateNotifications Capability = "notifications.operate"     // replay, cancel
	ViewAudit            Capability = "audit.view"
)

var (
	viewer = []Capability{ViewTenant}
	dev    = append(append([]Capability{}, viewer...),
		EditWorkflows, ManageApplications, ManageAPIKeysNonProd, ViewCredentialMeta, OperateNotifications)
	admin = append(append([]Capability{}, dev...),
		UpdateTenant, ManageMembers, DeleteApplications, ManageAPIKeysProd, ManageSettings, ViewAudit)
	owner = append(append([]Capability{}, admin...),
		DeleteTenant, ManageBilling, TransferOwnership)

	grants = map[store.Role]map[Capability]bool{
		store.RoleViewer:    set(viewer),
		store.RoleDeveloper: set(dev),
		store.RoleAdmin:     set(admin),
		store.RoleOwner:     set(owner),
	}
)

func set(cs []Capability) map[Capability]bool {
	m := make(map[Capability]bool, len(cs))
	for _, c := range cs {
		m[c] = true
	}
	return m
}

// Allow reports whether role has capability c. Unknown roles have nothing.
func Allow(role store.Role, c Capability) bool { return grants[role][c] }

// Rank orders roles: viewer < developer < admin < owner. Unknown is 0.
func Rank(r store.Role) int {
	switch r {
	case store.RoleViewer:
		return 1
	case store.RoleDeveloper:
		return 2
	case store.RoleAdmin:
		return 3
	case store.RoleOwner:
		return 4
	default:
		return 0
	}
}

// Valid reports whether r is a known role.
func Valid(r store.Role) bool { return Rank(r) > 0 }

// All lists every capability (for tests and docs).
func All() []Capability { return append([]Capability{}, owner...) }
