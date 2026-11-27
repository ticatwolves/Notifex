package schema

// ForeignKey declares a foreign key that the migration generator
// (ent/migrate/main.go) adds to the desired schema. Keeping them here, next
// to the schemas, makes every relationship reviewable in one place.
type ForeignKey struct {
	Name       string
	Table      string
	Columns    []string
	RefTable   string
	RefColumns []string
	// OnDelete is RESTRICT unless stated ("CASCADE" or "SET NULL"):
	// deleting a parent with children fails, and the store reports a conflict.
	OnDelete string
}

// tenantOwned lists tables whose tenant_id must reference tenants(id).
var tenantOwned = []string{"memberships", "applications", "environments", "api_keys", "invitations"}

// ForeignKeys returns every foreign key in the core schema.
func ForeignKeys() []ForeignKey {
	fks := make([]ForeignKey, 0, len(tenantOwned)+5)
	for _, t := range tenantOwned {
		fks = append(fks, ForeignKey{
			Name: t + "_tenant_fk", Table: t,
			Columns: []string{"tenant_id"}, RefTable: "tenants", RefColumns: []string{"id"},
		})
	}
	return append(fks,
		ForeignKey{
			Name: "memberships_user_fk", Table: "memberships",
			Columns: []string{"user_id"}, RefTable: "users", RefColumns: []string{"id"},
			OnDelete: "CASCADE", // deleting a user removes their memberships
		},
		// Composite keys: a child can only reference a parent in the same tenant.
		ForeignKey{
			Name: "environments_application_fk", Table: "environments",
			Columns: []string{"tenant_id", "application_id"}, RefTable: "applications", RefColumns: []string{"tenant_id", "id"},
		},
		ForeignKey{
			Name: "api_keys_environment_fk", Table: "api_keys",
			Columns:    []string{"tenant_id", "application_id", "environment_id"},
			RefTable:   "environments",
			RefColumns: []string{"tenant_id", "application_id", "id"},
		},
		ForeignKey{
			Name: "sessions_user_fk", Table: "sessions",
			Columns: []string{"user_id"}, RefTable: "users", RefColumns: []string{"id"},
			OnDelete: "CASCADE",
		},
		ForeignKey{
			Name: "email_tokens_user_fk", Table: "email_tokens",
			Columns: []string{"user_id"}, RefTable: "users", RefColumns: []string{"id"},
			OnDelete: "CASCADE",
		},
		ForeignKey{
			Name: "user_identities_user_fk", Table: "user_identities",
			Columns: []string{"user_id"}, RefTable: "users", RefColumns: []string{"id"},
			OnDelete: "CASCADE",
		},
		ForeignKey{
			Name: "oauth_states_link_user_fk", Table: "oauth_states",
			Columns: []string{"link_user_id"}, RefTable: "users", RefColumns: []string{"id"},
			OnDelete: "CASCADE",
		},
		ForeignKey{
			Name: "invitations_invited_by_fk", Table: "invitations",
			Columns: []string{"invited_by"}, RefTable: "users", RefColumns: []string{"id"},
			OnDelete: "SET NULL", // keep the invitation record if the inviter is deleted
		},
		ForeignKey{
			Name: "invitations_accepted_by_fk", Table: "invitations",
			Columns: []string{"accepted_by"}, RefTable: "users", RefColumns: []string{"id"},
			OnDelete: "SET NULL",
		},
		ForeignKey{
			Name: "audit_events_tenant_fk", Table: "audit_events",
			Columns: []string{"tenant_id"}, RefTable: "tenants", RefColumns: []string{"id"},
		},
	)
}
