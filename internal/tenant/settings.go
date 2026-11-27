package tenant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"time"
	_ "time/tzdata" // IANA zones in distroless images (quiet hours)
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/ticatwolves/notifex/internal/audit"
	"github.com/ticatwolves/notifex/internal/observability"
	"github.com/ticatwolves/notifex/internal/rbac"
	"github.com/ticatwolves/notifex/internal/store"
)

// Settings defaults (docs/03).
const (
	DefaultLocale        = "en"
	DefaultMaxAttempts   = 5
	DefaultRetentionDays = 7 // ADR-006: also the maximum
)

// Channels are every channel name a policy may mention (docs/03).
var Channels = map[string]bool{"email": true, "sms": true, "push": true, "slack": true, "outlook": true, "whatsapp": true, "webhook": true}

// FieldError is a validation failure on one field. It matches
// store.ErrInvalid, and transports show its message.
type FieldError struct {
	Field string
	Msg   string
}

func (e *FieldError) Error() string { return e.Field + ": " + e.Msg }

// Is matches store.ErrInvalid.
func (e *FieldError) Is(target error) bool { return target == store.ErrInvalid }

func fieldErr(field, format string, args ...any) error {
	return &FieldError{Field: field, Msg: fmt.Sprintf(format, args...)}
}

// ---- brand

// BrandColors are hex colours (#rrggbb).
type BrandColors struct {
	Primary    *string `json:"primary,omitempty"`
	Secondary  *string `json:"secondary,omitempty"`
	Background *string `json:"background,omitempty"`
	Text       *string `json:"text,omitempty"`
}

// BrandEmail is the email sender identity (the address comes from the credential).
type BrandEmail struct {
	FromName *string `json:"from_name,omitempty"`
	ReplyTo  *string `json:"reply_to,omitempty"`
}

// Brand is a brand profile or an override (nil fields inherit).
type Brand struct {
	DisplayName   *string      `json:"display_name,omitempty"`
	LogoURL       *string      `json:"logo_url,omitempty"`
	Colors        *BrandColors `json:"colors,omitempty"`
	Email         *BrandEmail  `json:"email,omitempty"`
	FooterText    *string      `json:"footer_text,omitempty"`
	LegalText     *string      `json:"legal_text,omitempty"`
	SupportURL    *string      `json:"support_url,omitempty"`
	DefaultLocale *string      `json:"default_locale,omitempty"`
}

// BrandView is a stored or effective brand. UpdatedAt is nil when never saved.
type BrandView struct {
	Brand
	UpdatedAt *time.Time
}

var (
	hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
	localeRE = regexp.MustCompile(`^[a-z]{2,3}(-[A-Z]{2}|-[A-Za-z]{4}|-[0-9]{3})?$`)
	clockRE  = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
)

func textField(field string, v *string, maxRunes int, multiline bool) error {
	if v == nil {
		return nil
	}
	if utf8.RuneCountInString(*v) > maxRunes {
		return fieldErr(field, "at most %d characters", maxRunes)
	}
	for _, r := range *v {
		allowed := multiline && (r == '\n' || r == '\t')
		if (r < 0x20 || r == 0x7f) && !allowed {
			return fieldErr(field, "control characters are not allowed")
		}
	}
	return nil
}

func httpsField(field string, v *string) error {
	if v == nil {
		return nil
	}
	u, err := url.Parse(*v)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || len(*v) > 2048 {
		return fieldErr(field, "must be an https URL without credentials")
	}
	return nil
}

