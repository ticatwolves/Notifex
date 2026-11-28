package httptransport

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/ticatwolves/notifex/internal/ids"
	"github.com/ticatwolves/notifex/internal/ratelimit"
	"github.com/ticatwolves/notifex/internal/store"
	"github.com/ticatwolves/notifex/internal/tenant"
	"github.com/ticatwolves/notifex/internal/workflow"
)

// WorkflowAdmin is what the workflow routes need from internal/workflow.
type WorkflowAdmin interface {
	List(ctx context.Context, a tenant.Actor, appID uuid.UUID, status *store.WorkflowStatus, p store.Page) ([]workflow.View, error)
	Get(ctx context.Context, a tenant.Actor, appID, id uuid.UUID) (workflow.View, error)
	Create(ctx context.Context, a tenant.Actor, appID uuid.UUID, in workflow.CreateInput) (workflow.View, error)
	Update(ctx context.Context, a tenant.Actor, appID, id uuid.UUID, p workflow.Patch) (workflow.View, error)
	Delete(ctx context.Context, a tenant.Actor, appID, id uuid.UUID) error
	CreateAction(ctx context.Context, a tenant.Actor, appID, wid uuid.UUID, channel string, enabled bool) (store.Action, error)
	SetActionEnabled(ctx context.Context, a tenant.Actor, appID, wid, aid uuid.UUID, enabled bool) (store.Action, error)
	DeleteAction(ctx context.Context, a tenant.Actor, appID, wid, aid uuid.UUID) error
	ListTemplates(ctx context.Context, a tenant.Actor, appID, wid, aid uuid.UUID) ([]workflow.TemplateView, error)
	GetTemplate(ctx context.Context, a tenant.Actor, appID, wid, aid uuid.UUID, locale string) (workflow.TemplateView, error)
	PutTemplate(ctx context.Context, a tenant.Actor, appID, wid, aid uuid.UUID, locale string, c workflow.Content) (workflow.TemplateView, error)
	DeleteTemplate(ctx context.Context, a tenant.Actor, appID, wid, aid uuid.UUID, locale string) error
	Preview(ctx context.Context, a tenant.Actor, appID uuid.UUID, in workflow.PreviewInput) (workflow.PreviewResult, error)
}

// pathIDs parses several path ids; any malformed one is 404.
func pathIDs(c *gin.Context, specs ...struct {
	name string
	kind ids.Kind
}) ([]uuid.UUID, bool) {
	out := make([]uuid.UUID, len(specs))
	for i, s := range specs {
		id, ok := pathID(c, s.name, s.kind)
		if !ok {
			return nil, false
		}
		out[i] = id
	}
	return out, true
}

type idSpec = struct {
	name string
	kind ids.Kind
}

var (
	pApp = idSpec{"application_id", ids.Application}
	pWF  = idSpec{"workflow_id", ids.Workflow}
	pAct = idSpec{"action_id", ids.Action}
)

