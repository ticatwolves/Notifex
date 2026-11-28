package httptransport

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/ticatwolves/notifex/internal/auth"
	"github.com/ticatwolves/notifex/internal/ids"
	"github.com/ticatwolves/notifex/internal/ratelimit"
	"github.com/ticatwolves/notifex/internal/trigger"
)

// TriggerAPI is what the data-plane routes need from internal/trigger.
type TriggerAPI interface {
	Accept(ctx context.Context, p auth.Principal, req trigger.Request) (trigger.Result, error)
	GetRun(ctx context.Context, p auth.Principal, id uuid.UUID) (trigger.RunView, error)
	GetNotification(ctx context.Context, p auth.Principal, id uuid.UUID) (trigger.NotificationView, error)
}

var idempotencyKeyRE = regexp.MustCompile(`^[A-Za-z0-9_.:-]{8,128}$`)

// requireScope allows keys with no scopes (full access, as before scopes
// were enforced) or keys that list the scope.
func requireScope(scope string) gin.HandlerFunc {
	return func(c *gin.Context) {
		p := principal(c)
		if len(p.Scopes) == 0 {
			c.Next()
			return
		}
		for _, s := range p.Scopes {
			if s == scope {
				c.Next()
				return
			}
		}
		writeError(c, http.StatusForbidden, "forbidden", "this API key lacks the "+scope+" scope")
	}
}

// perKey rate-limits by API key (per process, ADR-014 §8).
func perKey(l *ratelimit.Limiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		if ok, retry := l.Allow(principal(c).APIKeyID.String()); !ok {
			rateLimited(c, "rate_limited", retry)
			return
		}
		c.Next()
	}
}

func registerTrigger(r *gin.Engine, d Deps) {
	key := requireAPIKey(d.APIKeys, d.Limits.APIKeyFail)
	t := d.Triggers

	r.POST("/v1/trigger", key, requireScope("trigger"), perKey(d.Limits.Trigger), func(c *gin.Context) {
		var req trigger.Request
		if !bindJSON(c, &req) {
			return
		}
		if k := c.GetHeader("Idempotency-Key"); k != "" {
			if !idempotencyKeyRE.MatchString(k) {
				writeError(c, http.StatusBadRequest, "validation_failed", "Idempotency-Key must be 8-128 characters of letters, digits and _ . : -")
				return
			}
			req.IdempotencyKey = k
		}
		req.Source = "rest"
		res, err := t.Accept(c.Request.Context(), principal(c), req)
		if err != nil {
			dataError(c, err)
			return
		}
		c.JSON(http.StatusAccepted, gin.H{
			"run_id": ids.Format(ids.Run, res.Run.ID), "status": string(res.Run.Status), "notifications": outcomesJSON(res.Notifications),
		})
	})
	r.GET("/v1/runs/:run_id", key, requireScope("read"), func(c *gin.Context) {
		id, ok := pathID(c, "run_id", ids.Run)
		if !ok {
			return
		}
		v, err := t.GetRun(c.Request.Context(), principal(c), id)
		if err != nil {
			dataError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"id": ids.Format(ids.Run, v.Run.ID), "workflow": v.Run.WorkflowKey, "status": string(v.Run.Status),
			"source": v.Run.Source, "created_at": v.Run.CreatedAt.UTC(), "schedule_at": utcPtr(v.Run.ScheduleAt),
			"notifications": outcomesJSON(v.Notifications),
		})
	})
	r.GET("/v1/notifications/:notification_id", key, requireScope("read"), func(c *gin.Context) {
		id, ok := pathID(c, "notification_id", ids.Notification)
		if !ok {
			return
		}
		v, err := t.GetNotification(c.Request.Context(), principal(c), id)
		if err != nil {
			dataError(c, err)
			return
		}
		n := v.Notification
		evs := make([]gin.H, len(v.Events))
		for i, e := range v.Events {
			ev := gin.H{"type": e.Type, "at": e.CreatedAt.UTC()}
			if e.Detail != nil {
				ev["detail"] = e.Detail
			}
			evs[i] = ev
		}
		out := gin.H{
			"id": ids.Format(ids.Notification, n.ID), "run_id": ids.Format(ids.Run, n.RunID), "channel": n.Channel,
			"status": n.Status, "recipient_masked": n.RecipientMasked, "attempt": n.Attempt,
			"payload_expired": n.PurgedAt != nil, "created_at": n.CreatedAt.UTC(), "terminal_at": utcPtr(n.TerminalAt), "events": evs,
		}
		if n.RecipientRef != "" {
			out["recipient_ref"] = n.RecipientRef
		}
		if n.LastErrorCode != "" {
			out["last_error"] = gin.H{"code": n.LastErrorCode, "message": n.LastError}
		}
		c.JSON(http.StatusOK, out)
	})
}

func outcomesJSON(os []trigger.Outcome) []gin.H {
	out := make([]gin.H, len(os))
	for i, o := range os {
		h := gin.H{"id": ids.Format(ids.Notification, o.ID), "channel": o.Channel, "status": o.Status}
		if o.RecipientRef != "" {
			h["recipient_ref"] = o.RecipientRef
		}
		if o.ErrorCode != "" {
			e := gin.H{"code": o.ErrorCode, "message": o.ErrorMessage}
			if o.RetryAfter != nil {
				e["retry_after"] = o.RetryAfter.UTC().Format(time.RFC3339)
			}
			h["error"] = e
		}
		out[i] = h
	}
	return out
}

// dataError maps trigger and store errors to the docs/03 codes.
func dataError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, trigger.ErrWorkflowNotFound):
		writeError(c, http.StatusNotFound, "workflow_not_found", "no workflow with this key in the API key's application")
	case errors.Is(err, trigger.ErrWorkflowNotActive):
		writeError(c, http.StatusConflict, "workflow_not_active", "the workflow is not active")
	case errors.Is(err, trigger.ErrIdempotencyConflict):
		writeError(c, http.StatusConflict, "idempotency_conflict", "this Idempotency-Key was used with a different request")
	case errors.Is(err, trigger.ErrScheduleHorizon):
		writeError(c, http.StatusBadRequest, "schedule_horizon_exceeded", "schedule_at must be at most 7 days ahead")
	default: // validation (FieldError), not found, unavailable, internal
		consoleError(c, err)
	}
}
