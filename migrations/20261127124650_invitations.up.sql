-- create "invitations" table
CREATE TABLE "invitations" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "email" character varying NOT NULL, "role" character varying NOT NULL, "token_hash" bytea NOT NULL, "invited_by" uuid NULL, "created_at" timestamptz NOT NULL, "expires_at" timestamptz NOT NULL, "accepted_at" timestamptz NULL, "accepted_by" uuid NULL, "revoked_at" timestamptz NULL, PRIMARY KEY ("id"), CONSTRAINT "invitations_tenant_fk" FOREIGN KEY ("tenant_id") REFERENCES "tenants" ("id") ON UPDATE NO ACTION ON DELETE RESTRICT, CONSTRAINT "invitations_invited_by_fk" FOREIGN KEY ("invited_by") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE SET NULL, CONSTRAINT "invitations_accepted_by_fk" FOREIGN KEY ("accepted_by") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE SET NULL);
-- create index "invitation_tenant_id_created_at" to table: "invitations"
CREATE INDEX "invitation_tenant_id_created_at" ON "invitations" ("tenant_id", "created_at");
-- create index "invitation_tenant_id_email" to table: "invitations"
CREATE UNIQUE INDEX "invitation_tenant_id_email" ON "invitations" ("tenant_id", "email") WHERE ((accepted_at IS NULL) AND (revoked_at IS NULL));
-- create index "invitation_token_hash" to table: "invitations"
CREATE UNIQUE INDEX "invitation_token_hash" ON "invitations" ("token_hash");
