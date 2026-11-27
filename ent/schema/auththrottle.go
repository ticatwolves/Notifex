package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
)

// AuthThrottle tracks failed authentication attempts per key for progressive
// backoff (ADR-011 §2). Keys are opaque (e.g. "login:" + SHA-256 of the
// normalised email), so this table holds no PII and covers unknown emails
// too, which keeps backoff from revealing which accounts exist.
type AuthThrottle struct{ ent.Schema }

// Fields of the AuthThrottle.
func (AuthThrottle) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").StorageKey("key").MaxLen(128).NotEmpty().Immutable(),
		field.Int("failures").NonNegative().Default(0),
		field.Time("last_failure_at"),
		field.Time("blocked_until"),
	}
}
