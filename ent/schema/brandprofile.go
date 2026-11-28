package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// BrandProfile is a tenant's brand, or an application's override of it
// (application_id set). The fields live in doc (JSON) because an override
// stores only the fields it sets; absent fields inherit from the tenant.
type BrandProfile struct{ ent.Schema }

// Mixin of the BrandProfile.
func (BrandProfile) Mixin() []ent.Mixin {
	return []ent.Mixin{IDMixin{}, TenantMixin{}, TimeMixin{}}
}

// Fields of the BrandProfile.
func (BrandProfile) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("application_id", uuid.UUID{}).Optional().Nillable().Immutable(),
		field.JSON("doc", map[string]any{}),
	}
}

// Indexes of the BrandProfile: one tenant profile and at most one override
// per application.
func (BrandProfile) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id").Unique().
			Annotations(entsql.IndexWhere("application_id IS NULL")).StorageKey("brand_profiles_tenant_key"),
		index.Fields("tenant_id", "application_id").Unique().
			Annotations(entsql.IndexWhere("application_id IS NOT NULL")).StorageKey("brand_profiles_application_key"),
	}
}

// Annotations of the BrandProfile.
func (BrandProfile) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Checks(map[string]string{"brand_profiles_doc_object": "jsonb_typeof(doc) = 'object'"}),
	}
}
