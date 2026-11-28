package credential_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ticatwolves/notifex/internal/credential"
	"github.com/ticatwolves/notifex/internal/crypto"
	"github.com/ticatwolves/notifex/internal/mail"
	"github.com/ticatwolves/notifex/internal/store"
	"github.com/ticatwolves/notifex/internal/store/entstore"
	"github.com/ticatwolves/notifex/internal/store/storetest"
	"github.com/ticatwolves/notifex/internal/tenant"
)

// Fake credentials are built from repeats so no key-shaped, high-entropy
// literal sits in the repo (secret scanners would flag them).
var (
	sgKey      = "SG." + strings.Repeat("a", 16) + "." + strings.Repeat("b", 20) + "K1K1"
	twToken    = strings.Repeat("ab", 16)
	twSID      = "AC" + strings.Repeat("cd", 16)
	slackHook  = "https://hooks.slack.com/services/T000/B000/" + strings.Repeat("X", 24)
	slackToken = "xoxb-" + strings.Repeat("1", 10) + "-" + strings.Repeat("a", 10)
)

type env struct {
	st     *entstore.Store
	ten    *tenant.Service
	cipher *crypto.Cipher
	svc    *credential.Service
}

func newEnv(t *testing.T) env {
	t.Helper()
	mk := make([]byte, 32)
	_, _ = rand.Read(mk)
	kp, err := crypto.NewLocal(mk)
	require.NoError(t, err)
	st := entstore.New(storetest.DB(t))
	c := crypto.NewCipher(kp, st.Tenants(), crypto.CipherOptions{})
	ts := tenant.NewService(st, kp, &mail.Recorder{}, slog.New(slog.NewTextHandler(io.Discard, nil)), "https://console.test", tenant.DefaultPolicy)
	return env{st: st, ten: ts, cipher: c, svc: credential.NewService(st, c)}
}

type world struct {
	a   tenant.Actor
	app store.Application
	dev store.Environment
	prd store.Environment
}

func (e env) world(t *testing.T) world {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	u, err := e.st.Users().Create(ctx, store.NewUser{Email: "u" + strings.ReplaceAll(uuid.NewString()[:12], "-", "") + "@example.com", EmailVerifiedAt: &now})
	require.NoError(t, err)
	user := tenant.User{ID: u.ID, Email: u.Email, EmailVerified: true, ReauthenticatedAt: now}
	ten, err := e.ten.Create(ctx, user, "Acme", "t-"+strings.ReplaceAll(uuid.NewString()[:10], "-", ""))
	require.NoError(t, err)
	a, _, err := e.ten.Access(ctx, user, ten.ID)
	require.NoError(t, err)
	app, err := e.ten.CreateApplication(ctx, a, store.NewApplication{Name: "Shop", Key: "shop"})
	require.NoError(t, err)
	dev, err := e.ten.CreateEnvironment(ctx, a, app.ID, store.EnvDev)
	require.NoError(t, err)
	prd, err := e.ten.CreateEnvironment(ctx, a, app.ID, store.EnvProd)
	require.NoError(t, err)
	return world{a: a, app: app, dev: dev, prd: prd}
}

func (e env) member(t *testing.T, w world, role store.Role) tenant.Actor {
	t.Helper()
	ctx := context.Background()
	u, err := e.st.Users().Create(ctx, store.NewUser{Email: "m" + strings.ReplaceAll(uuid.NewString()[:12], "-", "") + "@example.com"})
	require.NoError(t, err)
	_, err = e.st.Memberships().Create(ctx, w.a.TenantID, u.ID, role)
	require.NoError(t, err)
	m, _, err := e.ten.Access(ctx, tenant.User{ID: u.ID, Email: u.Email}, w.a.TenantID)
	require.NoError(t, err)
	return m
}

func sendgrid(scopeApp, scopeEnv *uuid.UUID) credential.CreateInput {
	return credential.CreateInput{Channel: "email", Provider: "sendgrid", Name: "SG", ApplicationID: scopeApp, EnvironmentID: scopeEnv,
		Config: map[string]any{"from_email": "ops@acme.test"}, Secret: map[string]any{"api_key": sgKey}}
}

