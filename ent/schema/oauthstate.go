package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// OAuthState is a pending OAuth authorization (ADR-011 §3): the SHA-256 of
// the `state` parameter, the PKCE verifier and OIDC nonce, bound to the
// provider and redirect URI. Lives 10 minutes and is consumed once.
// link_user_id is set when a signed-in user is linking a new provider.
type OAuthState struct{ ent.Schema }

// Mixin of the OAuthState.
func (OAuthState) Mixin() []ent.Mixin { return []ent.Mixin{IDMixin{}} }

// Fields of the OAuthState.
func (OAuthState) Fields() []ent.Field {
	return []ent.Field{
		field.Bytes("state_hash").NotEmpty().Sensitive().Immutable(),
		field.Enum("provider").Values("google", "microsoft", "github").Immutable(),
		field.String("redirect_uri").NotEmpty().MaxLen(2048).Immutable(),
		// Short-lived secrets, useless without the authorization code.
		field.String("code_verifier").NotEmpty().Sensitive().Immutable(),
		field.String("nonce").NotEmpty().Sensitive().Immutable(),
		field.UUID("link_user_id", uuid.UUID{}).Optional().Nillable().Immutable(),
		field.Time("created_at").Immutable(),
		field.Time("expires_at").Immutable(),
		field.Time("used_at").Optional().Nillable(),
	}
}

// Indexes of the OAuthState.
func (OAuthState) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("state_hash").Unique(),
		index.Fields("expires_at"), // cleanup job
	}
}
