// Router tests: node --test images/router/test/router.test.js  (no dependencies)
'use strict';
const { test, before, after } = require('node:test');
const assert = require('node:assert');
const http = require('node:http');
const crypto = require('node:crypto');
const { spawn } = require('node:child_process');
const path = require('node:path');

// ROUTER_PORT: the public webhook port; INTERNAL: /status, /register, /idle, /healthz (#214)
const ROUTER_PORT = 18080, INTERNAL = 18085, SECRET = 'forge-secret', REG = 'reg-token';
let router; const wakes = { alice: [], bob: [] }; const agents = [];

const req = (port, method, p, body, headers = {}) => new Promise((res, rej) => {
  const data = body === undefined ? '' : (typeof body === 'string' ? body : JSON.stringify(body));
  const r = http.request({ host: '127.0.0.1', port, method, path: p, headers: { 'content-type': 'application/json', 'content-length': Buffer.byteLength(data), ...headers } },
    (x) => { let b = ''; x.on('data', (c) => (b += c)); x.on('end', () => res({ status: x.statusCode, body: b ? JSON.parse(b) : null })); });
  r.on('error', rej); r.end(data);
});
const sign = (b) => crypto.createHmac('sha256', SECRET).update(b).digest('hex');
const hook = (payload, { event = 'issues', sig } = {}) => {
  const b = JSON.stringify(payload);
  return req(ROUTER_PORT, 'POST', '/', b, { 'x-gitea-event': event, 'x-gitea-signature': sig ?? sign(b) });
};
const register = (name, port, repos = ['owner/playground'], token = REG) =>
  req(INTERNAL, 'POST', '/register', { agent: name, forge: 'gitea', user: name, repos, url: `http://127.0.0.1:${port}/`, token: `wake-${name}` }, { authorization: `Bearer ${token}` });
const settle = () => new Promise((r) => setTimeout(r, 150));

before(async () => {
  router = spawn(process.execPath, [path.join(__dirname, '..', 'router.js')], {
    env: { ...process.env, PORT: ROUTER_PORT, INTERNAL_PORT: INTERNAL, FORGE_SECRETS: `gitea=${SECRET}`, REGISTER_TOKEN: REG, REGISTRATION_TTL_SECONDS: '1' }, stdio: 'ignore' });
  for (const [name, port] of [['alice', 18081], ['bob', 18082]]) {
    agents.push(http.createServer((q, s) => {
      wakes[name].push(q.headers.authorization); q.resume(); s.writeHead(204).end();
    }).listen(port));
  }
  for (let i = 0; i < 50; i++) { try { await req(INTERNAL, 'GET', '/healthz'); await req(ROUTER_PORT, 'GET', '/'); break; } catch { await settle(); } }
});
after(() => { router.kill(); agents.forEach((a) => a.close()); });

test('registration needs the token', async () => {
  assert.equal((await register('alice', 18081, undefined, 'wrong')).status, 401);
  assert.equal((await register('alice', 18081)).status, 200);
  assert.equal((await register('bob', 18082)).status, 200);
});

test('bad or missing signature is rejected', async () => {
  assert.equal((await hook({ repository: { full_name: 'owner/playground' } }, { sig: 'deadbeef' })).status, 401);
  assert.equal((await hook({ repository: { full_name: 'owner/playground' } }, { sig: '' })).status, 401);
});

test('issue assigned to alice wakes only alice, with her token', async () => {
  wakes.alice.length = 0; wakes.bob.length = 0;
  const r = await hook({ repository: { full_name: 'owner/playground' }, issue: { user: { login: 'owner' }, assignees: [{ login: 'alice' }] } });
  assert.deepEqual(r.body, { routed: 1 }); await settle();
  assert.deepEqual(wakes.alice, ['Bearer wake-alice']); assert.equal(wakes.bob.length, 0);
});

test('review on bob\'s PR wakes bob (PR author)', async () => {
  wakes.alice.length = 0; wakes.bob.length = 0;
  const r = await hook({ repository: { full_name: 'owner/playground' }, pull_request: { user: { login: 'bob' }, requested_reviewers: [{ login: 'owner' }] } }, { event: 'pull_request_review_approved' });
  assert.deepEqual(r.body, { routed: 1 }); await settle();
  assert.equal(wakes.bob.length, 1); assert.equal(wakes.alice.length, 0);
});

test('other repo or nobody involved is not routed', async () => {
  assert.deepEqual((await hook({ repository: { full_name: 'someone/else' }, issue: { assignees: [{ login: 'alice' }] } })).body, { routed: 0 });
  assert.deepEqual((await hook({ repository: { full_name: 'owner/playground' }, issue: { user: { login: 'owner' }, assignees: [] } })).body, { routed: 0 });
});

test('registrations expire without refresh', async () => {
  await new Promise((r) => setTimeout(r, 1300));
  const r = await hook({ repository: { full_name: 'owner/playground' }, issue: { assignees: [{ login: 'alice' }] } });
  assert.deepEqual(r.body, { routed: 0 });
  assert.equal((await req(INTERNAL, 'GET', '/status')).body.agents.length, 0);
});

test('the public port serves only webhook deliveries (#214)', async () => {
  for (const [m, p] of [['GET', '/status'], ['GET', '/healthz'], ['GET', '/internal/x'], ['POST', '/internal/approvals/a/b'],
    ['POST', '/register'], ['POST', '/idle'], ['GET', '/hook'], ['GET', '/']]) {
    assert.equal((await req(ROUTER_PORT, m, p, {})).status, 404, `${m} ${p}`);
  }
  // a registration attempt with the right token is refused there too
  assert.equal((await req(ROUTER_PORT, 'POST', '/register', { agent: 'x' }, { authorization: `Bearer ${REG}` })).status, 404);
  // deliveries still reach the signature check on /hook and /
  assert.equal((await req(ROUTER_PORT, 'POST', '/hook', '{}', { 'x-gitea-event': 'issues', 'x-gitea-signature': 'x' })).status, 401);
  assert.equal((await hook({ repository: { full_name: 'owner/playground' } })).status, 202);
});

test('the internal port serves status, health and deliveries', async () => {
  assert.equal((await req(INTERNAL, 'GET', '/healthz')).status, 200);
  const s = await req(INTERNAL, 'GET', '/status');
  assert.equal(s.status, 200); assert.ok(Array.isArray(s.body.agents));
  const b = JSON.stringify({ repository: { full_name: 'owner/playground' } });
  assert.equal((await req(INTERNAL, 'POST', '/hook', b, { 'x-gitea-event': 'issues', 'x-gitea-signature': sign(b) })).status, 202);
});
