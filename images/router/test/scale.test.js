// Scale-to-zero tests (#35) against a fake Kubernetes API (plain HTTP, K8S_API_URL).
//   node --test images/router/test/
'use strict';
const { test, before, after } = require('node:test');
const assert = require('node:assert');
const http = require('node:http');
const crypto = require('node:crypto');
const { spawn } = require('node:child_process');
const path = require('node:path');

const PORT = 18090, K8S = 18091, SECRET = 's', REG = 'r', NS = 'coding-agents';
let router, k8s, badJson = false;
// fake cluster: StatefulSets by name; patches recorded
const sts = {};
const patches = [];
const mk = (name, user, replicas, enabled = true) => {
  sts[name] = { metadata: { name, labels: { app: 'coding-agent' },
    annotations: { 'agentdev/scale-to-zero': String(enabled), 'agentdev/user': user, 'agentdev/repos': 'owner/playground', 'agentdev/forge': 'gitea' } },
    spec: { replicas } };
};

const req = (method, p, body, headers = {}) => new Promise((res, rej) => {
  const data = body === undefined ? '' : (typeof body === 'string' ? body : JSON.stringify(body));
  const r = http.request({ host: '127.0.0.1', port: PORT, method, path: p, headers: { 'content-type': 'application/json', 'content-length': Buffer.byteLength(data), ...headers } },
    (x) => { let b = ''; x.on('data', (c) => (b += c)); x.on('end', () => res({ status: x.statusCode, body: b ? JSON.parse(b) : null })); });
  r.on('error', rej); r.end(data);
});
const hook = (payload) => { const b = JSON.stringify(payload);
  return req('POST', '/', b, { 'x-gitea-event': 'issues', 'x-gitea-signature': crypto.createHmac('sha256', SECRET).update(b).digest('hex') }); };
const idle = (agent, idleSince = Date.now(), token = `w-${agent}`) => req('POST', '/idle', { agent, idleSince }, { authorization: `Bearer ${token}` });
// register an agent whose wake URL is a sink (port 1: nothing listens, forwards just fail) unless a port is given
const reg = (agent, port = 1) => req('POST', '/register', { agent, forge: 'gitea', user: agent, repos: ['owner/playground'], url: `http://127.0.0.1:${port}/`, token: `w-${agent}` }, { authorization: `Bearer ${REG}` });
const issueFor = (login) => ({ repository: { full_name: 'owner/playground' }, issue: { user: { login: 'owner' }, assignees: [{ login }] } });

before(async () => {
  k8s = http.createServer((q, s) => {
    let b = ''; q.on('data', (c) => (b += c));
    q.on('end', () => {
      const base = `/apis/apps/v1/namespaces/${NS}/statefulsets`;
      const u = new URL(q.url, 'http://x');
      if (q.method === 'GET' && u.pathname === base) { s.writeHead(200).end(badJson ? '{not json' : JSON.stringify({ items: Object.values(sts) })); return; }
      const m = u.pathname.match(new RegExp(`^${base}/([^/]+)/scale$`));
      if (q.method === 'PATCH' && m && sts[m[1]]) {
        const n = JSON.parse(b).spec.replicas; sts[m[1]].spec.replicas = n; patches.push([m[1], n, q.headers['content-type']]);
        s.writeHead(200).end('{}'); return;
      }
      s.writeHead(404).end('{}');
    });
  }).listen(K8S);
  router = spawn(process.execPath, [path.join(__dirname, '..', 'router.js')], { stdio: 'ignore',
    env: { ...process.env, PORT, FORGE_SECRETS: `gitea=${SECRET}`, REGISTER_TOKEN: REG, K8S_NAMESPACE: NS, K8S_API_URL: `http://127.0.0.1:${K8S}` } });
  for (let i = 0; i < 50; i++) { try { await req('GET', '/healthz'); break; } catch { await new Promise((r) => setTimeout(r, 100)); } }
});
after(() => { router.kill(); k8s.close(); });