func registerWorkflows(one *gin.RouterGroup, w WorkflowAdmin, previewLimit *ratelimit.Limiter) {
	app := one.Group("/applications/:application_id")
	wfs := app.Group("/workflows")

	wfs.GET("", func(c *gin.Context) {
		id, ok := pathIDs(c, pApp)
		if !ok {
			return
		}
		p, ok := pageParams(c, ids.Workflow)
		if !ok {
			return
		}
		var status *store.WorkflowStatus
		if v := c.Query("status"); v != "" {
			s := store.WorkflowStatus(v)
			if s != store.WorkflowDraft && s != store.WorkflowActive && s != store.WorkflowArchived {
				writeError(c, http.StatusBadRequest, "validation_failed", "status must be draft, active or archived")
				return
			}
			status = &s
		}
		list, err := w.List(c.Request.Context(), actor(c), id[0], status, p)
		if err != nil {
			consoleError(c, err)
			return
		}
		out := make([]gin.H, len(list))
		for i, v := range list {
			out[i] = workflowJSON(v)
		}
		var next any
		if len(list) == p.Limit {
			next = ids.Format(ids.Workflow, list[len(list)-1].ID)
		}
		c.JSON(http.StatusOK, gin.H{"data": out, "next_cursor": next})
	})
	wfs.POST("", func(c *gin.Context) {
		id, ok := pathIDs(c, pApp)
		if !ok {
			return
		}
		var in struct {
			Key           string         `json:"key"`
			Name          string         `json:"name"`
			Description   string         `json:"description"`
			PayloadSchema map[string]any `json:"payload_schema"`
		}
		if !bindJSON(c, &in) {
			return
		}
		v, err := w.Create(c.Request.Context(), actor(c), id[0], workflow.CreateInput{
			Key: in.Key, Name: in.Name, Description: in.Description, PayloadSchema: in.PayloadSchema,
		})
		if err != nil {
			consoleError(c, err)
			return
		}
		c.JSON(http.StatusCreated, workflowJSON(v))
	})

	one1 := wfs.Group("/:workflow_id")
	one1.GET("", func(c *gin.Context) {
		id, ok := pathIDs(c, pApp, pWF)
		if !ok {
			return
		}
		v, err := w.Get(c.Request.Context(), actor(c), id[0], id[1])
		if err != nil {
			consoleError(c, err)
			return
		}
		c.JSON(http.StatusOK, workflowJSON(v))
	})
	one1.PATCH("", func(c *gin.Context) {
		id, ok := pathIDs(c, pApp, pWF)
		if !ok {
			return
		}
		var in struct {
			Name          *string               `json:"name"`
			Description   *string               `json:"description"`
			Status        *store.WorkflowStatus `json:"status"`
			PayloadSchema json.RawMessage       `json:"payload_schema"` // absent: unchanged; null: remove
		}
		if !bindJSON(c, &in) {
			return
		}
		p := workflow.Patch{Name: in.Name, Description: in.Description, Status: in.Status}
		switch {
		case in.PayloadSchema == nil:
		case bytes.Equal(bytes.TrimSpace(in.PayloadSchema), []byte("null")):
			p.ClearSchema = true
		default:
			dec := json.NewDecoder(bytes.NewReader(in.PayloadSchema))
			dec.UseNumber()
			if err := dec.Decode(&p.PayloadSchema); err != nil || p.PayloadSchema == nil {
				writeError(c, http.StatusBadRequest, "validation_failed", "payload_schema must be an object or null")
				return
			}
		}
		if p.Name == nil && p.Description == nil && p.Status == nil && in.PayloadSchema == nil {
			writeError(c, http.StatusBadRequest, "validation_failed", "nothing to update")
			return
		}
		v, err := w.Update(c.Request.Context(), actor(c), id[0], id[1], p)
		if err != nil {
			consoleError(c, err)
			return
		}
		c.JSON(http.StatusOK, workflowJSON(v))
	})
	one1.DELETE("", func(c *gin.Context) {
		id, ok := pathIDs(c, pApp, pWF)
		if !ok {
			return
		}
		if err := w.Delete(c.Request.Context(), actor(c), id[0], id[1]); err != nil {
			consoleError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	})

	one1.POST("/actions", func(c *gin.Context) {
		id, ok := pathIDs(c, pApp, pWF)
		if !ok {
			return
		}
		var in struct {
			Channel string `json:"channel"`
			Enabled *bool  `json:"enabled"`
		}
		if !bindJSON(c, &in) {
			return
		}
		enabled := in.Enabled == nil || *in.Enabled
		act, err := w.CreateAction(c.Request.Context(), actor(c), id[0], id[1], in.Channel, enabled)
		if err != nil {
			consoleError(c, err)
			return
		}
		c.JSON(http.StatusCreated, actionJSON(act))
	})
	act := one1.Group("/actions/:action_id")
	act.PATCH("", func(c *gin.Context) {
		id, ok := pathIDs(c, pApp, pWF, pAct)
		if !ok {
			return
		}
		var in struct {
			Enabled *bool `json:"enabled"`
		}
		if !bindJSON(c, &in) {
			return
		}
		if in.Enabled == nil {
			writeError(c, http.StatusBadRequest, "validation_failed", "enabled is required")
			return
		}
		a, err := w.SetActionEnabled(c.Request.Context(), actor(c), id[0], id[1], id[2], *in.Enabled)
		if err != nil {
			consoleError(c, err)
			return
		}
		c.JSON(http.StatusOK, actionJSON(a))
	})
	act.DELETE("", func(c *gin.Context) {
		id, ok := pathIDs(c, pApp, pWF, pAct)
		if !ok {
			return
		}
		if err := w.DeleteAction(c.Request.Context(), actor(c), id[0], id[1], id[2]); err != nil {
			consoleError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	})
	act.GET("/templates", func(c *gin.Context) {
		id, ok := pathIDs(c, pApp, pWF, pAct)
		if !ok {
			return
		}
		ts, err := w.ListTemplates(c.Request.Context(), actor(c), id[0], id[1], id[2])
		if err != nil {
			consoleError(c, err)
			return
		}
		out := make([]gin.H, len(ts))
		for i, t := range ts {
			out[i] = templateJSON(t)
		}
		c.JSON(http.StatusOK, gin.H{"data": out})
	})
	tpl := act.Group("/templates/:locale")
	tpl.GET("", func(c *gin.Context) {
		id, ok := pathIDs(c, pApp, pWF, pAct)
		if !ok {
			return
		}
		t, err := w.GetTemplate(c.Request.Context(), actor(c), id[0], id[1], id[2], c.Param("locale"))
		if err != nil {
			consoleError(c, err)
			return
		}
		c.JSON(http.StatusOK, templateJSON(t))
	})
	tpl.PUT("", func(c *gin.Context) {
		id, ok := pathIDs(c, pApp, pWF, pAct)
		if !ok {
			return
		}
		var in workflow.Content
		if !bindJSON(c, &in) {
			return
		}
		t, err := w.PutTemplate(c.Request.Context(), actor(c), id[0], id[1], id[2], c.Param("locale"), in)
		if err != nil {
			consoleError(c, err)
			return
		}
		c.JSON(http.StatusOK, templateJSON(t))
	})
	tpl.DELETE("", func(c *gin.Context) {
		id, ok := pathIDs(c, pApp, pWF, pAct)
		if !ok {
			return
		}
		if err := w.DeleteTemplate(c.Request.Context(), actor(c), id[0], id[1], id[2], c.Param("locale")); err != nil {
			consoleError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	})

	app.POST("/templates/preview", perIP(previewLimit), func(c *gin.Context) {
		id, ok := pathIDs(c, pApp)
		if !ok {
			return
		}
		var in struct {
			Channel    string           `json:"channel"`
			Template   workflow.Content `json:"template"`
			Payload    map[string]any   `json:"payload"`
			Recipient  map[string]any   `json:"recipient"`
			WorkflowID *string          `json:"workflow_id"`
		}
		if !bindJSON(c, &in) {
			return
		}
		pi := workflow.PreviewInput{Channel: in.Channel, Template: in.Template, Payload: in.Payload, Recipient: in.Recipient}
		if in.WorkflowID != nil {
			wid, err := ids.Parse(ids.Workflow, *in.WorkflowID)
			if err != nil {
				writeError(c, http.StatusBadRequest, "validation_failed", "workflow_id is not a valid id")
				return
			}
			pi.WorkflowID = &wid
		}
		r, err := w.Preview(c.Request.Context(), actor(c), id[0], pi)
		if err != nil {
			consoleError(c, err)
			return
		}
		c.JSON(http.StatusOK, previewJSON(r))
	})
}

func workflowJSON(v workflow.View) gin.H {
	acts := make([]gin.H, len(v.Actions))
	for i, a := range v.Actions {
		acts[i] = actionJSON(a)
	}
	var schema any
	if v.PayloadSchema != nil {
		schema = v.PayloadSchema
	}
	return gin.H{
		"id": ids.Format(ids.Workflow, v.ID), "application_id": ids.Format(ids.Application, v.ApplicationID),
		"key": v.Key, "name": v.Name, "description": v.Description, "status": string(v.Status),
		"payload_schema": schema, "actions": acts, "created_at": v.CreatedAt.UTC(), "updated_at": v.UpdatedAt.UTC(),
	}
}

func actionJSON(a store.Action) gin.H {
	locales := a.Locales
	if locales == nil {
		locales = []string{}
	}
	return gin.H{
		"id": ids.Format(ids.Action, a.ID), "channel": a.Channel, "enabled": a.Enabled, "locales": locales,
		"created_at": a.CreatedAt.UTC(), "updated_at": a.UpdatedAt.UTC(),
	}
}

func templateJSON(t workflow.TemplateView) gin.H {
	out := gin.H{"locale": t.Locale, "channel": t.Channel, "updated_at": t.UpdatedAt.UTC()}
	contentInto(out, t.Content)
	return out
}

func contentInto(out gin.H, c workflow.Content) {
	set := func(k string, v *string) {
		if v != nil {
			out[k] = *v
		}
	}
	set("subject", c.Subject)
	set("html", c.HTML)
	set("text", c.Text)
	set("layout", c.Layout)
	if c.Blocks != nil {
		out["blocks"] = c.Blocks
	}
	if c.Body != nil {
		out["body"] = c.Body
	}
}

func previewJSON(r workflow.PreviewResult) gin.H {
	out := gin.H{"channel": r.Channel}
	warnings := make([]gin.H, len(r.Warnings))
	for i, w := range r.Warnings {
		warnings[i] = gin.H{"field": w.Field, "message": w.Message}
	}
	out["warnings"] = warnings
	set := func(k string, v *string) {
		if v != nil {
			out[k] = *v
		}
	}
	set("subject", r.Subject)
	set("html", r.HTML)
	set("text", r.Text)
	if r.Blocks != nil {
		out["blocks"] = r.Blocks
	}
	if r.Body != nil {
		out["body"] = r.Body
	}
	if r.SMS != nil {
		sms := gin.H{"encoding": string(r.SMS.Encoding), "segments": r.SMS.Segments, "units": r.SMS.Units}
		if len(r.SMS.UCS2) > 0 {
			sms["ucs2_characters"] = r.SMS.UCS2
		}
		out["sms"] = sms
	}
	return out
}
