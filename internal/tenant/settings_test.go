package tenant_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ticatwolves/notifex/internal/store"
	"github.com/ticatwolves/notifex/internal/tenant"
)

func sp(s string) *string { return &s }
func ip(n int) *int       { return &n }

func TestBrandValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		b     tenant.Brand
		field string
	}{
		{"http logo", tenant.Brand{LogoURL: sp("http://x.test/l.png")}, "logo_url"},
		{"logo with credentials", tenant.Brand{LogoURL: sp("https://u:p@x.test/l.png")}, "logo_url"},
		{"javascript support url", tenant.Brand{SupportURL: sp("javascript:alert(1)")}, "support_url"},
		{"bad colour", tenant.Brand{Colors: &tenant.BrandColors{Primary: sp("red")}}, "colors.primary"},
		{"short colour", tenant.Brand{Colors: &tenant.BrandColors{Text: sp("#fff")}}, "colors.text"},
		{"reply-to with name", tenant.Brand{Email: &tenant.BrandEmail{ReplyTo: sp("Ops <ops@x.test>")}}, "email.reply_to"},
		{"from name header injection", tenant.Brand{Email: &tenant.BrandEmail{FromName: sp("Acme\r\nBcc: x@y.test")}}, "email.from_name"},
		{"display name too long", tenant.Brand{DisplayName: sp(strings.Repeat("a", 101))}, "display_name"},
		{"footer control char", tenant.Brand{FooterText: sp("a\x00b")}, "footer_text"},
		{"bad locale", tenant.Brand{DefaultLocale: sp("english")}, "default_locale"},
	}
	for _, tc := range cases {
		err := tc.b.Validate()
		require.ErrorIs(t, err, store.ErrInvalid, tc.name)
		assert.Contains(t, err.Error(), tc.field, tc.name)
	}
	ok := tenant.Brand{
		DisplayName: sp("Acme"), LogoURL: sp("https://cdn.acme.test/l.png"), FooterText: sp("Line 1\nLine 2\tend"),
		Colors: &tenant.BrandColors{Primary: sp("#112233")}, Email: &tenant.BrandEmail{FromName: sp("Acme"), ReplyTo: sp("help@acme.test")},
		DefaultLocale: sp("en-IN"), SupportURL: sp("https://acme.test/help"), LegalText: sp("© Acme"),
	}
	require.NoError(t, ok.Validate())
}

func TestPolicyValidation(t *testing.T) {
	t.Parallel()
	qh := func(start, end, tz string) *tenant.QuietHours {
		return &tenant.QuietHours{Start: start, End: end, Timezone: tz}
	}
	cases := []struct {
		name     string
		p        tenant.SendingPolicy
		override bool
		field    string
	}{
		{"unknown channel", tenant.SendingPolicy{AllowedChannels: []string{"fax"}}, false, "allowed_channels"},
		{"duplicate channel", tenant.SendingPolicy{AllowedChannels: []string{"sms", "sms"}}, false, "allowed_channels"},
		{"bad start", tenant.SendingPolicy{QuietHours: qh("25:00", "07:00", "Asia/Kolkata")}, false, "quiet_hours.start"},
		{"bad end", tenant.SendingPolicy{QuietHours: qh("22:00", "7am", "Asia/Kolkata")}, false, "quiet_hours.end"},
		{"empty window", tenant.SendingPolicy{QuietHours: qh("22:00", "22:00", "Asia/Kolkata")}, false, "quiet_hours"},
		{"unknown zone", tenant.SendingPolicy{QuietHours: qh("22:00", "07:00", "Mars/Olympus")}, false, "quiet_hours.timezone"},
		{"Local zone", tenant.SendingPolicy{QuietHours: qh("22:00", "07:00", "Local")}, false, "quiet_hours.timezone"},
		{"quiet hours channel", tenant.SendingPolicy{QuietHours: &tenant.QuietHours{Start: "22:00", End: "07:00", Timezone: "UTC", Channels: []string{"pager"}}}, false, "quiet_hours.channels"},
		{"rate limit channel", tenant.SendingPolicy{RateLimitsPerMinute: map[string]int{"pager": 5}}, false, "rate_limits_per_minute"},
		{"rate limit zero", tenant.SendingPolicy{RateLimitsPerMinute: map[string]int{"sms": 0}}, false, "rate_limits_per_minute.sms"},
		{"max attempts", tenant.SendingPolicy{MaxAttempts: ip(11)}, false, "max_attempts"},
		{"retention 8", tenant.SendingPolicy{PayloadRetentionDays: ip(8)}, false, "payload_retention_days"},
		{"retention 0", tenant.SendingPolicy{PayloadRetentionDays: ip(0)}, false, "payload_retention_days"},
		{"retention on override", tenant.SendingPolicy{PayloadRetentionDays: ip(3)}, true, "tenant policy only"},
	}
	for _, tc := range cases {
		err := tc.p.Validate(tc.override)
		require.ErrorIs(t, err, store.ErrInvalid, tc.name)
		assert.Contains(t, err.Error(), tc.field, tc.name)
	}
	require.NoError(t, tenant.SendingPolicy{
		AllowedChannels: []string{"email", "sms"}, QuietHours: &tenant.QuietHours{Start: "22:00", End: "07:00", Timezone: "Asia/Kolkata", Channels: []string{"sms"}},
		RateLimitsPerMinute: map[string]int{"sms": 60}, MaxAttempts: ip(3), PayloadRetentionDays: ip(1),
	}.Validate(false))
}

