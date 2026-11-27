package httptransport

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/ticatwolves/notifex/internal/auth"
	"github.com/ticatwolves/notifex/internal/ids"
	"github.com/ticatwolves/notifex/internal/store"
	"github.com/ticatwolves/notifex/internal/tenant"
)

// TenantAdmin is what the /v1/admin/tenants routes need from internal/tenant.
type TenantAdmin interface {
	Access(ctx context.Context, u tenant.User, tid store.TenantID) (tenant.Actor, store.Tenant, error)
	Create(ctx context.Context, u tenant.User, name, slug string) (store.Tenant, error)
	Rename(ctx context.Context, a tenant.Actor, name string) (store.Tenant, error)
	Delete(ctx context.Context, a tenant.Actor) error
	ListMembers(ctx context.Context, a tenant.Actor, p store.Page) ([]tenant.Member, error)
	UpdateRole(ctx context.Context, a tenant.Actor, userID uuid.UUID, role store.Role) (tenant.Member, error)
	RemoveMember(ctx context.Context, a tenant.Actor, userID uuid.UUID) error
	Invite(ctx context.Context, a tenant.Actor, email string, role store.Role) (store.Invitation, error)
	ListInvitations(ctx context.Context, a tenant.Actor) ([]store.Invitation, error)
	RevokeInvitation(ctx context.Context, a tenant.Actor, id uuid.UUID) error
	Accept(ctx context.Context, u tenant.User, token string) (tenant.Member, error)
}

const ctxActor = "tenant_actor"

func toTenantUser(p auth.SessionPrincipal) tenant.User {
	return tenant.User{ID: p.UserID, Email: p.Email, EmailVerified: p.EmailVerified, ReauthenticatedAt: p.ReauthenticatedAt}
}

func registerAdmin(r *gin.Engine, d Deps) {
	t := d.Tenants
	sess := requireSession(d.Sessions)

	r.POST("/v1/invitations/accept", perIP(d.Limits.Credential), sess, func(c *gin.Context) {
		var in struct {
			Token string `json:"token"`
		}
		if !bindJSON(c, &in) {
			return
		}
		m, err := t.Accept(c.Request.Context(), toTenantUser(sessionPrincipal(c)), in.Token)
		if err != nil {
			consoleError(c, err)
			return
		}
		c.JSON(http.StatusOK, memberJSON(m))
	})

	admin := r.Group("/v1/admin/tenants", sess)
	admin.POST("", func(c *gin.Context) {
		var in struct {
			Name string `json:"name"`
			Slug string `json:"slug"`
		}
		if !bindJSON(c, &in) {
			return
		}
		ten, err := t.Create(c.Request.Context(), toTenantUser(sessionPrincipal(c)), in.Name, in.Slug)
		if err != nil {
			consoleError(c, err)
			return
		}
		c.JSON(http.StatusCreated, tenantJSON(ten))
	})

	one := admin.Group("/:tenant_id", tenantAccess(t))
	one.GET("", func(c *gin.Context) {
		v, _ := c.Get("tenant")
		c.JSON(http.StatusOK, tenantJSON(v.(store.Tenant)))
	})
	one.PATCH("", func(c *gin.Context) {
		var in struct {
			Name *string `json:"name"`
		}
		if !bindJSON(c, &in) {
			return
		}
		if in.Name == nil {
			writeError(c, http.StatusBadRequest, "validation_failed", "nothing to update")
			return
		}
		ten, err := t.Rename(c.Request.Context(), actor(c), *in.Name)
		if err != nil {
			consoleError(c, err)
			return
		}
		c.JSON(http.StatusOK, tenantJSON(ten))
	})
	one.DELETE("", func(c *gin.Context) {
		if err := t.Delete(c.Request.Context(), actor(c)); err != nil {
			consoleError(c, err)
			return
		}
		c.Status(http.StatusAccepted)
	})

	one.GET("/members", func(c *gin.Context) {
		p, ok := pageParams(c, ids.Membership)
		if !ok {
			return
		}
		ms, err := t.ListMembers(c.Request.Context(), actor(c), p)
		if err != nil {
			consoleError(c, err)
			return
		}
		out := make([]gin.H, len(ms))
		for i, m := range ms {
			out[i] = memberJSON(m)
		}
		var next any
		if len(ms) == p.Limit {
			next = ids.Format(ids.Membership, ms[len(ms)-1].Membership.ID)
		}
		c.JSON(http.StatusOK, gin.H{"data": out, "next_cursor": next})
	})
	one.PATCH("/members/:user_id", func(c *gin.Context) {
		uid, ok := pathID(c, "user_id", ids.User)
		if !ok {
			return
		}
		var in struct {
			Role string `json:"role"`
		}
		if !bindJSON(c, &in) {
			return
		}
		m, err := t.UpdateRole(c.Request.Context(), actor(c), uid, store.Role(in.Role))
		if err != nil {
			consoleError(c, err)
			return
		}
		c.JSON(http.StatusOK, memberJSON(m))
	})
	one.DELETE("/members/:user_id", func(c *gin.Context) {
		uid, ok := pathID(c, "user_id", ids.User)
		if !ok {
			return
		}
		if err := t.RemoveMember(c.Request.Context(), actor(c), uid); err != nil {
			consoleError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	})

	one.GET("/invitations", func(c *gin.Context) {
		invs, err := t.ListInvitations(c.Request.Context(), actor(c))
		if err != nil {
			consoleError(c, err)
			return
		}
		out := make([]gin.H, len(invs))
		for i, inv := range invs {
			out[i] = invitationJSON(inv)
		}
		c.JSON(http.StatusOK, gin.H{"data": out})
	})
	one.POST("/invitations", func(c *gin.Context) {
		var in struct {
			Email string `json:"email"`
			Role  string `json:"role"`
		}
		if !bindJSON(c, &in) {
			return
		}
		inv, err := t.Invite(c.Request.Context(), actor(c), in.Email, store.Role(in.Role))
		if err != nil {
			consoleError(c, err)
			return
		}
		c.JSON(http.StatusCreated, invitationJSON(inv))
	})
	one.DELETE("/invitations/:invitation_id", func(c *gin.Context) {
		id, ok := pathID(c, "invitation_id", ids.Invitation)
		if !ok {
			return
		}
		if err := t.RevokeInvitation(c.Request.Context(), actor(c), id); err != nil {
			consoleError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	})
}

// tenantAccess resolves the caller's membership in :tenant_id. Malformed id,
// unknown tenant, deleted tenant and non-member are all the same 404.
func tenantAccess(t TenantAdmin) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := ids.Parse(ids.Tenant, c.Param("tenant_id"))
		if err != nil {
			writeError(c, http.StatusNotFound, "not_found", "not found")
			return
		}
		a, ten, err := t.Access(c.Request.Context(), toTenantUser(sessionPrincipal(c)), store.TenantID(id))
		if err != nil {
			consoleError(c, err)
			return
		}
		c.Set(ctxActor, a)
		c.Set("tenant", ten)
		c.Next()
	}
}

