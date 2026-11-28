-- reverse: create index "usagerecord_tenant_id_billable_at" to table: "usage_records"
DROP INDEX "usagerecord_tenant_id_billable_at";
-- reverse: create index "usage_records_notification_id_key" to table: "usage_records"
DROP INDEX "usage_records_notification_id_key";
-- reverse: create "usage_records" table
DROP TABLE "usage_records";
-- reverse: create index "scheduleddispatch_tenant_id_notification_id" to table: "scheduled_dispatch"
DROP INDEX "scheduleddispatch_tenant_id_notification_id";
-- reverse: create index "scheduleddispatch_due_at" to table: "scheduled_dispatch"
DROP INDEX "scheduleddispatch_due_at";
-- reverse: create "scheduled_dispatch" table
DROP TABLE "scheduled_dispatch";
-- reverse: create index "run_tenant_id_id" to table: "runs"
DROP INDEX "run_tenant_id_id";
-- reverse: create index "run_tenant_id_application_id_environment_id_id" to table: "runs"
DROP INDEX "run_tenant_id_application_id_environment_id_id";
-- reverse: create "runs" table
DROP TABLE "runs";
-- reverse: create index "platformsmsusage_tenant_id_day" to table: "platform_sms_usage"
DROP INDEX "platformsmsusage_tenant_id_day";
-- reverse: create "platform_sms_usage" table
DROP TABLE "platform_sms_usage";
-- reverse: create index "idempotencyrecord_tenant_id_key" to table: "idempotency_records"
DROP INDEX "idempotencyrecord_tenant_id_key";
-- reverse: create index "idempotencyrecord_expires_at" to table: "idempotency_records"
DROP INDEX "idempotencyrecord_expires_at";
-- reverse: create "idempotency_records" table
DROP TABLE "idempotency_records";
-- reverse: create index "dispatchoutbox_tenant_id_notification_id" to table: "dispatch_outbox"
DROP INDEX "dispatchoutbox_tenant_id_notification_id";
-- reverse: create index "dispatch_outbox_unpublished" to table: "dispatch_outbox"
DROP INDEX "dispatch_outbox_unpublished";
-- reverse: create index "dispatch_outbox_published" to table: "dispatch_outbox"
DROP INDEX "dispatch_outbox_published";
-- reverse: create "dispatch_outbox" table
DROP TABLE "dispatch_outbox";
-- reverse: create index "configsnapshot_tenant_id_hash" to table: "config_snapshots"
DROP INDEX "configsnapshot_tenant_id_hash";
-- reverse: create "config_snapshots" table
DROP TABLE "config_snapshots";
-- reverse: create index "scheduler_leases_name_key" to table: "scheduler_leases"
DROP INDEX "scheduler_leases_name_key";
-- reverse: create "scheduler_leases" table
DROP TABLE "scheduler_leases";
-- reverse: create index "platform_sms_totals_day_key" to table: "platform_sms_totals"
DROP INDEX "platform_sms_totals_day_key";
-- reverse: create "platform_sms_totals" table
DROP TABLE "platform_sms_totals";
