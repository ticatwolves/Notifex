-- reverse: create index "invitation_token_hash" to table: "invitations"
DROP INDEX "invitation_token_hash";
-- reverse: create index "invitation_tenant_id_email" to table: "invitations"
DROP INDEX "invitation_tenant_id_email";
-- reverse: create index "invitation_tenant_id_created_at" to table: "invitations"
DROP INDEX "invitation_tenant_id_created_at";
-- reverse: create "invitations" table
DROP TABLE "invitations";