func actor(c *gin.Context) tenant.Actor {
	v, _ := c.Get(ctxActor)
	return v.(tenant.Actor) // set by tenantAccess before any handler runs
}

// pathID parses a prefixed id path parameter; malformed ids are 404.
func pathID(c *gin.Context, name string, k ids.Kind) (uuid.UUID, bool) {
	id, err := ids.Parse(k, c.Param(name))
	if err != nil {
		writeError(c, http.StatusNotFound, "not_found", "not found")
		return uuid.Nil, false
	}
	return id, true
}

// pageParams reads `limit` (1–200) and `after` (a cursor of kind k).
func pageParams(c *gin.Context, k ids.Kind) (store.Page, bool) {
	var p store.Page
	if v := c.Query("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > store.MaxPageSize {
			writeError(c, http.StatusBadRequest, "validation_failed", "limit must be between 1 and 200")
			return p, false
		}
		p.Limit = n
	}
	if v := c.Query("after"); v != "" {
		id, err := ids.Parse(k, v)
		if err != nil {
			writeError(c, http.StatusBadRequest, "validation_failed", "after is not a valid cursor")
			return p, false
		}
		p.After = id
	}
	return p.Normalize(), true
}

func tenantJSON(t store.Tenant) gin.H {
	return gin.H{
		"id": ids.Format(ids.Tenant, t.ID.UUID()), "name": t.Name, "slug": t.Slug, "status": string(t.Status),
		"plan": t.Plan, "sandbox": t.Sandbox, "created_at": t.CreatedAt.UTC(),
	}
}

func memberJSON(m tenant.Member) gin.H {
	return gin.H{
		"id": ids.Format(ids.Membership, m.Membership.ID), "user": toUserJSON(m.User),
		"role": string(m.Membership.Role), "created_at": m.Membership.CreatedAt.UTC(),
	}
}

func invitationJSON(i store.Invitation) gin.H {
	return gin.H{
		"id": ids.Format(ids.Invitation, i.ID), "email": i.Email, "role": string(i.Role),
		"expires_at": i.ExpiresAt.UTC(), "created_at": i.CreatedAt.UTC().Truncate(time.Microsecond),
	}
}