// Validate checks a brand profile or override.
func (b Brand) Validate() error {
	checks := []error{
		textField("display_name", b.DisplayName, 100, false),
		httpsField("logo_url", b.LogoURL),
		textField("footer_text", b.FooterText, 2000, true),
		textField("legal_text", b.LegalText, 4000, true),
		httpsField("support_url", b.SupportURL),
	}
	if b.Colors != nil {
		for name, c := range map[string]*string{"primary": b.Colors.Primary, "secondary": b.Colors.Secondary, "background": b.Colors.Background, "text": b.Colors.Text} {
			if c != nil && !hexColor.MatchString(*c) {
				checks = append(checks, fieldErr("colors."+name, "must be #rrggbb"))
			}
		}
	}
	if b.Email != nil {
		checks = append(checks, textField("email.from_name", b.Email.FromName, 100, false))
		if r := b.Email.ReplyTo; r != nil {
			if a, err := mail.ParseAddress(*r); err != nil || a.Address != *r || len(*r) > 254 {
				checks = append(checks, fieldErr("email.reply_to", "must be a plain email address"))
			}
		}
	}
	if l := b.DefaultLocale; l != nil && !localeRE.MatchString(*l) {
		checks = append(checks, fieldErr("default_locale", "must be a BCP 47 tag such as en, en-IN or hi"))
	}
	return errors.Join(checks...)
}

// merge returns base with every field set in o applied (field by field,
// including nested colours and email).
func (b Brand) merge(o Brand) Brand {
	pick := func(x, y *string) *string {
		if y != nil {
			return y
		}
		return x
	}
	out := Brand{
		DisplayName: pick(b.DisplayName, o.DisplayName), LogoURL: pick(b.LogoURL, o.LogoURL),
		FooterText: pick(b.FooterText, o.FooterText), LegalText: pick(b.LegalText, o.LegalText),
		SupportURL: pick(b.SupportURL, o.SupportURL), DefaultLocale: pick(b.DefaultLocale, o.DefaultLocale),
	}
	if b.Colors != nil || o.Colors != nil {
		x, y := deref(b.Colors), deref(o.Colors)
		out.Colors = &BrandColors{Primary: pick(x.Primary, y.Primary), Secondary: pick(x.Secondary, y.Secondary),
			Background: pick(x.Background, y.Background), Text: pick(x.Text, y.Text)}
	}
	if b.Email != nil || o.Email != nil {
		x, y := deref(b.Email), deref(o.Email)
		out.Email = &BrandEmail{FromName: pick(x.FromName, y.FromName), ReplyTo: pick(x.ReplyTo, y.ReplyTo)}
	}
	return out
}

func deref[T any](p *T) T {
	if p == nil {
		var z T
		return z
	}
	return *p
}

func withBrandDefaults(b Brand) Brand {
	if b.DefaultLocale == nil {
		l := DefaultLocale
		b.DefaultLocale = &l
	}
	return b
}

// ---- policy

// QuietHours defers sends inside [Start, End) local time (Phase 3).
type QuietHours struct {
	Start                string   `json:"start"`
	End                  string   `json:"end"`
	Timezone             string   `json:"timezone"`
	UseRecipientTimezone bool     `json:"use_recipient_timezone,omitempty"`
	Channels             []string `json:"channels,omitempty"`
}

// SendingPolicy is a sending policy or an override (nil fields inherit).
// PayloadRetentionDays is tenant-level only.
type SendingPolicy struct {
	AllowedChannels      []string       `json:"allowed_channels,omitempty"`
	QuietHours           *QuietHours    `json:"quiet_hours,omitempty"`
	RateLimitsPerMinute  map[string]int `json:"rate_limits_per_minute,omitempty"`
	MaxAttempts          *int           `json:"max_attempts,omitempty"`
	PayloadRetentionDays *int           `json:"payload_retention_days,omitempty"`
}

// SendingPolicyView is a stored or effective policy. UpdatedAt is nil when never saved.
type SendingPolicyView struct {
	SendingPolicy
	UpdatedAt *time.Time
}

func channelList(field string, cs []string) error {
	seen := map[string]bool{}
	for _, c := range cs {
		if !Channels[c] {
			return fieldErr(field, "unknown channel %q", truncateStr(c, 20))
		}
		if seen[c] {
			return fieldErr(field, "duplicate channel %q", c)
		}
		seen[c] = true
	}
	return nil
}