func TestSecretsAreSealedAndNeverReturned(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	w := e.world(t)

	c, gen, err := e.svc.Create(ctx, w.a, sendgrid(nil, nil))
	require.NoError(t, err)
	assert.Empty(t, gen)
	assert.True(t, c.HasSecret)
	assert.Equal(t, sgKey[len(sgKey)-4:], c.SecretHint)
	assert.Equal(t, "pending", c.Status)
	raw, _ := json.Marshal(c)
	assert.NotContains(t, string(raw), sgKey, "metadata never carries the secret")

	// At rest it's ciphertext bound to this row; the dispatcher can open it.
	row, err := e.st.Credentials().Get(ctx, w.a.TenantID, c.ID)
	require.NoError(t, err)
	assert.NotContains(t, string(row.SecretCiphertext), sgKey)
	got, err := e.svc.Secret(ctx, w.a.TenantID, c.ID)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"api_key": sgKey}, got)

	// AAD: the ciphertext moved to another row of the same tenant won't open.
	other, _, err := e.svc.Create(ctx, w.a, sendgrid(&w.app.ID, nil))
	require.NoError(t, err)
	db := storetest.DB(t)
	_, err = db.ExecContext(ctx, `UPDATE channel_credentials SET secret_ciphertext = $1 WHERE id = $2`, row.SecretCiphertext, other.ID)
	require.NoError(t, err)
	_, err = e.svc.Secret(ctx, w.a.TenantID, other.ID)
	require.ErrorIs(t, err, crypto.ErrDecrypt)

	// Rotation: new secret, new hint, rotated_at; old plaintext gone.
	newKey := "SG." + strings.Repeat("Z", 16) + "." + strings.Repeat("y", 20) + "WXYZ"
	rot, _, err := e.svc.Rotate(ctx, w.a, c.ID, map[string]any{"api_key": newKey})
	require.NoError(t, err)
	assert.Equal(t, "WXYZ", rot.SecretHint)
	require.NotNil(t, rot.RotatedAt)
	got, err = e.svc.Secret(ctx, w.a.TenantID, c.ID)
	require.NoError(t, err)
	assert.Equal(t, newKey, got["api_key"])

	// Audit records never contain a secret.
	evs, err := e.st.Audit().List(ctx, w.a.TenantID, store.Page{Limit: store.MaxPageSize})
	require.NoError(t, err)
	trail, _ := json.Marshal(evs)
	assert.NotContains(t, string(trail), sgKey)
	assert.NotContains(t, string(trail), newKey)
	assert.NotContains(t, string(trail), "ops@acme.test", "config emails masked")
	assert.Contains(t, string(trail), "credential.rotate")

	// Crypto-shred: once the tenant's DEK is gone, no secret opens.
	require.NoError(t, e.st.Tenants().ShredDEK(ctx, w.a.TenantID))
	e.cipher.Forget(w.a.TenantID.UUID())
	_, err = e.svc.Secret(ctx, w.a.TenantID, c.ID)
	require.ErrorIs(t, err, crypto.ErrDataKeyDestroyed)
}

func TestGeneratedAndSecretlessProviders(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	w := e.world(t)

	c, gen, err := e.svc.Create(ctx, w.a, credential.CreateInput{Channel: "webhook", Provider: "signing", Name: "hooks"})
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(gen, "whsec_"), "generated secret shown once")
	assert.Equal(t, gen[len(gen)-4:], c.SecretHint)
	_, gen2, err := e.svc.Rotate(ctx, w.a, c.ID, nil)
	require.NoError(t, err)
	assert.NotEqual(t, gen, gen2)
	_, _, err = e.svc.Create(ctx, w.a, credential.CreateInput{Channel: "webhook", Provider: "signing", Name: "x", ApplicationID: &w.app.ID, Secret: map[string]any{"secret": "mine"}})
	require.ErrorIs(t, err, store.ErrInvalid, "callers can't choose a generated secret")

	ses, _, err := e.svc.Create(ctx, w.a, credential.CreateInput{Channel: "email", Provider: "ses", Name: "SES", ApplicationID: &w.app.ID,
		Config: map[string]any{"role_arn": "arn:aws:iam::123456789012:role/notifex-sender", "region": "ap-south-1", "from_email": "a@acme.test"}})
	require.NoError(t, err)
	assert.False(t, ses.HasSecret, "SES uses a role, not a stored key")
	ext := ses.Config["external_id"].(string)
	assert.True(t, strings.HasPrefix(ext, "nfx-"))
	upd, err := e.svc.Update(ctx, w.a, ses.ID, credential.Patch{Config: map[string]any{"role_arn": "arn:aws:iam::123456789012:role/other", "region": "eu-west-1", "from_email": "a@acme.test"}})
	require.NoError(t, err)
	assert.Equal(t, ext, upd.Config["external_id"], "external_id survives config changes")
	_, err = e.svc.Update(ctx, w.a, ses.ID, credential.Patch{Config: map[string]any{"role_arn": "arn:aws:iam::123456789012:role/x", "region": "eu-west-1", "from_email": "a@acme.test", "external_id": "mine"}})
	require.ErrorIs(t, err, store.ErrInvalid, "external_id is read-only")
	_, _, err = e.svc.Rotate(ctx, w.a, ses.ID, map[string]any{"x": "y"})
	require.ErrorIs(t, err, store.ErrInvalid, "nothing to rotate")

	pl, _, err := e.svc.Create(ctx, w.a, credential.CreateInput{Channel: "sms", Provider: "platform", Name: "Platform SMS", ApplicationID: &w.app.ID, EnvironmentID: &w.dev.ID})
	require.NoError(t, err)
	assert.Equal(t, "platform", pl.Mode)
	assert.False(t, pl.HasSecret)

	hook, _, err := e.svc.Create(ctx, w.a, credential.CreateInput{Channel: "slack", Provider: "incoming_webhook", Name: "alerts",
		Config: map[string]any{"channel_label": "#alerts"}, Secret: map[string]any{"webhook_url": slackHook}})
	require.NoError(t, err)
	assert.Equal(t, "XXXX", hook.SecretHint)
	got, err := e.svc.Secret(ctx, w.a.TenantID, hook.ID)
	require.NoError(t, err)
	assert.Equal(t, slackHook, got["webhook_url"])
}

