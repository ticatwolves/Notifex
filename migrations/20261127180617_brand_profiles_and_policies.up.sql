-- create "brand_profiles" table
CREATE TABLE "brand_profiles" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, "application_id" uuid NULL, "doc" jsonb NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "brand_profiles_tenant_fk" FOREIGN KEY ("tenant_id") REFERENCES "tenants" ("id") ON UPDATE NO ACTION ON DELETE RESTRICT, CONSTRAINT "brand_profiles_application_fk" FOREIGN KEY ("tenant_id", "application_id") REFERENCES "applications" ("tenant_id", "id") ON UPDATE NO ACTION ON DELETE CASCADE, CONSTRAINT "brand_profiles_doc_object" CHECK (jsonb_typeof(doc) = 'object'::text));
-- create index "brand_profiles_application_key" to table: "brand_profiles"
CREATE UNIQUE INDEX "brand_profiles_application_key" ON "brand_profiles" ("tenant_id", "application_id") WHERE (application_id IS NOT NULL);
-- create index "brand_profiles_tenant_key" to table: "brand_profiles"
CREATE UNIQUE INDEX "brand_profiles_tenant_key" ON "brand_profiles" ("tenant_id") WHERE (application_id IS NULL);
-- create "policies" table
CREATE TABLE "policies" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, "application_id" uuid NULL, "doc" jsonb NOT NULL, "payload_retention_days" bigint NULL, PRIMARY KEY ("id"), CONSTRAINT "policies_tenant_fk" FOREIGN KEY ("tenant_id") REFERENCES "tenants" ("id") ON UPDATE NO ACTION ON DELETE RESTRICT, CONSTRAINT "policies_application_fk" FOREIGN KEY ("tenant_id", "application_id") REFERENCES "applications" ("tenant_id", "id") ON UPDATE NO ACTION ON DELETE CASCADE, CONSTRAINT "policies_doc_object" CHECK (jsonb_typeof(doc) = 'object'::text), CONSTRAINT "policies_retention_range" CHECK ((payload_retention_days IS NULL) OR ((payload_retention_days >= 1) AND (payload_retention_days <= 7))), CONSTRAINT "policies_retention_tenant_only" CHECK ((application_id IS NULL) OR (payload_retention_days IS NULL)));
-- create index "policies_application_key" to table: "policies"
CREATE UNIQUE INDEX "policies_application_key" ON "policies" ("tenant_id", "application_id") WHERE (application_id IS NOT NULL);
-- create index "policies_tenant_key" to table: "policies"
CREATE UNIQUE INDEX "policies_tenant_key" ON "policies" ("tenant_id") WHERE (application_id IS NULL);
