package policy_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/ticatwolves/notifex/internal/policy"
	"github.com/ticatwolves/notifex/internal/tenant"
)

func at(t *testing.T, tz, s string) time.Time {
	t.Helper()
	loc, err := time.LoadLocation(tz)
	if err != nil {
		t.Fatal(err)
	}
	v, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestQuietHours(t *testing.T) {
	t.Parallel()
	night := &tenant.QuietHours{Start: "22:00", End: "07:00", Timezone: "Asia/Kolkata"}
	day := &tenant.QuietHours{Start: "13:00", End: "14:00", Timezone: "Asia/Kolkata", Channels: []string{"sms"}}
	cases := []struct {
		name     string
		q        *tenant.QuietHours
		ch       string
		rtz      string
		now      time.Time
		deferred bool
		until    time.Time
	}{
		{"no quiet hours", nil, "sms", "", at(t, "Asia/Kolkata", "2026-11-28 23:00"), false, time.Time{}},
		{"before window", night, "sms", "", at(t, "Asia/Kolkata", "2026-11-28 21:59"), false, time.Time{}},
		{"late evening defers to next morning", night, "sms", "", at(t, "Asia/Kolkata", "2026-11-28 23:30"), true, at(t, "Asia/Kolkata", "2026-11-29 07:00")},
		{"early morning defers to same morning", night, "email", "", at(t, "Asia/Kolkata", "2026-11-29 03:00"), true, at(t, "Asia/Kolkata", "2026-11-29 07:00")},
		{"end is exclusive", night, "sms", "", at(t, "Asia/Kolkata", "2026-11-29 07:00"), false, time.Time{}},
		{"start is inclusive", night, "sms", "", at(t, "Asia/Kolkata", "2026-11-28 22:00"), true, at(t, "Asia/Kolkata", "2026-11-29 07:00")},
		{"daytime window, listed channel", day, "sms", "", at(t, "Asia/Kolkata", "2026-11-28 13:30"), true, at(t, "Asia/Kolkata", "2026-11-28 14:00")},
		{"daytime window, other channel", day, "email", "", at(t, "Asia/Kolkata", "2026-11-28 13:30"), false, time.Time{}},
		{"tenant zone ignores recipient zone", night, "sms", "America/New_York", at(t, "Asia/Kolkata", "2026-11-28 23:30"), true, at(t, "Asia/Kolkata", "2026-11-29 07:00")},
	}
	for _, tc := range cases {
		d := policy.Evaluate(tenant.SendingPolicy{QuietHours: tc.q}, tc.ch, tc.rtz, tc.now)
		assert.Equal(t, tc.deferred, d.Defer, tc.name)
		if tc.deferred {
			assert.True(t, tc.until.Equal(d.Until), "%s: until %s, want %s", tc.name, d.Until, tc.until)
			assert.Equal(t, "quiet_hours", d.Reason)
		}
	}

	// Recipient time zone when the policy asks for it (and it's valid).
	rq := &tenant.QuietHours{Start: "22:00", End: "07:00", Timezone: "Asia/Kolkata", UseRecipientTimezone: true}
	nyLate := at(t, "America/New_York", "2026-11-28 23:00") // 09:30 next day in Kolkata
	d := policy.Evaluate(tenant.SendingPolicy{QuietHours: rq}, "sms", "America/New_York", nyLate)
	assert.True(t, d.Defer)
	assert.True(t, at(t, "America/New_York", "2026-11-29 07:00").Equal(d.Until))
	d = policy.Evaluate(tenant.SendingPolicy{QuietHours: rq}, "sms", "Not/AZone", nyLate)
	assert.False(t, d.Defer, "invalid recipient zone falls back to the policy zone (09:30 Kolkata)")

	// DST: New York springs forward on 2026-03-08; 07:00 local is still 07:00.
	dst := &tenant.QuietHours{Start: "22:00", End: "07:00", Timezone: "America/New_York"}
	d = policy.Evaluate(tenant.SendingPolicy{QuietHours: dst}, "sms", "", at(t, "America/New_York", "2026-03-07 23:00"))
	assert.True(t, d.Defer)
	assert.True(t, at(t, "America/New_York", "2026-03-08 07:00").Equal(d.Until))
	assert.Equal(t, 7*time.Hour, d.Until.Sub(at(t, "America/New_York", "2026-03-07 23:00")), "one hour shorter across the DST jump")
}