// Validate checks a policy; override reports whether it is an application
// override (which can't set retention).
func (p SendingPolicy) Validate(override bool) error {
	checks := []error{channelList("allowed_channels", p.AllowedChannels)}
	if q := p.QuietHours; q != nil {
		switch {
		case !clockRE.MatchString(q.Start):
			checks = append(checks, fieldErr("quiet_hours.start", "must be HH:MM (24 h)"))
		case !clockRE.MatchString(q.End):
			checks = append(checks, fieldErr("quiet_hours.end", "must be HH:MM (24 h)"))
		case q.Start == q.End:
			checks = append(checks, fieldErr("quiet_hours", "start and end must differ"))
		}
		if q.Timezone == "" || q.Timezone == "Local" || len(q.Timezone) > 64 {
			checks = append(checks, fieldErr("quiet_hours.timezone", "must be an IANA time zone such as Asia/Kolkata"))
		} else if _, err := time.LoadLocation(q.Timezone); err != nil {
			checks = append(checks, fieldErr("quiet_hours.timezone", "unknown time zone"))
		}
		checks = append(checks, channelList("quiet_hours.channels", q.Channels))
	}
	for c, n := range p.RateLimitsPerMinute {
		if !Channels[c] {
			checks = append(checks, fieldErr("rate_limits_per_minute", "unknown channel %q", truncateStr(c, 20)))
		} else if n < 1 || n > 100_000 {
			checks = append(checks, fieldErr("rate_limits_per_minute."+c, "must be 1-100000"))
		}
	}
	if m := p.MaxAttempts; m != nil && (*m < 1 || *m > 10) {
		checks = append(checks, fieldErr("max_attempts", "must be 1-10"))
	}
	if r := p.PayloadRetentionDays; r != nil {
		if override {
			checks = append(checks, fieldErr("payload_retention_days", "is set on the tenant policy only"))
		} else if *r < 1 || *r > DefaultRetentionDays {
			checks = append(checks, fieldErr("payload_retention_days", "must be 1-7"))
		}
	}
	return errors.Join(checks...)
}

func (p SendingPolicy) merge(o SendingPolicy) SendingPolicy {
	out := p
	if o.AllowedChannels != nil {
		out.AllowedChannels = o.AllowedChannels
	}
	if o.QuietHours != nil {
		out.QuietHours = o.QuietHours
	}
	if o.RateLimitsPerMinute != nil {
		m := make(map[string]int, len(p.RateLimitsPerMinute)+len(o.RateLimitsPerMinute))
		for k, v := range p.RateLimitsPerMinute {
			m[k] = v
		}
		for k, v := range o.RateLimitsPerMinute {
			m[k] = v
		}
		out.RateLimitsPerMinute = m
	}
	if o.MaxAttempts != nil {
		out.MaxAttempts = o.MaxAttempts
	}
	return out
}

func withPolicyDefaults(p SendingPolicy) SendingPolicy {
	if p.AllowedChannels == nil {
		p.AllowedChannels = []string{}
	}
	if p.RateLimitsPerMinute == nil {
		p.RateLimitsPerMinute = map[string]int{}
	}
	if p.MaxAttempts == nil {
		n := DefaultMaxAttempts
		p.MaxAttempts = &n
	}
	if p.PayloadRetentionDays == nil {
		n := DefaultRetentionDays
		p.PayloadRetentionDays = &n
	}
	return p
}

// ---- documents

func toDoc(v any) (map[string]any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode settings: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("encode settings: %w", err)
	}
	return m, nil
}

func fromDoc(doc map[string]any, v any) error {
	raw, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("decode settings: %w", err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("decode settings: %w", err)
	}
	return nil
}

