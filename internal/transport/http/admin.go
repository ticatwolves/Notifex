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

	ListApplications(ctx context.Context, a tenant.Actor, p store.Page) ([]store.Application, error)
	GetApplication(ctx context.Context, a tenant.Actor, id uuid.UUID) (store.Application, error)
	CreateApplication(ctx context.Context, a tenant.Actor, in store.NewApplication) (store.Application, error)
	UpdateApplication(ctx context.Context, a tenant.Actor, id uuid.UUID, p store.ApplicationPatch) (store.Application, error)
	DeleteApplication(ctx context.Context, a tenant.Actor, id uuid.UUID) error
	ListEnvironments(ctx context.Context, a tenant.Actor, appID uuid.UUID) ([]store.Environment, error)
	CreateEnvironment(ctx context.Context, a tenant.Actor, appID uuid.UUID, name store.EnvironmentName) (store.Environment, error)
	DeleteEnvironment(ctx context.Context, a tenant.Actor, appID, envID uuid.UUID) error
	ListAPIKeys(ctx context.Context, a tenant.Actor, appID uuid.UUID, p store.Page) ([]store.APIKey, error)
	CreateAPIKey(ctx context.Context, a tenant.Actor, appID uuid.UUID, in tenant.KeyInput) (auth.Issued, error)
	RevokeAPIKey(ctx context.Context, a tenant.Actor, appID, keyID uuid.UUID) error
	RotateAPIKey(ctx context.Context, a tenant.Actor, appID, keyID uuid.UUID, grace time.Duration) (tenant.Rotated, error)

	ListAuditEvents(ctx context.Context, a tenant.Actor, p store.Page) ([]store.AuditEvent, error)

	GetBrand(ctx context.Context, a tenant.Actor) (tenant.BrandView, error)
	PutBrand(ctx context.Context, a tenant.Actor, b tenant.Brand) (tenant.BrandView, error)
	AppBrand(ctx context.Context, a tenant.Actor, appID uuid.UUID) (*tenant.Brand, tenant.BrandView, error)
	PutAppBrand(ctx context.Context, a tenant.Actor, appID uuid.UUID, b tenant.Brand) (*tenant.Brand, tenant.BrandView, error)
	DeleteAppBrand(ctx context.Context, a tenant.Actor, appID uuid.UUID) error
	GetPolicy(ctx context.Context, a tenant.Actor) (tenant.SendingPolicyView, error)
	PutPolicy(ctx context.Context, a tenant.Actor, p tenant.SendingPolicy) (tenant.SendingPolicyView, error)
	AppPolicy(ctx context.Context, a tenant.Actor, appID uuid.UUID) (*tenant.SendingPolicy, tenant.SendingPolicyView, error)
	PutAppPolicy(ctx context.Context, a tenant.Actor, appID uuid.UUID, p tenant.SendingPolicy) (*tenant.SendingPolicy, tenant.SendingPolicyView, error)
	DeleteAppPolicy(ctx context.Context, a tenant.Actor, appID uuid.UUID) error
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

	registerApps(one, t)
	registerSettings(one, t)
	if d.Workflows != nil {
		registerWorkflows(one, d.Workflows, d.Limits.Preview)
	}

	one.GET("/audit-events", func(c *gin.Context) {
		p, ok := pageParams(c, ids.AuditEvent)
		if !ok {
			return
		}
		evs, err := t.ListAuditEvents(c.Request.Context(), actor(c), p)
		if err != nil {
			consoleError(c, err)
			return
		}
		out := make([]gin.H, len(evs))
		for i, ev := range evs {
			out[i] = auditEventJSON(ev)
		}
		var next any
		if len(evs) == p.Limit {
			next = ids.Format(ids.AuditEvent, evs[len(evs)-1].ID)
		}
		c.JSON(http.StatusOK, gin.H{"data": out, "next_cursor": next})
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

func registerApps(one *gin.RouterGroup, t TenantAdmin) {
	one.GET("/applications", func(c *gin.Context) {
		p, ok := pageParams(c, ids.Application)
		if !ok {
			return
		}
		apps, err := t.ListApplications(c.Request.Context(), actor(c), p)
		if err != nil {
			consoleError(c, err)
			return
		}
		out := make([]gin.H, len(apps))
		for i, a := range apps {
			out[i] = applicationJSON(a)
		}
		var next any
		if len(apps) == p.Limit {
			next = ids.Format(ids.Application, apps[len(apps)-1].ID)
		}
		c.JSON(http.StatusOK, gin.H{"data": out, "next_cursor": next})
	})
	one.POST("/applications", func(c *gin.Context) {
		var in struct {
			Name        string `json:"name"`
			Key         string `json:"key"`
			Description string `json:"description"`
		}
		if !bindJSON(c, &in) {
			return
		}
		app, err := t.CreateApplication(c.Request.Context(), actor(c), store.NewApplication{Name: in.Name, Key: in.Key, Description: in.Description})
		if err != nil {
			consoleError(c, err)
			return
		}
		c.JSON(http.StatusCreated, applicationJSON(app))
	})

	app := one.Group("/applications/:application_id")
	app.GET("", func(c *gin.Context) {
		id, ok := pathID(c, "application_id", ids.Application)
		if !ok {
			return
		}
		a, err := t.GetApplication(c.Request.Context(), actor(c), id)
		if err != nil {
			consoleError(c, err)
			return
		}
		c.JSON(http.StatusOK, applicationJSON(a))
	})
	app.PATCH("", func(c *gin.Context) {
		id, ok := pathID(c, "application_id", ids.Application)
		if !ok {
			return
		}
		var in struct {
			Name        *string `json:"name"`
			Description *string `json:"description"`
		}
		if !bindJSON(c, &in) {
			return
		}
		if in.Name == nil && in.Description == nil {
			writeError(c, http.StatusBadRequest, "validation_failed", "nothing to update")
			return
		}
		a, err := t.UpdateApplication(c.Request.Context(), actor(c), id, store.ApplicationPatch{Name: in.Name, Description: in.Description})
		if err != nil {
			consoleError(c, err)
			return
		}
		c.JSON(http.StatusOK, applicationJSON(a))
	})
	app.DELETE("", func(c *gin.Context) {
		id, ok := pathID(c, "application_id", ids.Application)
		if !ok {
			return
		}
		if err := t.DeleteApplication(c.Request.Context(), actor(c), id); err != nil {
			consoleError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	})

	app.GET("/environments", func(c *gin.Context) {
		id, ok := pathID(c, "application_id", ids.Application)
		if !ok {
			return
		}
		envs, err := t.ListEnvironments(c.Request.Context(), actor(c), id)
		if err != nil {
			consoleError(c, err)
			return
		}
		out := make([]gin.H, len(envs))
		for i, e := range envs {
			out[i] = environmentJSON(e)
		}
		c.JSON(http.StatusOK, gin.H{"data": out})
	})
	app.POST("/environments", func(c *gin.Context) {
		id, ok := pathID(c, "application_id", ids.Application)
		if !ok {
			return
		}
		var in struct {
			Name string `json:"name"`
		}
		if !bindJSON(c, &in) {
			return
		}
		env, err := t.CreateEnvironment(c.Request.Context(), actor(c), id, store.EnvironmentName(in.Name))
		if err != nil {
			consoleError(c, err)
			return
		}
		c.JSON(http.StatusCreated, environmentJSON(env))
	})
	app.DELETE("/environments/:environment_id", func(c *gin.Context) {
		appID, ok := pathID(c, "application_id", ids.Application)
		if !ok {
			return
		}
		envID, ok := pathID(c, "environment_id", ids.Environment)
		if !ok {
			return
		}
		if err := t.DeleteEnvironment(c.Request.Context(), actor(c), appID, envID); err != nil {
			consoleError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	})

	app.GET("/api-keys", func(c *gin.Context) {
		appID, ok := pathID(c, "application_id", ids.Application)
		if !ok {
			return
		}
		p, ok := pageParams(c, ids.APIKey)
		if !ok {
			return
		}
		keys, err := t.ListAPIKeys(c.Request.Context(), actor(c), appID, p)
		if err != nil {
			consoleError(c, err)
			return
		}
		out := make([]gin.H, len(keys))
		for i, k := range keys {
			out[i] = apiKeyJSON(k)
		}
		var next any
		if len(keys) == p.Limit {
			next = ids.Format(ids.APIKey, keys[len(keys)-1].ID)
		}
		c.JSON(http.StatusOK, gin.H{"data": out, "next_cursor": next})
	})
	app.POST("/api-keys", func(c *gin.Context) {
		appID, ok := pathID(c, "application_id", ids.Application)
		if !ok {
			return
		}
		var in struct {
			EnvironmentID string     `json:"environment_id"`
			Name          string     `json:"name"`
			Scopes        []string   `json:"scopes"`
			ExpiresAt     *time.Time `json:"expires_at"`
		}
		if !bindJSON(c, &in) {
			return
		}
		envID, err := ids.Parse(ids.Environment, in.EnvironmentID)
		if err != nil {
			writeError(c, http.StatusBadRequest, "validation_failed", "environment_id is not a valid id")
			return
		}
		k, err := t.CreateAPIKey(c.Request.Context(), actor(c), appID, tenant.KeyInput{
			EnvironmentID: envID, Name: in.Name, Scopes: in.Scopes, ExpiresAt: in.ExpiresAt,
		})
		if err != nil {
			consoleError(c, err)
			return
		}
		c.JSON(http.StatusCreated, gin.H{"api_key": apiKeyJSON(k.Key), "key": k.Plaintext})
	})
	app.POST("/api-keys/:api_key_id/revoke", func(c *gin.Context) {
		appID, ok := pathID(c, "application_id", ids.Application)
		if !ok {
			return
		}
		keyID, ok := pathID(c, "api_key_id", ids.APIKey)
		if !ok {
			return
		}
		if err := t.RevokeAPIKey(c.Request.Context(), actor(c), appID, keyID); err != nil {
			consoleError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	})
	app.POST("/api-keys/:api_key_id/rotate", func(c *gin.Context) {
		appID, ok := pathID(c, "application_id", ids.Application)
		if !ok {
			return
		}
		keyID, ok := pathID(c, "api_key_id", ids.APIKey)
		if !ok {
			return
		}
		grace := tenant.DefaultRotationGrace
		if c.Request.ContentLength != 0 {
			var in struct {
				GraceSeconds *int64 `json:"grace_period_seconds"`
			}
			if !bindJSON(c, &in) {
				return
			}
			if in.GraceSeconds != nil {
				grace = time.Duration(*in.GraceSeconds) * time.Second
				if *in.GraceSeconds < 0 || grace > tenant.MaxRotationGrace {
					writeError(c, http.StatusBadRequest, "validation_failed", "grace_period_seconds must be between 0 and 604800")
					return
				}
			}
		}
		r, err := t.RotateAPIKey(c.Request.Context(), actor(c), appID, keyID, grace)
		if err != nil {
			consoleError(c, err)
			return
		}
		c.JSON(http.StatusCreated, gin.H{"api_key": apiKeyJSON(r.New.Key), "key": r.New.Plaintext, "old_key_revoked_at": r.OldKeyRevokedAt.UTC()})
	})
}

func applicationJSON(a store.Application) gin.H {
	return gin.H{
		"id": ids.Format(ids.Application, a.ID), "name": a.Name, "key": a.Key, "description": a.Description,
		"created_at": a.CreatedAt.UTC(), "updated_at": a.UpdatedAt.UTC(),
	}
}

func environmentJSON(e store.Environment) gin.H {
	return gin.H{
		"id": ids.Format(ids.Environment, e.ID), "application_id": ids.Format(ids.Application, e.ApplicationID),
		"name": string(e.Name), "created_at": e.CreatedAt.UTC(),
	}
}

func apiKeyJSON(k store.APIKey) gin.H {
	scopes := k.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	return gin.H{
		"id": ids.Format(ids.APIKey, k.ID), "environment_id": ids.Format(ids.Environment, k.EnvironmentID),
		"name": k.Name, "prefix": k.Prefix, "scopes": scopes,
		"expires_at": utcPtr(k.ExpiresAt), "last_used_at": utcPtr(k.LastUsedAt), "revoked_at": utcPtr(k.RevokedAt),
		"created_at": k.CreatedAt.UTC(),
	}
}

func utcPtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}

// auditEntityKinds maps entity types to public id prefixes, so the trail
// shows the same ids as the rest of the API.
var auditEntityKinds = map[string]ids.Kind{
	"tenant": ids.Tenant, "application": ids.Application, "environment": ids.Environment,
	"api_key": ids.APIKey, "membership": ids.Membership, "invitation": ids.Invitation,
	"user": ids.User, "session": ids.Session,
}

func publicID(k ids.Kind, raw string) string {
	id, err := uuid.Parse(raw)
	if err != nil || id == uuid.Nil {
		return raw
	}
	return ids.Format(k, id)
}

func auditEventJSON(ev store.AuditEvent) gin.H {
	actorID := ev.ActorID
	switch ev.ActorType {
	case store.ActorUser:
		actorID = publicID(ids.User, actorID)
	case store.ActorAPIKey:
		actorID = publicID(ids.APIKey, actorID)
	case store.ActorOperator:
		// Tenants see that Notifex staff acted, not which staff account.
		actorID = "notifex"
	case store.ActorSystem:
	}
	entityID := ev.EntityID
	if k, ok := auditEntityKinds[ev.EntityType]; ok {
		entityID = publicID(k, entityID)
	}
	out := gin.H{
		"id": ids.Format(ids.AuditEvent, ev.ID), "actor_type": string(ev.ActorType), "actor_id": actorID,
		"action": ev.Action, "entity_type": ev.EntityType, "entity_id": entityID,
		"before": ev.Before, "after": ev.After, "created_at": ev.CreatedAt.UTC(),
	}
	if ev.RequestID != "" {
		out["request_id"] = ev.RequestID
	}
	return out
}