func TestBrandAndPolicyLifecycle(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	owner := e.user(t, true)
	ten := e.tenantFor(t, owner)
	a := e.access(t, owner, ten)
	app, err := e.svc.CreateApplication(ctx, a, store.NewApplication{Name: "Shop", Key: "shop"})
	require.NoError(t, err)

	// Defaults before anything is saved.
	b, err := e.svc.GetBrand(ctx, a)
	require.NoError(t, err)
	assert.Equal(t, "en", *b.DefaultLocale)
	assert.Nil(t, b.UpdatedAt)
	p, err := e.svc.GetPolicy(ctx, a)
	require.NoError(t, err)
	assert.Equal(t, 5, *p.MaxAttempts)
	assert.Equal(t, 7, *p.PayloadRetentionDays)
	assert.Equal(t, []string{}, p.AllowedChannels)

	// Tenant brand, then an override that changes one colour and the locale.
	_, err = e.svc.PutBrand(ctx, a, tenant.Brand{
		DisplayName: sp("Acme"), Colors: &tenant.BrandColors{Primary: sp("#111111"), Background: sp("#ffffff")},
		Email: &tenant.BrandEmail{FromName: sp("Acme"), ReplyTo: sp("help@acme.test")},
	})
	require.NoError(t, err)
	ov, eff, err := e.svc.AppBrand(ctx, a, app.ID)
	require.NoError(t, err)
	assert.Nil(t, ov, "no override yet")
	assert.Equal(t, "Acme", *eff.DisplayName)

	ovp, eff, err := e.svc.PutAppBrand(ctx, a, app.ID, tenant.Brand{Colors: &tenant.BrandColors{Primary: sp("#ff0000")}, DefaultLocale: sp("hi")})
	require.NoError(t, err)
	require.NotNil(t, ovp)
	assert.Nil(t, ovp.DisplayName, "override stores only what it sets")
	assert.Equal(t, "#ff0000", *eff.Colors.Primary, "override wins")
	assert.Equal(t, "#ffffff", *eff.Colors.Background, "nested fields merge one by one")
	assert.Equal(t, "Acme", *eff.DisplayName, "unset fields inherit")
	assert.Equal(t, "hi", *eff.DefaultLocale)
	assert.Equal(t, "help@acme.test", *eff.Email.ReplyTo)

	// Policy: tenant retention 3; override changes attempts and one rate limit.
	_, err = e.svc.PutPolicy(ctx, a, tenant.SendingPolicy{PayloadRetentionDays: ip(3), RateLimitsPerMinute: map[string]int{"sms": 10, "email": 100}})
	require.NoError(t, err)
	_, _, err = e.svc.PutAppPolicy(ctx, a, app.ID, tenant.SendingPolicy{PayloadRetentionDays: ip(1)})
	require.ErrorIs(t, err, store.ErrInvalid, "retention can't be overridden")
	_, effp, err := e.svc.PutAppPolicy(ctx, a, app.ID, tenant.SendingPolicy{MaxAttempts: ip(2), RateLimitsPerMinute: map[string]int{"sms": 1}})
	require.NoError(t, err)
	assert.Equal(t, 2, *effp.MaxAttempts)
	assert.Equal(t, 3, *effp.PayloadRetentionDays, "retention always comes from the tenant")
	assert.Equal(t, map[string]int{"sms": 1, "email": 100}, effp.RateLimitsPerMinute)
	direct, err := e.svc.EffectivePolicy(ctx, ten.ID, app.ID)
	require.NoError(t, err)
	assert.Equal(t, effp.SendingPolicy, direct.SendingPolicy)

	// Removing overrides falls back to the tenant.
	require.NoError(t, e.svc.DeleteAppBrand(ctx, a, app.ID))
	require.ErrorIs(t, e.svc.DeleteAppBrand(ctx, a, app.ID), store.ErrNotFound)
	_, eff, err = e.svc.AppBrand(ctx, a, app.ID)
	require.NoError(t, err)
	assert.Equal(t, "#111111", *eff.Colors.Primary)
	require.NoError(t, e.svc.DeleteAppPolicy(ctx, a, app.ID))

	// Audit: every change, before and after, reply-to masked.
	evs, err := e.st.Audit().List(ctx, ten.ID, store.Page{Limit: store.MaxPageSize})
	require.NoError(t, err)
	actions := map[string]store.AuditEvent{}
	for _, ev := range evs {
		if _, seen := actions[ev.Action]; !seen {
			actions[ev.Action] = ev
		}
	}
	for _, act := range []string{"brand.update", "brand.override.update", "brand.override.delete", "policy.update", "policy.override.update", "policy.override.delete"} {
		assert.Contains(t, actions, act)
	}
	raw, _ := json.Marshal(actions["brand.update"].After)
	assert.NotContains(t, string(raw), "help@acme.test", "reply-to is masked in audit")
	assert.Equal(t, float64(3), actions["policy.update"].After["payload_retention_days"])
}

