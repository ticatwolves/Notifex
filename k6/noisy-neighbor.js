// Tenant A floods, tenant B stays steady. Compare B's latency with and without A's flood.
// Usage: k6 run -e BASE_URL=... -e KEY_A=... -e KEY_B=... k6/noisy-neighbor.js
import http from 'k6/http';
import { check } from 'k6';
import { uuid4 } from './lib.js';
import { Trend } from 'k6/metrics';

const BASE = __ENV.BASE_URL || 'http://localhost:8080';
const bLatency = new Trend('tenant_b_latency', true);

export const options = {
  scenarios: {
    tenant_a_flood: { executor: 'ramping-arrival-rate', exec: 'flood', startRate: 50, timeUnit: '1s',
      preAllocatedVUs: 500, maxVUs: 5000, startTime: '2m',
      stages: [{ target: 3000, duration: '1m' }, { target: 3000, duration: '10m' }, { target: 0, duration: '1m' }] },
    tenant_b_steady: { executor: 'constant-arrival-rate', exec: 'steady', rate: 50, timeUnit: '1s',
      duration: '15m', preAllocatedVUs: 100, maxVUs: 400 },
  },
  thresholds: { tenant_b_latency: ['p(99)<150'], 'http_req_failed{scenario:tenant_b_steady}': ['rate<0.001'] },
};

function trigger(key, tag) {
  return http.post(`${BASE}/v1/trigger`, JSON.stringify({
    workflow: 'order_shipped',
    recipients: [{ id: 'r1', email: 'u@example.test', phone: '+919876543210' }],
    payload: { order_id: tag },
  }), { headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${key}`, 'Idempotency-Key': uuid4() } });
}
export function flood() { trigger(__ENV.KEY_A, 'A'); }
export function steady() {
  const r = trigger(__ENV.KEY_B, 'B');
  bLatency.add(r.timings.duration);
  check(r, { 'B accepted': (x) => x.status === 202 });
}
// Also assert separately (via metrics/DB) that tenant B's dispatch lag stays within SLO during the flood.