test('event for a stopped, opted-in agent scales it 0 -> 1', async () => {
  mk('agent-alice', 'alice', 0); patches.length = 0;
  const r = await hook(issueFor('alice'));
  assert.deepEqual(r.body, { routed: 0, started: ['agent-alice'] });
  assert.deepEqual(patches, [['agent-alice', 1, 'application/merge-patch+json']]);
});

test('a second event while it starts does not scale again', async () => {
  patches.length = 0; sts['agent-alice'].spec.replicas = 0;   // the pod is not up yet
  const r = await hook(issueFor('alice'));
  assert.deepEqual(r.body, { routed: 0 }); assert.equal(patches.length, 0);
});

test('agents without the opt-in are never scaled', async () => {
  mk('agent-bob', 'bob', 0, false); patches.length = 0;
  assert.deepEqual((await hook(issueFor('bob'))).body, { routed: 0 });
  await reg('bob');
  assert.deepEqual((await idle('bob')).body, { stopped: false, reason: 'scale-to-zero not enabled' });
  assert.equal(patches.length, 0);
});

test('idle agent is scaled to 0', async () => {
  mk('agent-carol', 'carol', 1); patches.length = 0; await reg('carol');
  assert.deepEqual((await idle('carol')).body, { stopped: true });
  assert.deepEqual(patches.map((p) => p.slice(0, 2)), [['agent-carol', 0]]);
});

test('idle request after a wake is refused (no stop mid-work)', async () => {
  mk('agent-dave', 'dave', 1); patches.length = 0;
  const wakeSrv = http.createServer((q, s) => { q.resume(); s.writeHead(204).end(); }).listen(18092);
  await reg('dave', 18092);
  const idleSince = Date.now() - 1000;
  assert.deepEqual((await hook(issueFor('dave'))).body, { routed: 1 });
  assert.deepEqual((await idle('dave', idleSince)).body, { stopped: false, reason: 'woken since idle' });
  assert.equal(patches.length, 0);
  wakeSrv.close();
});

test('idle needs the agent\'s own token: one agent cannot stop another', async () => {
  mk('agent-erin', 'erin', 1); mk('agent-frank', 'frank', 1); await reg('erin'); await reg('frank'); patches.length = 0;
  assert.equal((await idle('erin', Date.now(), 'w-frank')).status, 401);
  assert.equal((await idle('erin', Date.now(), REG)).status, 401);
  assert.equal((await idle('ghost')).status, 401);   // not registered
  assert.equal(patches.length, 0);
});

test('an event right after a scale-down brings the agent back (no lost wake)', async () => {
  mk('agent-gina', 'gina', 1); await reg('gina');
  assert.deepEqual((await idle('gina')).body, { stopped: true });
  sts['agent-gina'].spec.replicas = 1;   // the API has not applied the scale-down yet: pod still terminating
  patches.length = 0;
  const r = await hook(issueFor('gina'));
  assert.deepEqual(r.body, { routed: 0, started: ['agent-gina'] });
  assert.deepEqual(patches.map((p) => p.slice(0, 2)), [['agent-gina', 1]]);
});

test('a blocked agent is never started by an event (#27)', async () => {
  mk('agent-ivy', 'ivy', 0); sts['agent-ivy'].metadata.annotations['agentdev/blocked'] = 'true'; patches.length = 0;
  assert.deepEqual((await hook(issueFor('ivy'))).body, { routed: 0 });
  assert.equal(patches.length, 0);
});

test('a bad response from the Kubernetes API does not crash the router', async () => {
  mk('agent-hal', 'hal', 0); sts['agent-hal'].metadata.annotations['agentdev/repos'] = 'owner/playground';
  badJson = true;
  assert.deepEqual((await hook(issueFor('hal'))).body, { routed: 0 });
  badJson = false;
  assert.equal((await req('GET', '/healthz')).status, 200);
});
