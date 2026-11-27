// Usage: k6 run -e BASE_URL=http://localhost:8080 -e API_KEYS=key1,key2 -e SCENARIO=load k6/trigger-load.js
import http from 'k6/http';
import { check } from 'k6';
import { uuidv4 } from 'https://jslib.k6.io/k6-utils/1.4.0/index.js';

const BASE = __ENV.BASE_URL || 'http://localhost:8080';
const KEYS = (__ENV.API_KEYS || 'test_key').split(',');
const PEAK = parseInt(__ENV.PEAK_RPS || '500');

const scenarios = {
  smoke:  { executor: 'constant-vus', vus: 5, duration: '1m' },
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
    'checks': ['rate>0.999'],
  },
};

export default function () {
  const key = KEYS[Math.floor(Math.random() * KEYS.length)];
  const body = JSON.stringify({
    workflow: 'order_shipped',
    recipients: [{ id: `r${__VU}-${__ITER}`, email: `user${__VU}@example.test`, phone: '+910000000000' }],
    payload: { order_id: `A${__ITER}`, tracking_url: 'https://example.test/t/1' },
  });
  const res = http.post(`${BASE}/v1/trigger`, body, {
    headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${key}`, 'Idempotency-Key': uuidv4() },
  });
  check(res, {
    'accepted (202)': (r) => r.status === 202,
    'has run_id': (r) => r.status !== 202 || !!r.json('run_id'),
  });
}
