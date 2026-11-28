-- reverse: create index "template_tenant_id_action_id_locale" to table: "templates"
DROP INDEX "template_tenant_id_action_id_locale";
-- reverse: create "templates" table
DROP TABLE "templates";
-- reverse: create index "action_tenant_id_workflow_id_channel" to table: "actions"
DROP INDEX "action_tenant_id_workflow_id_channel";
-- reverse: create index "action_tenant_id_id" to table: "actions"
DROP INDEX "action_tenant_id_id";
-- reverse: create "actions" table
DROP TABLE "actions";
-- reverse: create index "workflow_tenant_id_id" to table: "workflows"
DROP INDEX "workflow_tenant_id_id";
-- reverse: create index "workflow_tenant_id_application_id_key" to table: "workflows"
DROP INDEX "workflow_tenant_id_application_id_key";
-- reverse: create "workflows" table
DROP TABLE "workflows";