func TestCatalogValidation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	w := e.world(t)
	cases := []struct {
		name  string
		in    credential.CreateInput
		field string
	}{
		{"unknown provider", credential.CreateInput{Channel: "email", Provider: "mailgun", Name: "x"}, "provider"},
		{"phase 4 channel", credential.CreateInput{Channel: "push", Provider: "fcm", Name: "x"}, "provider"},
		{"missing secret", credential.CreateInput{Channel: "email", Provider: "sendgrid", Name: "x", Config: map[string]any{"from_email": "a@b.test"}}, "secret"},
		{"bad sendgrid key", credential.CreateInput{Channel: "email", Provider: "sendgrid", Name: "x", Config: map[string]any{"from_email": "a@b.test"}, Secret: map[string]any{"api_key": "nope"}}, "secret.api_key"},
		{"unknown secret field", credential.CreateInput{Channel: "email", Provider: "sendgrid", Name: "x", Config: map[string]any{"from_email": "a@b.test"}, Secret: map[string]any{"api_key": sgKey, "extra": "x"}}, "secret.extra"},
		{"secret in config", credential.CreateInput{Channel: "email", Provider: "sendgrid", Name: "x", Config: map[string]any{"from_email": "a@b.test", "api_key": sgKey}, Secret: map[string]any{"api_key": sgKey}}, "config.api_key"},
		{"from name header injection", credential.CreateInput{Channel: "email", Provider: "sendgrid", Name: "x", Config: map[string]any{"from_email": "a@b.test", "from_name": "A\r\nBcc: v@x.test"}, Secret: map[string]any{"api_key": sgKey}}, "config.from_name"},
		{"twilio both senders", credential.CreateInput{Channel: "sms", Provider: "twilio", Name: "x", Config: map[string]any{"account_sid": twSID, "from": "+14155550100", "messaging_service_sid": "MG" + strings.Repeat("ef", 16)}, Secret: map[string]any{"auth_token": twToken}}, "config"},
		{"twilio no sender", credential.CreateInput{Channel: "sms", Provider: "twilio", Name: "x", Config: map[string]any{"account_sid": twSID}, Secret: map[string]any{"auth_token": twToken}}, "config"},
		{"slack webhook elsewhere", credential.CreateInput{Channel: "slack", Provider: "incoming_webhook", Name: "x", Secret: map[string]any{"webhook_url": "https://evil.test/services/x"}}, "secret.webhook_url"},
		{"slack webhook http", credential.CreateInput{Channel: "slack", Provider: "incoming_webhook", Name: "x", Secret: map[string]any{"webhook_url": "http://hooks.slack.com/services/x"}}, "secret.webhook_url"},
		{"smtp bad port", credential.CreateInput{Channel: "email", Provider: "smtp", Name: "x", Config: map[string]any{"host": "smtp.acme.test", "port": 22, "tls": "starttls", "from_email": "a@b.test"}, Secret: map[string]any{"password": "p"}}, "config.port"},
		{"ses bad region", credential.CreateInput{Channel: "email", Provider: "ses", Name: "x", Config: map[string]any{"role_arn": "arn:aws:iam::123456789012:role/r", "region": "mars-1", "from_email": "a@b.test"}}, "config.region"},
		{"ses bad arn", credential.CreateInput{Channel: "email", Provider: "ses", Name: "x", Config: map[string]any{"role_arn": "arn:aws:iam::1:user/u", "region": "ap-south-1", "from_email": "a@b.test"}}, "config.role_arn"},
		{"env without app", credential.CreateInput{Channel: "sms", Provider: "platform", Name: "x", EnvironmentID: &w.dev.ID}, "environment_id"},
		{"empty name", credential.CreateInput{Channel: "sms", Provider: "platform", Name: " "}, "name"},
	}
	for _, tc := range cases {
		_, _, err := e.svc.Create(ctx, w.a, tc.in)
		require.ErrorIs(t, err, store.ErrInvalid, tc.name)
		assert.Contains(t, err.Error(), tc.field, tc.name)
		assert.NotContains(t, err.Error(), sgKey, "%s: errors never echo secrets", tc.name)
	}
}

