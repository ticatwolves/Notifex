-- reverse: create index "membership_user_id" to table: "memberships"
DROP INDEX "membership_user_id";
-- reverse: create index "membership_tenant_id_user_id" to table: "memberships"
DROP INDEX "membership_tenant_id_user_id";
-- reverse: create "memberships" table
DROP TABLE "memberships";
-- reverse: create index "users_email_key" to table: "users"
DROP INDEX "users_email_key";
-- reverse: create "users" table
DROP TABLE "users";
-- reverse: create index "auditevent_tenant_id_created_at" to table: "audit_events"
DROP INDEX "auditevent_tenant_id_created_at";
-- reverse: create index "auditevent_created_at" to table: "audit_events"
DROP INDEX "auditevent_created_at";
-- reverse: create "audit_events" table
DROP TABLE "audit_events";
-- reverse: create index "apikey_tenant_id_application_id" to table: "api_keys"
DROP INDEX "apikey_tenant_id_application_id";
-- reverse: create index "api_keys_prefix_key" to table: "api_keys"
DROP INDEX "api_keys_prefix_key";
-- reverse: create "api_keys" table
DROP TABLE "api_keys";
-- reverse: create index "environment_tenant_id_application_id_name" to table: "environments"
DROP INDEX "environment_tenant_id_application_id_name";
-- reverse: create index "environment_tenant_id_application_id_id" to table: "environments"
DROP INDEX "environment_tenant_id_application_id_id";
-- reverse: create "environments" table
DROP TABLE "environments";
-- reverse: create index "application_tenant_id_key" to table: "applications"
DROP INDEX "application_tenant_id_key";
-- reverse: create index "application_tenant_id_id" to table: "applications"
DROP INDEX "application_tenant_id_id";
-- reverse: create "applications" table
DROP TABLE "applications";
-- reverse: create index "tenants_slug_key" to table: "tenants"
DROP INDEX "tenants_slug_key";
-- reverse: create "tenants" table
DROP TABLE "tenants";
