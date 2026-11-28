package httptransport

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/ticatwolves/notifex/internal/credential"
	"github.com/ticatwolves/notifex/internal/ids"
	"github.com/ticatwolves/notifex/internal/store"
	"github.com/ticatwolves/notifex/internal/tenant"
)

// CredentialAdmin is what the credential routes need from internal/credential.
// There is deliberately no method that returns a secret.
type CredentialAdmin interface {
	List(ctx context.Context, a tenant.Actor, channel *string, appID *uuid.UUID, p store.Page) ([]credential.Credential, error)
	Get(ctx context.Context, a tenant.Actor, id uuid.UUID) (credential.Credential, error)
	Create(ctx context.Context, a tenant.Actor, in credential.CreateInput) (credential.Credential, string, error)
	Update(ctx context.Context, a tenant.Actor, id uuid.UUID, p credential.Patch) (credential.Credential, error)
	Rotate(ctx context.Context, a tenant.Actor, id uuid.UUID, secret map[string]any) (credential.Credential, string, error)
	Delete(ctx context.Context, a tenant.Actor, id uuid.UUID) error
	ResolveFor(ctx context.Context, a tenant.Actor, appID, envID uuid.UUID, channel string) (credential.Ref, error)
}

// optionalID parses an optional prefixed id from a body or query value.
func optionalID(c *gin.Context, field string, k ids.Kind, v *string) (*uuid.UUID, bool) {
	if v == nil || *v == "" {
		return nil, true
	}
	id, err := ids.Parse(k, *v)
	if err != nil {
		writeError(c, http.StatusBadRequest, "validation_failed", field+" is not a valid id")
		return nil, false
	}
	return &id, true
}

func registerProviders(r *gin.Engine, sess gin.HandlerFunc) {
	r.GET("/v1/admin/channel-providers", sess, func(c *gin.Context) {
		out := make([]gin.H, len(credential.Catalog))
		for i, p := range credential.Catalog {
			out[i] = gin.H{
				"channel": p.Channel, "provider": p.Name, "mode": p.Mode,
				"config_fields": fieldsJSON(p.Config), "secret_fields": fieldsJSON(p.Secret), "secret_generated": p.SecretGenerated,
			}
		}
		c.JSON(http.StatusOK, gin.H{"data": out})
	})
}

func fieldsJSON(fs []credential.Field) []gin.H {
	out := make([]gin.H, len(fs))
	for i, f := range fs {
		h := gin.H{"name": f.Name, "type": string(f.Type), "required": f.Required, "description": f.Description}
		if len(f.Values) > 0 {
			h["values"] = f.Values
		}
		out[i] = h
	}
	return out
}