func TestSettingsPermissionsAndScope(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	owner := e.user(t, true)
	ten := e.tenantFor(t, owner)
	oa := e.access(t, owner, ten)
	app, err := e.svc.CreateApplication(ctx, oa, store.NewApplication{Name: "Shop", Key: "shop"})
	require.NoError(t, err)

	for _, role := range []store.Role{store.RoleViewer, store.RoleDeveloper} {
		a := e.member(t, ten, role)
		_, err := e.svc.GetBrand(ctx, a)
		require.NoError(t, err, "%s can read", role)
		_, _, err = e.svc.AppPolicy(ctx, a, app.ID)
		require.NoError(t, err, "%s can read", role)
		_, err = e.svc.PutBrand(ctx, a, tenant.Brand{})
		require.ErrorIs(t, err, tenant.ErrForbidden, "%s can't write", role)
		_, err = e.svc.PutPolicy(ctx, a, tenant.SendingPolicy{})
		require.ErrorIs(t, err, tenant.ErrForbidden)
		_, _, err = e.svc.PutAppBrand(ctx, a, app.ID, tenant.Brand{})
		require.ErrorIs(t, err, tenant.ErrForbidden)
		require.ErrorIs(t, e.svc.DeleteAppPolicy(ctx, a, app.ID), tenant.ErrForbidden)
	}
	admin := e.member(t, ten, store.RoleAdmin)
	_, err = e.svc.PutPolicy(ctx, admin, tenant.SendingPolicy{MaxAttempts: ip(4)})
	require.NoError(t, err, "admins manage settings")

	// Another tenant's application, or an unknown one, is not found.
	owner2 := e.user(t, true)
	other := e.tenantFor(t, owner2)
	otherApp, err := e.svc.CreateApplication(ctx, e.access(t, owner2, other), store.NewApplication{Name: "X", Key: "x"})
	require.NoError(t, err)
	for _, id := range []uuid.UUID{otherApp.ID, uuid.Must(uuid.NewV7())} {
		_, _, err = e.svc.AppBrand(ctx, oa, id)
		require.ErrorIs(t, err, store.ErrNotFound)
		_, _, err = e.svc.PutAppBrand(ctx, oa, id, tenant.Brand{DisplayName: sp("pwned")})
		require.ErrorIs(t, err, store.ErrNotFound)
		_, _, err = e.svc.PutAppPolicy(ctx, oa, id, tenant.SendingPolicy{MaxAttempts: ip(1)})
		require.ErrorIs(t, err, store.ErrNotFound)
		require.ErrorIs(t, e.svc.DeleteAppBrand(ctx, oa, id), store.ErrNotFound)
	}
}