// loadBrand returns the stored brand at a scope (zero Brand, nil time if unset).
func loadBrand(ctx context.Context, st store.Store, tid store.TenantID, appID *uuid.UUID) (Brand, *time.Time, error) {
	row, err := st.Brands().Get(ctx, tid, appID)
	if errors.Is(err, store.ErrNotFound) {
		return Brand{}, nil, nil
	}
	if err != nil {
		return Brand{}, nil, err
	}
	var b Brand
	if err := fromDoc(row.Doc, &b); err != nil {
		return Brand{}, nil, err
	}
	return b, &row.UpdatedAt, nil
}

func loadPolicy(ctx context.Context, st store.Store, tid store.TenantID, appID *uuid.UUID) (SendingPolicy, *time.Time, error) {
	row, err := st.Policies().Get(ctx, tid, appID)
	if errors.Is(err, store.ErrNotFound) {
		return SendingPolicy{}, nil, nil
	}
	if err != nil {
		return SendingPolicy{}, nil, err
	}
	var p SendingPolicy
	if err := fromDoc(row.Doc, &p); err != nil {
		return SendingPolicy{}, nil, err
	}
	p.PayloadRetentionDays = row.PayloadRetentionDays
	return p, &row.UpdatedAt, nil
}

func later(a, b *time.Time) *time.Time {
	switch {
	case a == nil:
		return b
	case b == nil || a.After(*b):
		return a
	default:
		return b
	}
}

// EffectiveBrand is the brand used for an application's sends: the tenant
// brand with the application's override applied, plus defaults. System
// callers (preview, trigger snapshot) use it; there is no role check.
func (s *Service) EffectiveBrand(ctx context.Context, tid store.TenantID, appID uuid.UUID) (BrandView, error) {
	base, t1, err := loadBrand(ctx, s.st, tid, nil)
	if err != nil {
		return BrandView{}, fmt.Errorf("effective brand: %w", err)
	}
	ov, t2, err := loadBrand(ctx, s.st, tid, &appID)
	if err != nil {
		return BrandView{}, fmt.Errorf("effective brand: %w", err)
	}
	return BrandView{Brand: withBrandDefaults(base.merge(ov)), UpdatedAt: later(t1, t2)}, nil
}

// EffectivePolicy is the policy used for an application's sends.
func (s *Service) EffectivePolicy(ctx context.Context, tid store.TenantID, appID uuid.UUID) (SendingPolicyView, error) {
	base, t1, err := loadPolicy(ctx, s.st, tid, nil)
	if err != nil {
		return SendingPolicyView{}, fmt.Errorf("effective policy: %w", err)
	}
	ov, t2, err := loadPolicy(ctx, s.st, tid, &appID)
	if err != nil {
		return SendingPolicyView{}, fmt.Errorf("effective policy: %w", err)
	}
	return SendingPolicyView{SendingPolicy: withPolicyDefaults(base.merge(ov)), UpdatedAt: later(t1, t2)}, nil
}

// ---- service API

// GetBrand returns the tenant brand (defaults if never saved).
func (s *Service) GetBrand(ctx context.Context, a Actor) (BrandView, error) {
	if err := s.require(a, rbac.ViewTenant); err != nil {
		return BrandView{}, err
	}
	b, at, err := loadBrand(ctx, s.st, a.TenantID, nil)
	if err != nil {
		return BrandView{}, wrap("get brand", err)
	}
	return BrandView{Brand: withBrandDefaults(b), UpdatedAt: at}, nil
}

// PutBrand replaces the tenant brand.
func (s *Service) PutBrand(ctx context.Context, a Actor, b Brand) (BrandView, error) {
	if err := s.require(a, rbac.ManageSettings); err != nil {
		return BrandView{}, err
	}
	if err := b.Validate(); err != nil {
		return BrandView{}, err
	}
	if err := s.putSettings(ctx, a, nil, b, nil, "brand.update", maskBrand); err != nil {
		return BrandView{}, wrap("put brand", err)
	}
	return s.GetBrand(ctx, a)
}

