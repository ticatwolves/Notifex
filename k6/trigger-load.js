// Trigger-path load scenarios (docs/07). Triggers the seeded workflow
// (internal/devseed: order_shipped, email + SMS).
//
// Local smoke (stack up with DISPATCH_ENABLED=true): make load-smoke
// Other scenarios: k6 run -e BASE_URL=... -e API_KEYS=key1,key2 -e SCENARIO=load -e PEAK_RPS=500 k6/trigger-load.js
//
// smoke is an arrival-rate scenario below the per-key trigger limit
// (TRIGGER_RATE_PER_MINUTE, default 6000/min per task), so 429s mean a bug.
import http from 'k6/http';
import { check } from 'k6';
import { uuid4 } from './lib.js';

const BASE = __ENV.BASE_URL || 'http://localhost:8080';
const KEYS = (__ENV.API_KEYS || 'test_key').split(',');
const PEAK = parseInt(__ENV.PEAK_RPS || '500');
const WORKFLOW = __ENV.WORKFLOW || 'order_shipped';
const SMOKE_RPS = parseInt(__ENV.SMOKE_RPS || '50');

const scenarios = {
  smoke:  { executor: 'constant-arrival-rate', rate: SMOKE_RPS, timeUnit: '1s', duration: __ENV.DURATION || '1m', preAllocatedVUs: 20, maxVUs: 100 },
  load:   { executor: 'ramping-arrival-rate', startRate: 10, timeUnit: '1s', preAllocatedVUs: 200, maxVUs: 2000,
            stages: [{ target: PEAK, duration: '5m' }, { target: PEAK, duration: '30m' }, { target: 0, duration: '2m' }] },
  stress: { executor: 'ramping-arrival-rate', startRate: PEAK / 2, timeUnit: '1s', preAllocatedVUs: 500, maxVUs: 5000,
            stages: Array.from({ length: 8 }, (_, i) => ({ target: Math.round(PEAK * (0.5 + 0.25 * (i + 1))), duration: '10m' })) },
  spike:  { executor: 'ramping-arrival-rate', startRate: 10, timeUnit: '1s', preAllocatedVUs: 500, maxVUs: 5000,
            stages: [{ target: 10, duration: '2m' }, { target: PEAK * 10, duration: '30s' }, { target: PEAK * 10, duration: '5m' }, { target: 10, duration: '1m' }] },
  soak:   { executor: 'constant-arrival-rate', rate: Math.round(PEAK * 0.7), timeUnit: '1s', duration: '4h', preAllocatedVUs: 300, maxVUs: 2000 },
};

export const options = {
  scenarios: { main: scenarios[__ENV.SCENARIO || 'smoke'] },
  thresholds: {
    http_req_failed: ['rate<0.001'],
    http_req_duration: ['p(95)<60', 'p(99)<100'],
    checks: ['rate>0.999'],
    dropped_iterations: ['count<1'],
  },
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
};

export default function () {
  const key = KEYS[Math.floor(Math.random() * KEYS.length)];
  const body = JSON.stringify({
    workflow: WORKFLOW,
    recipients: [{ id: `r${__VU}-${__ITER}`, email: `user${__VU}@example.test`, phone: '+919876543210' }],
    payload: { order_id: `A${__VU}-${__ITER}`, tracking_url: 'https://example.test/t/1' },
  });
  const res = http.post(`${BASE}/v1/trigger`, body, {
    headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${key}`, 'Idempotency-Key': uuid4() },
  });
  check(res, {
    'accepted (202)': (r) => r.status === 202,
    'has run_id': (r) => r.status !== 202 || !!r.json('run_id'),
    'all notifications queued': (r) => r.status !== 202 || (r.json('notifications') || []).every((n) => n.status === 'queued'),
  });
}

export function handleSummary(data) {
  const out = { stdout: textSummary(data) };
  if (__ENV.SUMMARY_JSON) out[__ENV.SUMMARY_JSON] = JSON.stringify(data, null, 2);
  return out;
}

function textSummary(data) {
  const d = data.metrics.http_req_duration.values;
  const reqs = data.metrics.http_reqs.values;
  const failed = data.metrics.http_req_failed.values.rate;
  const f = (x) => x.toFixed(1);
  return `\ntrigger accept: ${reqs.count} requests (${f(reqs.rate)}/s), failed ${(failed * 100).toFixed(3)}%\n` +
    `latency ms: med ${f(d.med)}  p90 ${f(d['p(90)'])}  p95 ${f(d['p(95)'])}  p99 ${f(d['p(99)'])}  max ${f(d.max)}\n`;
}
