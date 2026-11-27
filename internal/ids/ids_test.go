package ids

import (
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var idRE = regexp.MustCompile(`^[a-z]{3}_[0-9A-Za-z]{22}$`)

func TestRoundTrip(t *testing.T) {
	cases := []uuid.UUID{uuid.Max, uuid.Must(uuid.NewV7()), uuid.New()}
	for range 200 {
		cases = append(cases, uuid.Must(uuid.NewV7()))
	}
	for _, id := range cases {
		for _, k := range []Kind{Tenant, Application, Environment, APIKey, Run, Notification, User, Membership, Invitation, Session, AuditEvent} {
			s := Format(k, id)
			assert.Regexp(t, idRE, s)
			got, err := Parse(k, s)
			require.NoError(t, err)
			assert.Equal(t, id, got)
		}
	}
}

func TestParseRejects(t *testing.T) {
	good := Format(Run, uuid.Must(uuid.NewV7()))
	tests := map[string]string{
		"wrong kind":     strings.Replace(good, "run_", "app_", 1),
		"no prefix":      good[4:],
		"short":          good[:len(good)-1],
		"long":           good + "0",
		"bad char":       good[:len(good)-1] + "-",
		"overflow 2^128": "run_" + strings.Repeat("z", 22),
		"raw uuid":       "run_" + uuid.NewString(),
		"nil uuid":       Format(Run, uuid.Nil),
		"empty":          "",
	}
	for name, s := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(Run, s)
			require.ErrorIs(t, err, ErrInvalid)
		})
	}
}

func TestOrderPreservedForUUIDv7(t *testing.T) {
	// Fixed width + 0-9A-Za-z digit order means lexical order of the encoded
	// form tracks numeric order, so time-ordered UUIDv7s sort by creation.
	prev := Format(Run, uuid.Must(uuid.NewV7()))
	for range 100 {
		next := Format(Run, uuid.Must(uuid.NewV7()))
		assert.Less(t, prev, next)
		prev = next
	}
}