func registerCredentials(one *gin.RouterGroup, cs CredentialAdmin) {
	g := one.Group("/channel-credentials")
	g.GET("", func(c *gin.Context) {
		p, ok := pageParams(c, ids.Credential)
		if !ok {
			return
		}
		var channel *string
		if v := c.Query("channel"); v != "" {
			if !tenant.Channels[v] {
				writeError(c, http.StatusBadRequest, "validation_failed", "channel is not valid")
				return
			}
			channel = &v
		}
		q := c.Query("application_id")
		appID, ok := optionalID(c, "application_id", ids.Application, &q)
		if !ok {
			return
		}
		list, err := cs.List(c.Request.Context(), actor(c), channel, appID, p)
		if err != nil {
			consoleError(c, err)
			return
		}
		out := make([]gin.H, len(list))
		for i, cr := range list {
			out[i] = credentialJSON(cr)
		}
		var next any
		if len(list) == p.Limit {
			next = ids.Format(ids.Credential, list[len(list)-1].ID)
		}
		c.JSON(http.StatusOK, gin.H{"data": out, "next_cursor": next})
	})
	g.POST("", func(c *gin.Context) {
		var in struct {
			Channel       string         `json:"channel"`
			Provider      string         `json:"provider"`
			Name          string         `json:"name"`
			ApplicationID *string        `json:"application_id"`
			EnvironmentID *string        `json:"environment_id"`
			Config        map[string]any `json:"config"`
			Secret        map[string]any `json:"secret"`
		}
		if !bindJSON(c, &in) {
			return
		}
		appID, ok := optionalID(c, "application_id", ids.Application, in.ApplicationID)
		if !ok {
			return
		}
		envID, ok := optionalID(c, "environment_id", ids.Environment, in.EnvironmentID)
		if !ok {
			return
		}
		cr, gen, err := cs.Create(c.Request.Context(), actor(c), credential.CreateInput{
			Channel: in.Channel, Provider: in.Provider, Name: in.Name, ApplicationID: appID, EnvironmentID: envID,
			Config: in.Config, Secret: in.Secret,
		})
		clear(in.Secret)
		if err != nil {
			consoleError(c, err)
			return
		}
		c.JSON(http.StatusCreated, createdJSON(cr, gen))
	})
	g.GET("/resolve", func(c *gin.Context) {
		ch := c.Query("channel")
		app, env := c.Query("application_id"), c.Query("environment_id")
		appID, err1 := ids.Parse(ids.Application, app)
		envID, err2 := ids.Parse(ids.Environment, env)
		if ch == "" || err1 != nil || err2 != nil {
			writeError(c, http.StatusBadRequest, "validation_failed", "channel, application_id and environment_id are required")
			return
		}
		ref, err := cs.ResolveFor(c.Request.Context(), actor(c), appID, envID, ch)
		if errors.Is(err, credential.ErrUnavailable) {
			c.JSON(http.StatusOK, gin.H{"available": false, "channel": ch, "reason": "credential_unavailable"})
			return
		}
		if err != nil {
			consoleError(c, err)
			return
		}
		var cid any
		if ref.CredentialID != nil {
			cid = ids.Format(ids.Credential, *ref.CredentialID)
		}
		c.JSON(http.StatusOK, gin.H{"available": true, "channel": ref.Channel, "mode": ref.Mode, "provider": ref.Provider,
			"credential_id": cid, "scope": ref.Scope})
	})
	one1 := g.Group("/:credential_id")
	one1.GET("", func(c *gin.Context) {
		id, ok := pathID(c, "credential_id", ids.Credential)
		if !ok {
			return
		}
		cr, err := cs.Get(c.Request.Context(), actor(c), id)
		if err != nil {
			consoleError(c, err)
			return
		}
		c.JSON(http.StatusOK, credentialJSON(cr))
	})
	one1.PATCH("", func(c *gin.Context) {
		id, ok := pathID(c, "credential_id", ids.Credential)
		if !ok {
			return
		}
		var in struct {
			Name    *string        `json:"name"`
			Config  map[string]any `json:"config"`
			Enabled *bool          `json:"enabled"`
		}
		if !bindJSON(c, &in) {
			return
		}
		if in.Name == nil && in.Config == nil && in.Enabled == nil {
			writeError(c, http.StatusBadRequest, "validation_failed", "nothing to update")
			return
		}
		cr, err := cs.Update(c.Request.Context(), actor(c), id, credential.Patch{Name: in.Name, Config: in.Config, Enabled: in.Enabled})
		if err != nil {
			consoleError(c, err)
			return
		}
		c.JSON(http.StatusOK, credentialJSON(cr))
	})
	one1.DELETE("", func(c *gin.Context) {
		id, ok := pathID(c, "credential_id", ids.Credential)
		if !ok {
			return
		}
		if err := cs.Delete(c.Request.Context(), actor(c), id); err != nil {
			consoleError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	})
	one1.POST("/rotate", func(c *gin.Context) {
		id, ok := pathID(c, "credential_id", ids.Credential)
		if !ok {
			return
		}
		var secret map[string]any
		if c.Request.ContentLength != 0 {
			var in struct {
				Secret map[string]any `json:"secret"`
			}
			if !bindJSON(c, &in) {
				return
			}
			if in.Secret == nil {
				writeError(c, http.StatusBadRequest, "validation_failed", "secret is required")
				return
			}
			secret = in.Secret
		}
		cr, gen, err := cs.Rotate(c.Request.Context(), actor(c), id, secret)
		clear(secret)
		if err != nil {
			consoleError(c, err)
			return
		}
		c.JSON(http.StatusOK, createdJSON(cr, gen))
	})
}

func credentialJSON(cr credential.Credential) gin.H {
	var app, env any
	if cr.ApplicationID != nil {
		app = ids.Format(ids.Application, *cr.ApplicationID)
	}
	if cr.EnvironmentID != nil {
		env = ids.Format(ids.Environment, *cr.EnvironmentID)
	}
	out := gin.H{
		"id": ids.Format(ids.Credential, cr.ID), "channel": cr.Channel, "provider": cr.Provider, "mode": cr.Mode,
		"name": cr.Name, "application_id": app, "environment_id": env, "config": cr.Config, "has_secret": cr.HasSecret,
		"status": cr.Status, "verified_at": utcPtr(cr.VerifiedAt), "rotated_at": utcPtr(cr.RotatedAt),
		"created_at": cr.CreatedAt.UTC(), "updated_at": cr.UpdatedAt.UTC(),
	}
	if cr.SecretHint != "" {
		out["secret_hint"] = cr.SecretHint
	}
	return out
}

func createdJSON(cr credential.Credential, generated string) gin.H {
	out := gin.H{"credential": credentialJSON(cr)}
	if generated != "" {
		out["generated_secret"] = generated
	}
	return out
}
