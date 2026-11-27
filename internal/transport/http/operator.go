package httptransport

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/ticatwolves/notifex/internal/auth"
	"github.com/ticatwolves/notifex/internal/ids"
	"github.com/ticatwolves/notifex/internal/observability"
	"github.com/ticatwolves/notifex/internal/operator"
	"github.com/ticatwolves/notifex/internal/store"
)

// OperatorRealm is what /v1/operator needs from internal/operator.
type OperatorRealm interface {
	Login(ctx context.Context, email, password, code, userAgent string) (string, store.Session, error)
	Logout(ctx context.Context, p operator.Principal) error
	Authenticate(ctx context.Context, token string) (operator.Principal, error)
	SearchTenants(ctx context.Context, p operator.Principal, q string, status *store.TenantStatus, page store.Page) ([]store.Tenant, error)
	GetTenant(ctx context.Context, p operator.Principal, tid store.TenantID) (store.Tenant, error)
	UpdateTenant(ctx context.Context, p operator.Principal, tid store.TenantID, patch operator.TenantPatch) (store.Tenant, error)
}

const ctxOperator = "operator_principal"

func registerOperator(r *gin.Engine, d Deps) {
	o := d.Operators
	if d.Limits.OperatorLogin == nil {
		d.Limits.OperatorLogin = DefaultLimits().OperatorLogin
	}
	g := r.Group("/v1/operator")

	g.POST("/auth/login", perIP(d.Limits.OperatorLogin), func(c *gin.Context) {
		var in struct {
			Email    string `json:"email"`
			Password string `json:"password"`
			TOTP     string `json:"totp"`
		}
		if !bindJSON(c, &in) {
			return
		}
		tok, sess, err := o.Login(c.Request.Context(), in.Email, in.Password, in.TOTP, c.Request.UserAgent())
		if err != nil {
			operatorError(c, err)
			return
		}
		c.JSON(http.StatusOK, sessionCreatedResponse{
			Token: tok, SessionID: ids.Format(ids.Session, sess.ID), ExpiresAt: sess.AbsoluteExpiresAt.UTC(),
		})
	})

	authed := g.Group("", requireOperator(o))
	authed.POST("/auth/logout", func(c *gin.Context) {
		if err := o.Logout(c.Request.Context(), operatorPrincipal(c)); err != nil {
			operatorError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	})
	authed.GET("/tenants", func(c *gin.Context) {
		p, ok := pageParams(c, ids.Tenant)
		if !ok {
			return
		}
		q := c.Query("q")
		if len(q) > 63 {
			writeError(c, http.StatusBadRequest, "validation_failed", "q is too long")
			return
		}
		var status *store.TenantStatus
		if v := c.Query("status"); v != "" {
			s := store.TenantStatus(v)
			if s != store.TenantActive && s != store.TenantSuspended && s != store.TenantDeleted {
				writeError(c, http.StatusBadRequest, "validation_failed", "status is not valid")
				return
			}
			status = &s
		}
		ts, err := o.SearchTenants(c.Request.Context(), operatorPrincipal(c), q, status, p)
		if err != nil {
			operatorError(c, err)
			return
		}
		out := make([]gin.H, len(ts))
		for i, t := range ts {
			out[i] = operatorTenantJSON(t)
		}
		var next any
		if len(ts) == p.Limit {
			next = ids.Format(ids.Tenant, ts[len(ts)-1].ID.UUID())
		}
		c.JSON(http.StatusOK, gin.H{"data": out, "next_cursor": next})
	})
	authed.GET("/tenants/:tenant_id", func(c *gin.Context) {
		id, ok := pathID(c, "tenant_id", ids.Tenant)
		if !ok {
			return
		}
		t, err := o.GetTenant(c.Request.Context(), operatorPrincipal(c), store.TenantID(id))
		if err != nil {
			operatorError(c, err)
			return
		}
		c.JSON(http.StatusOK, operatorTenantJSON(t))
	})
	authed.PATCH("/tenants/:tenant_id", func(c *gin.Context) {
		id, ok := pathID(c, "tenant_id", ids.Tenant)
		if !ok {
			return
		}
		var in struct {
			Status                *store.TenantStatus `json:"status"`
			Plan                  *string             `json:"plan"`
			Sandbox               *bool               `json:"sandbox"`
			PlatformSMSDailyLimit *int                `json:"platform_sms_daily_limit"`
			Quotas                map[string]int64    `json:"quotas"`
			Reason                string              `json:"reason"`
		}
		if !bindJSON(c, &in) {
			return
		}
		t, err := o.UpdateTenant(c.Request.Context(), operatorPrincipal(c), store.TenantID(id), operator.TenantPatch{
			Status: in.Status, Plan: in.Plan, Sandbox: in.Sandbox,
			PlatformSMSDailyLimit: in.PlatformSMSDailyLimit, Quotas: in.Quotas, Reason: in.Reason,
		})
		if err != nil {
			operatorError(c, err)
			return
		}
		c.JSON(http.StatusOK, operatorTenantJSON(t))
	})
}

// requireOperator authenticates `Authorization: Bearer nfo_…`. Console
// sessions and API keys are rejected by the token prefix.
func requireOperator(o OperatorRealm) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, ok := bearer(c.GetHeader("Authorization"))
		if !ok {
			unauthenticated(c)
			return
		}
		p, err := o.Authenticate(c.Request.Context(), raw)
		switch {
		case errors.Is(err, auth.ErrUnavailable):
			c.Header("Retry-After", "1")
			writeError(c, http.StatusServiceUnavailable, "unavailable", "authentication temporarily unavailable")
			return
		case err != nil:
			unauthenticated(c)
			return
		}
		c.Set(ctxOperator, p)
		ctx := observability.WithLogAttrs(c.Request.Context(),
			slog.String("operator_id", p.OperatorID.String()), slog.String("session_id", p.SessionID.String()))
		trace.SpanFromContext(ctx).SetAttributes(attribute.String("notifex.operator_id", p.OperatorID.String()))
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

func operatorPrincipal(c *gin.Context) operator.Principal {
	v, _ := c.Get(ctxOperator)
	return v.(operator.Principal) // set by requireOperator before any handler runs
}

func operatorError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, operator.ErrReasonRequired):
		writeError(c, http.StatusBadRequest, "validation_failed", "reason is required")
	case errors.Is(err, operator.ErrNothingToUpdate):
		writeError(c, http.StatusBadRequest, "validation_failed", "nothing to update")
	case errors.Is(err, auth.ErrInvalidCredentials):
		c.Header("WWW-Authenticate", `Bearer realm="notifex-operator"`)
		writeError(c, http.StatusUnauthorized, "invalid_credentials", "email, password or code is incorrect")
	default:
		consoleError(c, err)
	}
}

func operatorTenantJSON(t store.Tenant) gin.H {
	quotas := t.Quotas
	if quotas == nil {
		quotas = map[string]int64{}
	}
	return gin.H{
		"id": ids.Format(ids.Tenant, t.ID.UUID()), "name": t.Name, "slug": t.Slug, "status": string(t.Status),
		"plan": t.Plan, "sandbox": t.Sandbox, "platform_sms_daily_limit": t.PlatformSMSDailyLimit,
		"quotas": quotas, "dek_shredded": t.DEKShredded, "created_at": t.CreatedAt.UTC(),
	}
}
