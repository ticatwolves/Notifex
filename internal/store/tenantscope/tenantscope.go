// Package tenantscope carries the tenant a store operation is scoped to.
//
// Store methods take the tenant id as an explicit argument (so scoping is
// visible at every call site) and put it in the context with With. ent
// interceptors and hooks on every tenant-owned table read it back and add
// `tenant_id = <scope>` to the query or mutation. A query on a tenant-owned
// table without a scope fails, unless the caller opted out with Bypass for a
// named, reviewed reason (e.g. resolving an API key, which is how the tenant
// is discovered in the first place).
//
// This package must stay dependency-free: the generated ent code imports it.
package tenantscope

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// ErrMissing is returned when a tenant-owned table is accessed without a scope.
var ErrMissing = errors.New("tenantscope: tenant-owned table accessed without tenant scope")

type scopeKey struct{}

type bypassKey struct{}

// With returns ctx scoped to tenantID.
func With(ctx context.Context, tenantID uuid.UUID) context.Context {
	return context.WithValue(ctx, scopeKey{}, tenantID)
}

// From returns the tenant scope in ctx, if any.
func From(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(scopeKey{}).(uuid.UUID)
	return id, ok && id != uuid.Nil
}

// Reason names why an operation runs without a tenant scope. Every value is a
// constant declared here, so all bypasses are greppable and reviewed.
type Reason string

// Allowed bypass reasons.
const (
	// ReasonAPIKeyLookup resolves an API key by prefix; the key determines the tenant.
	ReasonAPIKeyLookup Reason = "api_key_lookup" //nolint:gosec // G101: a reason label, not a credential
	// ReasonUserMemberships lists one user's memberships across tenants
	// (tenant switcher). Filtered by user_id instead.
	ReasonUserMemberships Reason = "user_memberships"
	// ReasonPlatform is a platform-operator or system operation across tenants
	// (tenant provisioning, partition and retention jobs). It must be audited.
	ReasonPlatform Reason = "platform"
)

// Bypass returns ctx allowed to access tenant-owned tables without a scope.
func Bypass(ctx context.Context, reason Reason) context.Context {
	return context.WithValue(ctx, bypassKey{}, reason)
}

// Bypassed reports the bypass reason, if any.
func Bypassed(ctx context.Context) (Reason, bool) {
	r, ok := ctx.Value(bypassKey{}).(Reason)
	return r, ok && r != ""
}
