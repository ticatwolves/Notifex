-- create "workflows" table
CREATE TABLE "workflows" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, "application_id" uuid NOT NULL, "key" character varying NOT NULL, "name" character varying NOT NULL, "description" character varying NOT NULL DEFAULT '', "status" character varying NOT NULL DEFAULT 'draft', "payload_schema" jsonb NULL, PRIMARY KEY ("id"), CONSTRAINT "workflows_tenant_fk" FOREIGN KEY ("tenant_id") REFERENCES "tenants" ("id") ON UPDATE NO ACTION ON DELETE RESTRICT, CONSTRAINT "workflows_application_fk" FOREIGN KEY ("tenant_id", "application_id") REFERENCES "applications" ("tenant_id", "id") ON UPDATE NO ACTION ON DELETE RESTRICT);
-- create index "workflow_tenant_id_application_id_key" to table: "workflows"
CREATE UNIQUE INDEX "workflow_tenant_id_application_id_key" ON "workflows" ("tenant_id", "application_id", "key");
-- create index "workflow_tenant_id_id" to table: "workflows"
CREATE UNIQUE INDEX "workflow_tenant_id_id" ON "workflows" ("tenant_id", "id");
-- create "actions" table
CREATE TABLE "actions" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, "workflow_id" uuid NOT NULL, "channel" character varying NOT NULL, "enabled" boolean NOT NULL DEFAULT true, PRIMARY KEY ("id"), CONSTRAINT "actions_tenant_fk" FOREIGN KEY ("tenant_id") REFERENCES "tenants" ("id") ON UPDATE NO ACTION ON DELETE RESTRICT, CONSTRAINT "actions_workflow_fk" FOREIGN KEY ("tenant_id", "workflow_id") REFERENCES "workflows" ("tenant_id", "id") ON UPDATE NO ACTION ON DELETE CASCADE);
-- create index "action_tenant_id_id" to table: "actions"
CREATE UNIQUE INDEX "action_tenant_id_id" ON "actions" ("tenant_id", "id");
-- create index "action_tenant_id_workflow_id_channel" to table: "actions"
CREATE UNIQUE INDEX "action_tenant_id_workflow_id_channel" ON "actions" ("tenant_id", "workflow_id", "channel");
-- create "templates" table
CREATE TABLE "templates" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, "action_id" uuid NOT NULL, "locale" character varying NOT NULL, "content" jsonb NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "templates_tenant_fk" FOREIGN KEY ("tenant_id") REFERENCES "tenants" ("id") ON UPDATE NO ACTION ON DELETE RESTRICT, CONSTRAINT "templates_action_fk" FOREIGN KEY ("tenant_id", "action_id") REFERENCES "actions" ("tenant_id", "id") ON UPDATE NO ACTION ON DELETE CASCADE, CONSTRAINT "templates_content_object" CHECK (jsonb_typeof(content) = 'object'::text));
-- create index "template_tenant_id_action_id_locale" to table: "templates"
CREATE UNIQUE INDEX "template_tenant_id_action_id_locale" ON "templates" ("tenant_id", "action_id", "locale");