// AppBrand returns an application's override (nil if none) and its
// effective brand.
func (s *Service) AppBrand(ctx context.Context, a Actor, appID uuid.UUID) (*Brand, BrandView, error) {
	if err := s.require(a, rbac.ViewTenant); err != nil {
		return nil, BrandView{}, err
	}
	if _, err := s.st.Applications().Get(ctx, a.TenantID, appID); err != nil {
		return nil, BrandView{}, wrap("get application brand", err)
	}
	ov, at, err := loadBrand(ctx, s.st, a.TenantID, &appID)
	if err != nil {
		return nil, BrandView{}, wrap("get application brand", err)
	}
	eff, err := s.EffectiveBrand(ctx, a.TenantID, appID)
	if err != nil {
		return nil, BrandView{}, err
	}
	if at == nil {
		return nil, eff, nil
	}
	return &ov, eff, nil
}

// PutAppBrand replaces an application's brand override.
func (s *Service) PutAppBrand(ctx context.Context, a Actor, appID uuid.UUID, b Brand) (*Brand, BrandView, error) {
	if err := s.require(a, rbac.ManageSettings); err != nil {
		return nil, BrandView{}, err
	}
	if err := b.Validate(); err != nil {
		return nil, BrandView{}, err
	}
	if err := s.putSettings(ctx, a, &appID, b, nil, "brand.override.update", maskBrand); err != nil {
		return nil, BrandView{}, wrap("put application brand", err)
	}
	return s.AppBrand(ctx, a, appID)
}

// DeleteAppBrand removes an application's brand override.
func (s *Service) DeleteAppBrand(ctx context.Context, a Actor, appID uuid.UUID) error {
	return wrap("delete application brand", s.deleteOverride(ctx, a, appID, func(tx store.Store) store.SettingsStore { return tx.Brands() }, "brand.override.delete"))
}

// GetPolicy returns the tenant policy (defaults if never saved).
func (s *Service) GetPolicy(ctx context.Context, a Actor) (SendingPolicyView, error) {
	if err := s.require(a, rbac.ViewTenant); err != nil {
		return SendingPolicyView{}, err
	}
	p, at, err := loadPolicy(ctx, s.st, a.TenantID, nil)
	if err != nil {
		return SendingPolicyView{}, wrap("get policy", err)
	}
	return SendingPolicyView{SendingPolicy: withPolicyDefaults(p), UpdatedAt: at}, nil
}

// PutPolicy replaces the tenant policy. A retention change applies to
// notifications that reach a terminal state afterwards (ADR-006).
func (s *Service) PutPolicy(ctx context.Context, a Actor, p SendingPolicy) (SendingPolicyView, error) {
	if err := s.require(a, rbac.ManageSettings); err != nil {
		return SendingPolicyView{}, err
	}
	if err := p.Validate(false); err != nil {
		return SendingPolicyView{}, err
	}
	ret := p.PayloadRetentionDays
	p.PayloadRetentionDays = nil // stored in its own column
	if err := s.putSettings(ctx, a, nil, p, ret, "policy.update", nil); err != nil {
		return SendingPolicyView{}, wrap("put policy", err)
	}
	return s.GetPolicy(ctx, a)
}

// AppPolicy returns an application's override (nil if none) and its
// effective policy.
func (s *Service) AppPolicy(ctx context.Context, a Actor, appID uuid.UUID) (*SendingPolicy, SendingPolicyView, error) {
	if err := s.require(a, rbac.ViewTenant); err != nil {
		return nil, SendingPolicyView{}, err
	}
	if _, err := s.st.Applications().Get(ctx, a.TenantID, appID); err != nil {
		return nil, SendingPolicyView{}, wrap("get application policy", err)
	}
	ov, at, err := loadPolicy(ctx, s.st, a.TenantID, &appID)
	if err != nil {
		return nil, SendingPolicyView{}, wrap("get application policy", err)
	}
	eff, err := s.EffectivePolicy(ctx, a.TenantID, appID)
	if err != nil {
		return nil, SendingPolicyView{}, err
	}
	if at == nil {
		return nil, eff, nil
	}
	return &ov, eff, nil
}

