// Mixed-workload load test for the NetworkAI API.
//
//   k6 run -e RATE=200 -e DURATION=60s tools/loadtest/mixed.js
//
// Point API at the container's IP, not a published localhost port: Docker's
// userland port proxy refuses connections under bursty load and would be
// measured instead of the service (see docs/benchmark.md).
//
// Open model: requests arrive at a fixed RATE per second no matter how slow
// the server gets (constant-arrival-rate). A closed model, where each virtual
// user waits for its previous response, quietly lowers the load when the
// server slows down and hides queueing latency ("coordinated omission").
//
// Traffic mix (per iteration):
//   45% browse   GET /posts?limit=20 (half of those follow one cursor page)
//    5% foryou   GET /feed?limit=20 (personalised, two-stage ranking)
//   20% read     GET /posts/{id}
//   20% search   GET /posts/search?q=<1-2 topic words>
//   10% write    POST /posts (text only, authenticated)
import http from 'k6/http';
import { check } from 'k6';

const API = __ENV.API || 'http://localhost:8080';
const RATE = Number(__ENV.RATE || 100);
const DURATION = __ENV.DURATION || '60s';
// Share of iterations that request the personalised feed (default 5%).
const FEED_SHARE = Number(__ENV.FEED_SHARE ?? 0.05);

const WORDS = ('run marathon trail coffee espresso roast travel flight beach recipe pasta oven ' +
  'music guitar album photo camera lens book novel author game console quest gym squat ' +
  'protein code database deploy model embedding gpu hiking summit forest bike gravel ' +
  'movie director garden tomatoes dog puppy budget invest outfit vintage physics lab ramen tacos').split(' ');

export const options = {
  scenarios: {
    mixed: {
      executor: 'constant-arrival-rate',
      rate: RATE,
      timeUnit: '1s',
      duration: DURATION,
      preAllocatedVUs: Math.max(50, RATE),
      maxVUs: RATE * 4,
    },
  },
  thresholds: {
    http_req_failed: ['rate<0.01'],
    'http_req_duration{op:browse}': ['p(95)<100'],
    'http_req_duration{op:read}': ['p(95)<100'],
    'http_req_duration{op:search}': ['p(95)<150'],
    'http_req_duration{op:write}': ['p(95)<150'],
    'http_req_duration{op:foryou}': ['p(95)<250'],
  },
  summaryTrendStats: ['avg', 'p(50)', 'p(95)', 'p(99)', 'max'],
};

export function setup() {
  const tokens = [];
  for (let i = 1; i <= 20; i++) {
    const username = `user_${String(i).padStart(5, '0')}`;
    const r = http.post(`${API}/auth/login`, JSON.stringify({ username, password: 'password123' }),
      { headers: { 'Content-Type': 'application/json' } });
    if (r.status !== 200) throw new Error(`login ${username}: ${r.status} (did you run tools/synth/load.sh?)`);
    tokens.push(r.json('token'));
  }
  const ids = [];
  let cursor = '';
  for (let p = 0; p < 20; p++) {
    const r = http.get(`${API}/posts?limit=100${cursor ? '&cursor=' + cursor : ''}`);
    r.json('items').forEach((it) => ids.push(it.id));
    cursor = r.json('next_cursor');
    if (!cursor) break;
  }
  return { tokens, ids };
}

const pick = (arr) => arr[Math.floor(Math.random() * arr.length)];

export default function (data) {
  const x = Math.random();
  if (x < FEED_SHARE) {
    const r = http.get(`${API}/feed?limit=20`, {
      headers: { Authorization: `Bearer ${pick(data.tokens)}` }, tags: { op: 'foryou' },
    });
    check(r, { 'foryou 200': (res) => res.status === 200 });
  } else if (x < 0.5) {
    const r = http.get(`${API}/posts?limit=20`, { tags: { op: 'browse' } });
    check(r, { 'browse 200': (res) => res.status === 200 });
    const next = r.status === 200 ? r.json('next_cursor') : '';
    if (next && Math.random() < 0.5) {
      http.get(`${API}/posts?limit=20&cursor=${next}`, { tags: { op: 'browse' } });
    }
  } else if (x < 0.7) {
    const r = http.get(`${API}/posts/${pick(data.ids)}`, { tags: { op: 'read' } });
    check(r, { 'read 200': (res) => res.status === 200 });
  } else if (x < 0.9) {
    const q = Math.random() < 0.5 ? pick(WORDS) : `${pick(WORDS)} ${pick(WORDS)}`;
    const r = http.get(`${API}/posts/search?q=${encodeURIComponent(q)}&limit=20`, { tags: { op: 'search' } });
    check(r, { 'search 200': (res) => res.status === 200 });
  } else {
    const body = JSON.stringify({ message: `load test ${pick(WORDS)} ${pick(WORDS)} ${Date.now()}` });
    const r = http.post(`${API}/posts`, body, {
      headers: { Authorization: `Bearer ${pick(data.tokens)}`, 'Content-Type': 'application/json' },
      tags: { op: 'write' },
    });
    check(r, { 'write 201': (res) => res.status === 201 });
  }
}

export function handleSummary(data) {
  const out = __ENV.SUMMARY;
  const res = { stdout: textSummary(data) };
  if (out) res[out] = JSON.stringify(data, null, 1);
  return res;
}

function textSummary(data) {
  const m = data.metrics;
  const line = (op) => {
    const k = `http_req_duration{op:${op}}`;
    if (!m[k]) return `  ${op.padEnd(7)} n/a`;
    const v = m[k].values;
    return `  ${op.padEnd(7)} p50=${v['p(50)'].toFixed(1)}ms p95=${v['p(95)'].toFixed(1)}ms p99=${v['p(99)'].toFixed(1)}ms max=${v.max.toFixed(0)}ms`;
  };
  const reqs = m.http_reqs.values;
  const failed = (m.http_req_failed.values.rate * 100).toFixed(2);
  const dropped = m.dropped_iterations ? m.dropped_iterations.values.count : 0;
  return [
    `target rate ${RATE}/s for ${DURATION}: achieved ${reqs.rate.toFixed(1)} req/s, failed ${failed}%, dropped iterations ${dropped}`,
    line('browse'), line('read'), line('search'), line('write'), line('foryou'), '',
  ].join('\n');
}
