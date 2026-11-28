-- Post-run check for make load-smoke (psql -v slug=... -v since=...).
-- Fails (non-zero exit) if any notification accepted since :since is still
-- pending, failed, or a billable send lacks its usage record.
\set ON_ERROR_STOP on
\pset footer off

\echo '== notifications by channel and status'
SELECT n.channel, n.status, count(*)
FROM notifications n JOIN tenants t ON t.id = n.tenant_id
WHERE t.slug = :'slug' AND n.created_at >= :'since'::timestamptz
GROUP BY 1, 2 ORDER BY 1, 2;

\echo '== dispatch lag (accept -> vendor accepted), seconds'
SELECT n.channel, count(*) AS n,
       round(percentile_cont(0.50) WITHIN GROUP (ORDER BY extract(epoch FROM e.created_at - n.created_at))::numeric, 3) AS p50,
       round(percentile_cont(0.95) WITHIN GROUP (ORDER BY extract(epoch FROM e.created_at - n.created_at))::numeric, 3) AS p95,
       round(percentile_cont(0.99) WITHIN GROUP (ORDER BY extract(epoch FROM e.created_at - n.created_at))::numeric, 3) AS p99,
       round(max(extract(epoch FROM e.created_at - n.created_at))::numeric, 3) AS max
FROM notifications n
JOIN tenants t ON t.id = n.tenant_id
JOIN notification_events e ON e.tenant_id = n.tenant_id AND e.notification_id = n.id AND e.type IN ('sent', 'delivered')
WHERE t.slug = :'slug' AND n.created_at >= :'since'::timestamptz
GROUP BY 1 ORDER BY 1;

\echo '== attempts > 1 (duplicate or retried claims)'
SELECT count(*) AS reclaimed FROM notifications n JOIN tenants t ON t.id = n.tenant_id
WHERE t.slug = :'slug' AND n.created_at >= :'since'::timestamptz AND n.attempt > 1;

SET verify.slug TO :'slug';
SET verify.since TO :'since';

DO $$
DECLARE
  bad_status int;
  unbilled int;
  dup_usage int;
BEGIN
  SELECT count(*) INTO bad_status FROM notifications n JOIN tenants t ON t.id = n.tenant_id
  WHERE t.slug = current_setting('verify.slug') AND n.created_at >= current_setting('verify.since')::timestamptz
    AND n.status NOT IN ('sent', 'delivered');
  SELECT count(*) INTO unbilled FROM notifications n JOIN tenants t ON t.id = n.tenant_id
  WHERE t.slug = current_setting('verify.slug') AND n.created_at >= current_setting('verify.since')::timestamptz
    AND ((n.billing_basis = 'sent' AND n.status IN ('sent', 'delivered')) OR (n.billing_basis = 'delivered' AND n.status = 'delivered'))
    AND NOT EXISTS (SELECT 1 FROM usage_records u WHERE u.notification_id = n.id);
  SELECT count(*) - count(DISTINCT notification_id) INTO dup_usage FROM usage_records;
  RAISE NOTICE 'not sent: %, billable without usage record: %, duplicate usage records: %', bad_status, unbilled, dup_usage;
  IF bad_status > 0 OR unbilled > 0 OR dup_usage > 0 THEN
    RAISE EXCEPTION 'load smoke verification failed';
  END IF;
END $$;
