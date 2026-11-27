package httptransport

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/ticatwolves/notifex/internal/ids"
	"github.com/ticatwolves/notifex/internal/tenant"
)

// registerSettings serves brand profiles and policies, per tenant and per
// application override (docs/03, ADR-006).
func registerSettings(one *gin.RouterGroup, t TenantAdmin) {
	one.GET("/brand", func(c *gin.Context) {
		b, err := t.GetBrand(c.Request.Context(), actor(c))
		if err != nil {
			consoleError(c, err)
			return
		}
		c.JSON(http.StatusOK, brandJSON(b))
	})
	one.PUT("/brand", func(c *gin.Context) {
		var in tenant.Brand
		if !bindJSON(c, &in) {
			return
		}
		b, err := t.PutBrand(c.Request.Context(), actor(c), in)
		if err != nil {
			consoleError(c, err)
			return
		}
		c.JSON(http.StatusOK, brandJSON(b))
	})
	one.GET("/policy", func(c *gin.Context) {
		p, err := t.GetPolicy(c.Request.Context(), actor(c))
		if err != nil {
			consoleError(c, err)
			return
		}
		c.JSON(http.StatusOK, policyJSON(p))
	})
	one.PUT("/policy", func(c *gin.Context) {
		var in tenant.SendingPolicy
		if !bindJSON(c, &in) {
			return
		}
		p, err := t.PutPolicy(c.Request.Context(), actor(c), in)
		if err != nil {
			consoleError(c, err)
			return
		}
		c.JSON(http.StatusOK, policyJSON(p))
	})

	app := one.Group("/applications/:application_id")
	app.GET("/brand", func(c *gin.Context) {
		id, ok := pathID(c, "application_id", ids.Application)
		if !ok {
			return
		}
		ov, eff, err := t.AppBrand(c.Request.Context(), actor(c), id)
		if err != nil {
			consoleError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"override": ov, "effective": brandJSON(eff)})
	})
	app.PUT("/brand", func(c *gin.Context) {
		id, ok := pathID(c, "application_id", ids.Application)
		if !ok {
			return
		}
		var in tenant.Brand
		if !bindJSON(c, &in) {
			return
		}
		ov, eff, err := t.PutAppBrand(c.Request.Context(), actor(c), id, in)
		if err != nil {
			consoleError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"override": ov, "effective": brandJSON(eff)})
	})
	app.DELETE("/brand", func(c *gin.Context) {
		id, ok := pathID(c, "application_id", ids.Application)
		if !ok {
			return
		}
		if err := t.DeleteAppBrand(c.Request.Context(), actor(c), id); err != nil {
			consoleError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	})
	app.GET("/policy", func(c *gin.Context) {
		id, ok := pathID(c, "application_id", ids.Application)
		if !ok {
			return
		}
		ov, eff, err := t.AppPolicy(c.Request.Context(), actor(c), id)
		if err != nil {
			consoleError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"override": ov, "effective": policyJSON(eff)})
	})
	app.PUT("/policy", func(c *gin.Context) {
		id, ok := pathID(c, "application_id", ids.Application)
		if !ok {
			return
		}
		var in tenant.SendingPolicy
		if !bindJSON(c, &in) {
			return
		}
		ov, eff, err := t.PutAppPolicy(c.Request.Context(), actor(c), id, in)
		if err != nil {
			consoleError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"override": ov, "effective": policyJSON(eff)})
	})
	app.DELETE("/policy", func(c *gin.Context) {
		id, ok := pathID(c, "application_id", ids.Application)
		if !ok {
			return
		}
		if err := t.DeleteAppPolicy(c.Request.Context(), actor(c), id); err != nil {
			consoleError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	})
}

// brandJSON is the Brand schema: the profile's fields plus updated_at.
func brandJSON(b tenant.BrandView) gin.H {
	out := gin.H{"updated_at": utcPtr(b.UpdatedAt)}
	set := func(k string, v *string) {
		if v != nil {
			out[k] = *v
		}
	}
	set("display_name", b.DisplayName)
	set("logo_url", b.LogoURL)
	set("footer_text", b.FooterText)
	set("legal_text", b.LegalText)
	set("support_url", b.SupportURL)
	set("default_locale", b.DefaultLocale)
	if b.Colors != nil {
		out["colors"] = b.Colors
	}
	if b.Email != nil {
		out["email"] = b.Email
	}
	return out
}

// policyJSON is the Policy schema (every field present).
func policyJSON(p tenant.SendingPolicyView) gin.H {
	return gin.H{
		"allowed_channels": p.AllowedChannels, "quiet_hours": p.QuietHours,
		"rate_limits_per_minute": p.RateLimitsPerMinute, "max_attempts": p.MaxAttempts,
		"payload_retention_days": p.PayloadRetentionDays, "updated_at": utcPtr(p.UpdatedAt),
	}
}