func TestSMTPHostRules(t *testing.T) {
	t.Parallel()
	bad := []string{"localhost", "127.0.0.1", "10.0.0.5", "172.16.3.4", "192.168.1.1", "169.254.169.254", "100.64.0.1",
		"0.0.0.0", "[::1]", "::1", "fd00::1", "fe80::1", "::ffff:10.0.0.1", "64:ff9b::a00:1", "smtp", "mail.local", "db.internal",
		"ip-10-0-0-1.ec2.internal", "2130706433", "0x7f.1", "a..b", "-bad.example.com", "printer.lan"}
	for _, h := range bad {
		assert.Error(t, credential.SMTPHost(h), h)
	}
	for _, h := range []string{"smtp.sendgrid.net", "email-smtp.ap-south-1.amazonaws.com", "8.8.8.8", "smtp.acme.co.in", "2001:4860:4860::8888"} {
		assert.NoError(t, credential.SMTPHost(h), h)
	}
	assert.False(t, credential.PublicAddr(netip.MustParseAddr("198.18.0.1")))
	assert.True(t, credential.PublicAddr(netip.MustParseAddr("1.1.1.1")))
}

func TestResolution(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	w := e.world(t)
	tid := w.a.TenantID
	resolve := func(env store.Environment, ch string) (credential.Ref, error) {
		return e.svc.Resolve(ctx, tid, w.app.ID, env.ID, ch)
	}

	// Nothing configured: platform for email/SMS, unavailable otherwise.
	r, err := resolve(w.prd, "email")
	require.NoError(t, err)
	assert.Equal(t, credential.Ref{Channel: "email", Mode: "platform", Provider: "platform", Scope: "platform_default"}, r)
	_, err = resolve(w.prd, "slack")
	require.ErrorIs(t, err, credential.ErrUnavailable)
	_, err = resolve(w.prd, "webhook")
	require.ErrorIs(t, err, credential.ErrUnavailable)

	tenantSG, _, err := e.svc.Create(ctx, w.a, sendgrid(nil, nil))
	require.NoError(t, err)
	r, err = resolve(w.prd, "email")
	require.NoError(t, err)
	assert.Equal(t, "tenant", r.Scope)
	assert.Equal(t, tenantSG.ID, *r.CredentialID)

	appSG, _, err := e.svc.Create(ctx, w.a, sendgrid(&w.app.ID, nil))
	require.NoError(t, err)
	r, _ = resolve(w.prd, "email")
	assert.Equal(t, appSG.ID, *r.CredentialID, "application beats tenant")

	// An explicit platform row pins dev to the platform sender.
	devPlat, _, err := e.svc.Create(ctx, w.a, credential.CreateInput{Channel: "email", Provider: "platform", Name: "dev", ApplicationID: &w.app.ID, EnvironmentID: &w.dev.ID})
	require.NoError(t, err)
	r, _ = resolve(w.dev, "email")
	assert.Equal(t, "environment", r.Scope)
	assert.Equal(t, "platform", r.Mode)
	assert.Equal(t, devPlat.ID, *r.CredentialID)
	r, _ = resolve(w.prd, "email")
	assert.Equal(t, appSG.ID, *r.CredentialID, "prod is unaffected")

	// Disabled rows are skipped.
	off := false
	_, err = e.svc.Update(ctx, w.a, appSG.ID, credential.Patch{Enabled: &off})
	require.NoError(t, err)
	r, _ = resolve(w.prd, "email")
	assert.Equal(t, tenantSG.ID, *r.CredentialID)
	on := true
	_, err = e.svc.Update(ctx, w.a, appSG.ID, credential.Patch{Enabled: &on})
	require.NoError(t, err)

	// Another application's credentials never apply.
	app2, err := e.ten.CreateApplication(ctx, w.a, store.NewApplication{Name: "Two", Key: "two"})
	require.NoError(t, err)
	env2, err := e.ten.CreateEnvironment(ctx, w.a, app2.ID, store.EnvProd)
	require.NoError(t, err)
	r, err = e.svc.Resolve(ctx, tid, app2.ID, env2.ID, "email")
	require.NoError(t, err)
	assert.Equal(t, tenantSG.ID, *r.CredentialID)

	// Suspended tenants lose the platform fallback, never BYO.
	slackCred, _, err := e.svc.Create(ctx, w.a, credential.CreateInput{Channel: "slack", Provider: "bot_token", Name: "bot", Secret: map[string]any{"bot_token": slackToken}})
	require.NoError(t, err)
	susp := store.TenantSuspended
	_, err = e.st.Tenants().Update(ctx, tid, store.TenantPatch{Status: &susp})
	require.NoError(t, err)
	_, err = resolve(w.prd, "sms")
	require.ErrorIs(t, err, credential.ErrUnavailable)
	r, err = resolve(w.prd, "slack")
	require.NoError(t, err)
	assert.Equal(t, slackCred.ID, *r.CredentialID)

	// The console variant checks the environment belongs to the application.
	_, err = e.svc.ResolveFor(ctx, w.a, app2.ID, w.prd.ID, "email")
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestCredentialPermissionsAndScope(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := context.Background()
	w := e.world(t)
	c, _, err := e.svc.Create(ctx, w.a, sendgrid(nil, nil))
	require.NoError(t, err)

	viewer := e.member(t, w, store.RoleViewer)
	_, err = e.svc.List(ctx, viewer, nil, nil, store.Page{})
	require.ErrorIs(t, err, tenant.ErrForbidden, "viewers see nothing")
	_, err = e.svc.Get(ctx, viewer, c.ID)
	require.ErrorIs(t, err, tenant.ErrForbidden)

	dev := e.member(t, w, store.RoleDeveloper)
	list, err := e.svc.List(ctx, dev, nil, nil, store.Page{})
	require.NoError(t, err, "developers see metadata")
	require.Len(t, list, 1)
	_, _, err = e.svc.Create(ctx, dev, sendgrid(&w.app.ID, nil))
	require.ErrorIs(t, err, tenant.ErrForbidden)
	_, _, err = e.svc.Rotate(ctx, dev, c.ID, map[string]any{"api_key": sgKey})
	require.ErrorIs(t, err, tenant.ErrForbidden)
	require.ErrorIs(t, e.svc.Delete(ctx, dev, c.ID), tenant.ErrForbidden)

	admin := e.member(t, w, store.RoleAdmin)
	_, err = e.svc.Update(ctx, admin, c.ID, credential.Patch{Name: ptr("Renamed")})
	require.NoError(t, err)

	// Another tenant can't see or touch it, or scope into this tenant's app.
	b := e.world(t)
	_, err = e.svc.Get(ctx, b.a, c.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
	_, _, err = e.svc.Rotate(ctx, b.a, c.ID, map[string]any{"api_key": sgKey})
	require.ErrorIs(t, err, store.ErrNotFound)
	_, _, err = e.svc.Create(ctx, b.a, sendgrid(&w.app.ID, nil))
	require.ErrorIs(t, err, store.ErrNotFound)
	_, _, err = e.svc.Create(ctx, b.a, sendgrid(&b.app.ID, &w.prd.ID))
	require.ErrorIs(t, err, store.ErrNotFound, "B's app with A's environment")
	_, err = e.svc.Secret(ctx, b.a.TenantID, c.ID)
	require.ErrorIs(t, err, store.ErrNotFound)

	require.NoError(t, e.svc.Delete(ctx, w.a, c.ID))
	require.ErrorIs(t, e.svc.Delete(ctx, w.a, c.ID), store.ErrNotFound)
}

func ptr[T any](v T) *T { return &v }