// PutAppPolicy replaces an application's policy override.
func (s *Service) PutAppPolicy(ctx context.Context, a Actor, appID uuid.UUID, p SendingPolicy) (*SendingPolicy, SendingPolicyView, error) {
	if err := s.require(a, rbac.ManageSettings); err != nil {
		return nil, SendingPolicyView{}, err
	}
	if err := p.Validate(true); err != nil {
		return nil, SendingPolicyView{}, err
	}
	if err := s.putSettings(ctx, a, &appID, p, nil, "policy.override.update", nil); err != nil {
		return nil, SendingPolicyView{}, wrap("put application policy", err)
	}
	return s.AppPolicy(ctx, a, appID)
}

// DeleteAppPolicy removes an application's policy override.
func (s *Service) DeleteAppPolicy(ctx context.Context, a Actor, appID uuid.UUID) error {
	return wrap("delete application policy", s.deleteOverride(ctx, a, appID, func(tx store.Store) store.SettingsStore { return tx.Policies() }, "policy.override.delete"))
}

// putSettings writes a document and its audit event (before and after) in
// one transaction. mask redacts personal data in the audit copy.
func (s *Service) putSettings(ctx context.Context, a Actor, appID *uuid.UUID, v any, ret *int, action string, mask func(map[string]any)) error {
	doc, err := toDoc(v)
	if err != nil {
		return err
	}
	kind := strings.SplitN(action, ".", 2)[0] // "brand" or "policy"
	return s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		ss := tx.Brands()
		if kind == "policy" {
			ss = tx.Policies()
		}
		var before map[string]any
		if old, err := ss.Get(ctx, a.TenantID, appID); err == nil {
			before = auditDoc(old.Doc, old.PayloadRetentionDays, mask)
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		saved, err := ss.Put(ctx, a.TenantID, appID, doc, ret)
		if err != nil {
			return err
		}
		entity := a.TenantID.String()
		if appID != nil {
			entity = appID.String()
		}
		etype := "tenant"
		if appID != nil {
			etype = "application"
		}
		return audit.Record(ctx, tx, actorOf(a.User), audit.Event{
			TenantID: &a.TenantID, Action: action, EntityType: etype, EntityID: entity,
			Before: before, After: auditDoc(saved.Doc, saved.PayloadRetentionDays, mask),
		})
	})
}

func (s *Service) deleteOverride(ctx context.Context, a Actor, appID uuid.UUID, pick func(store.Store) store.SettingsStore, action string) error {
	if err := s.require(a, rbac.ManageSettings); err != nil {
		return err
	}
	return s.st.WithTx(ctx, func(ctx context.Context, tx store.Store) error {
		old, err := pick(tx).Get(ctx, a.TenantID, &appID)
		if err != nil {
			return err
		}
		if err := pick(tx).Delete(ctx, a.TenantID, appID); err != nil {
			return err
		}
		mask := maskBrand
		if strings.HasPrefix(action, "policy") {
			mask = nil
		}
		return audit.Record(ctx, tx, actorOf(a.User), audit.Event{
			TenantID: &a.TenantID, Action: action, EntityType: "application", EntityID: appID.String(),
			Before: auditDoc(old.Doc, nil, mask),
		})
	})
}

func auditDoc(doc map[string]any, ret *int, mask func(map[string]any)) map[string]any {
	out := make(map[string]any, len(doc)+1)
	for k, v := range doc {
		out[k] = v
	}
	if ret != nil {
		out["payload_retention_days"] = *ret
	}
	if mask != nil {
		mask(out)
	}
	return out
}

// maskBrand masks the reply-to address in audit copies.
func maskBrand(doc map[string]any) {
	email, ok := doc["email"].(map[string]any)
	if !ok {
		return
	}
	c := make(map[string]any, len(email))
	for k, v := range email {
		c[k] = v
	}
	if r, ok := c["reply_to"].(string); ok {
		c["reply_to"] = observability.MaskEmail(r)
	}
	doc["email"] = c
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
