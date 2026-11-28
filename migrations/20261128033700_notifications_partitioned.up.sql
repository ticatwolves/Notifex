-- Hand-written (ADR-014 §1): notifications and notification_events are
-- range-partitioned by day on created_at, which is the UUIDv7 id's timestamp.
-- ent skips these tables (schema.ManualTables); the scheduler keeps partitions
-- 14 days ahead (internal/db/partition). No DEFAULT partition: a row outside
-- coverage fails loudly.
CREATE TABLE "notifications" (
  "id" uuid NOT NULL,
  "created_at" timestamptz NOT NULL,
  "updated_at" timestamptz NOT NULL,
  "tenant_id" uuid NOT NULL,
  "run_id" uuid NOT NULL,
  "application_id" uuid NOT NULL,
  "environment_id" uuid NOT NULL,
  "action_id" uuid NOT NULL,
  "channel" character varying NOT NULL,
  "locale" character varying NOT NULL DEFAULT '',
  "status" character varying NOT NULL,
  "attempt" bigint NOT NULL DEFAULT 0,
  "lease_until" timestamptz NULL,
  "next_attempt_at" timestamptz NULL,
  "recipient" bytea NULL,
  "recipient_masked" character varying NOT NULL DEFAULT '',
  "recipient_hash" bytea NULL,
  "recipient_ref" character varying NOT NULL DEFAULT '',
  "credential_id" uuid NULL,
  "credential_mode" character varying NOT NULL DEFAULT '',
  "provider" character varying NOT NULL DEFAULT '',
  "billing_basis" character varying NOT NULL DEFAULT '',
  "sms_reserved" boolean NOT NULL DEFAULT false,
  "last_error_code" character varying NOT NULL DEFAULT '',
  "last_error" character varying NOT NULL DEFAULT '',
  "vendor_message_id" character varying NOT NULL DEFAULT '',
  "rendered_content" bytea NULL,
  "vendor_response" bytea NULL,
  "terminal_at" timestamptz NULL,
  "purged_at" timestamptz NULL,
  PRIMARY KEY ("id", "created_at"),
  CONSTRAINT "notifications_tenant_fk" FOREIGN KEY ("tenant_id") REFERENCES "tenants" ("id") ON UPDATE NO ACTION ON DELETE RESTRICT,
  CONSTRAINT "notifications_channel_check" CHECK ("channel" IN ('email', 'sms', 'push', 'slack', 'outlook', 'whatsapp', 'webhook')),
  CONSTRAINT "notifications_status_check" CHECK ("status" IN ('queued', 'deferred', 'sending', 'retrying', 'sent', 'delivered', 'sent_unconfirmed', 'failed', 'suppressed', 'canceled', 'expired')),
  CONSTRAINT "notifications_attempt_nonneg" CHECK ("attempt" >= 0)
) PARTITION BY RANGE ("created_at");
-- A run's notifications; the purge job's work list (ADR-006).
CREATE INDEX "notifications_tenant_run" ON "notifications" ("tenant_id", "run_id");
CREATE INDEX "notifications_purge" ON "notifications" ("terminal_at") WHERE "purged_at" IS NULL AND "terminal_at" IS NOT NULL;

CREATE TABLE "notification_events" (
  "id" uuid NOT NULL,
  "created_at" timestamptz NOT NULL,
  "tenant_id" uuid NOT NULL,
  "notification_id" uuid NOT NULL,
  "type" character varying NOT NULL,
  "detail" jsonb NULL,
  PRIMARY KEY ("id", "created_at"),
  CONSTRAINT "notification_events_tenant_fk" FOREIGN KEY ("tenant_id") REFERENCES "tenants" ("id") ON UPDATE NO ACTION ON DELETE RESTRICT
) PARTITION BY RANGE ("created_at");
CREATE INDEX "notification_events_timeline" ON "notification_events" ("tenant_id", "notification_id", "created_at");

-- Initial partitions, yesterday through 14 days ahead (UTC), so a fresh
-- database accepts writes before the scheduler's first run. Names and bounds
-- match internal/db/partition.
DO $$
DECLARE
  d date;
  t text;
BEGIN
  FOR d IN SELECT generate_series((now() AT TIME ZONE 'UTC')::date - 1, (now() AT TIME ZONE 'UTC')::date + 14, interval '1 day')::date LOOP
    FOREACH t IN ARRAY ARRAY['notifications', 'notification_events'] LOOP
      EXECUTE format('CREATE TABLE IF NOT EXISTS %I PARTITION OF %I FOR VALUES FROM (%L) TO (%L)',
        t || '_p' || to_char(d, 'YYYYMMDD'), t, d::text || 'T00:00:00Z', (d + 1)::text || 'T00:00:00Z');
    END LOOP;
  END LOOP;
END
$$;
