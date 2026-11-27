-- create "tenants" table
CREATE TABLE "tenants" ("id" uuid NOT NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, "name" character varying NOT NULL, "slug" character varying NOT NULL, "status" character varying NOT NULL DEFAULT 'active', "plan" character varying NOT NULL DEFAULT 'free', "quotas" jsonb NULL, "platform_sms_daily_limit" bigint NOT NULL DEFAULT 0, "sandbox" boolean NOT NULL DEFAULT true, "dek_wrapped" bytea NULL, "dek_key_id" character varying NOT NULL, "dek_version" bigint NOT NULL DEFAULT 1, PRIMARY KEY ("id"), CONSTRAINT "tenants_dek_version_range" CHECK ((dek_version >= 1) AND (dek_version <= 255)), CONSTRAINT "tenants_platform_sms_daily_limit_nonneg" CHECK (platform_sms_daily_limit >= 0));
-- create index "tenants_slug_key" to table: "tenants"
CREATE UNIQUE INDEX "tenants_slug_key" ON "tenants" ("slug");
-- create "applications" table
CREATE TABLE "applications" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, "name" character varying NOT NULL, "key" character varying NOT NULL, "description" character varying NOT NULL DEFAULT '', PRIMARY KEY ("id"), CONSTRAINT "applications_tenant_fk" FOREIGN KEY ("tenant_id") REFERENCES "tenants" ("id") ON UPDATE NO ACTION ON DELETE RESTRICT);
-- create index "application_tenant_id_id" to table: "applications"
CREATE UNIQUE INDEX "application_tenant_id_id" ON "applications" ("tenant_id", "id");
-- create index "application_tenant_id_key" to table: "applications"
CREATE UNIQUE INDEX "application_tenant_id_key" ON "applications" ("tenant_id", "key");
-- create "environments" table
CREATE TABLE "environments" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, "application_id" uuid NOT NULL, "name" character varying NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "environments_tenant_fk" FOREIGN KEY ("tenant_id") REFERENCES "tenants" ("id") ON UPDATE NO ACTION ON DELETE RESTRICT, CONSTRAINT "environments_application_fk" FOREIGN KEY ("tenant_id", "application_id") REFERENCES "applications" ("tenant_id", "id") ON UPDATE NO ACTION ON DELETE RESTRICT);
-- create index "environment_tenant_id_application_id_id" to table: "environments"
CREATE UNIQUE INDEX "environment_tenant_id_application_id_id" ON "environments" ("tenant_id", "application_id", "id");
-- create index "environment_tenant_id_application_id_name" to table: "environments"
CREATE UNIQUE INDEX "environment_tenant_id_application_id_name" ON "environments" ("tenant_id", "application_id", "name");
-- create "api_keys" table
CREATE TABLE "api_keys" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, "application_id" uuid NOT NULL, "environment_id" uuid NOT NULL, "name" character varying NOT NULL, "prefix" character varying NOT NULL, "hash" bytea NOT NULL, "scopes" jsonb NOT NULL, "expires_at" timestamptz NULL, "last_used_at" timestamptz NULL, "revoked_at" timestamptz NULL, PRIMARY KEY ("id"), CONSTRAINT "api_keys_tenant_fk" FOREIGN KEY ("tenant_id") REFERENCES "tenants" ("id") ON UPDATE NO ACTION ON DELETE RESTRICT, CONSTRAINT "api_keys_environment_fk" FOREIGN KEY ("tenant_id", "application_id", "environment_id") REFERENCES "environments" ("tenant_id", "application_id", "id") ON UPDATE NO ACTION ON DELETE RESTRICT);
-- create index "api_keys_prefix_key" to table: "api_keys"
CREATE UNIQUE INDEX "api_keys_prefix_key" ON "api_keys" ("prefix");
-- create index "apikey_tenant_id_application_id" to table: "api_keys"
CREATE INDEX "apikey_tenant_id_application_id" ON "api_keys" ("tenant_id", "application_id");
-- create "audit_events" table
CREATE TABLE "audit_events" ("id" uuid NOT NULL, "tenant_id" uuid NULL, "actor_type" character varying NOT NULL, "actor_id" character varying NOT NULL, "action" character varying NOT NULL, "entity_type" character varying NOT NULL, "entity_id" character varying NOT NULL, "before" jsonb NULL, "after" jsonb NULL, "request_id" character varying NOT NULL DEFAULT '', "created_at" timestamptz NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "audit_events_tenant_fk" FOREIGN KEY ("tenant_id") REFERENCES "tenants" ("id") ON UPDATE NO ACTION ON DELETE RESTRICT);
-- create index "auditevent_created_at" to table: "audit_events"
CREATE INDEX "auditevent_created_at" ON "audit_events" ("created_at");
-- create index "auditevent_tenant_id_created_at" to table: "audit_events"
CREATE INDEX "auditevent_tenant_id_created_at" ON "audit_events" ("tenant_id", "created_at");
-- create "users" table
CREATE TABLE "users" ("id" uuid NOT NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, "email" character varying NOT NULL, "name" character varying NOT NULL DEFAULT '', "password_hash" character varying NULL, "status" character varying NOT NULL DEFAULT 'active', "email_verified_at" timestamptz NULL, PRIMARY KEY ("id"));
-- create index "users_email_key" to table: "users"
CREATE UNIQUE INDEX "users_email_key" ON "users" ("email");
-- create "memberships" table
CREATE TABLE "memberships" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, "user_id" uuid NOT NULL, "role" character varying NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "memberships_tenant_fk" FOREIGN KEY ("tenant_id") REFERENCES "tenants" ("id") ON UPDATE NO ACTION ON DELETE RESTRICT, CONSTRAINT "memberships_user_fk" FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE CASCADE);
-- create index "membership_tenant_id_user_id" to table: "memberships"
CREATE UNIQUE INDEX "membership_tenant_id_user_id" ON "memberships" ("tenant_id", "user_id");
-- create index "membership_user_id" to table: "memberships"
CREATE INDEX "membership_user_id" ON "memberships" ("user_id");
