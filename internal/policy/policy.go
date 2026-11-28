// Package policy evaluates a notification against its snapshotted sending
// policy at dispatch time (docs/02 §Policy engine): a pure function with no
// I/O. Allowed channels are decided at accept; quiet hours here. Rate
// limits are stateful and live in the dispatcher.
package policy

import (
	"strconv"
	"time"

	"github.com/ticatwolves/notifex/internal/tenant"
)

// Decision is Allow, or Defer until a time.
type Decision struct {
	Defer  bool
	Until  time.Time
	Reason string
}

// Allow is the zero decision.
var Allow = Decision{}

// Evaluate applies quiet hours. recipientTZ is the trigger's recipient
// timezone (used only if the policy says so and it is a valid IANA name).
func Evaluate(p tenant.SendingPolicy, channel, recipientTZ string, now time.Time) Decision {
	q := p.QuietHours
	if q == nil || !applies(q.Channels, channel) {
		return Allow
	}
	tz := q.Timezone
	if q.UseRecipientTimezone && recipientTZ != "" {
		if _, err := time.LoadLocation(recipientTZ); err == nil {
			tz = recipientTZ
		}
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return Allow // validated on save; a bad zone never blocks sends
	}
	start, ok1 := clock(q.Start)
	end, ok2 := clock(q.End)
	if !ok1 || !ok2 || start == end {
		return Allow
	}
	local := now.In(loc)
	m := local.Hour()*60 + local.Minute()
	in := m >= start && m < end
	if start > end { // spans midnight, e.g. 22:00-07:00
		in = m >= start || m < end
	}
	if !in {
		return Allow
	}
	until := time.Date(local.Year(), local.Month(), local.Day(), end/60, end%60, 0, 0, loc)
	if !until.After(local) {
		until = time.Date(local.Year(), local.Month(), local.Day()+1, end/60, end%60, 0, 0, loc)
	}
	return Decision{Defer: true, Until: until.UTC(), Reason: "quiet_hours"}
}

func applies(channels []string, ch string) bool {
	if len(channels) == 0 {
		return true
	}
	for _, c := range channels {
		if c == ch {
			return true
		}
	}
	return false
}

// clock parses HH:MM into minutes after midnight.
func clock(s string) (int, bool) {
	if len(s) != 5 || s[2] != ':' {
		return 0, false
	}
	h, err1 := strconv.Atoi(s[:2])
	m, err2 := strconv.Atoi(s[3:])
	if err1 != nil || err2 != nil || h > 23 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}
