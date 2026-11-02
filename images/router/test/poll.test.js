// Wake poll tests (#181): scale to zero without webhooks. A fake Kubernetes API and a fake Gitea.
//   node --test images/router/test/poll.test.js
'use strict';
const { test, before, after } = require('node:test');
const assert = require('node:assert');
const http = require('node:http');
const fs = require('node:fs');
const os = require('node:os');
const { spawn } = require('node:child_process');
const path = require('node:path');

const PORT = 18290, K8S = 18291, GITEA = 18292, NS = 'coding-agents';
let router, k8s, gitea;
const sts = {}; const patches = []; const giteaCalls = [];
let issues = {}; let pulls = []; let giteaDown = false;
const mk = (name, user, replicas, extra = {}) => {
  sts[name] = { metadata: { name, labels: { app: 'coding-agent' },
    annotations: { 'agentdev/scale-to-zero': 'true', 'agentdev/user': user, 'agentdev/repos': 'owner/playground', 'agentdev/forge': 'gitea', ...extra } },
    spec: { replicas } };
};
// "new" work is stamped a little in the future: the router starts after the fixtures, and looks back one interval
const ago = (s) => new Date(Date.now() - s * 1000).toISOString();
const soon = () => new Promise((r) => setTimeout(r, 100));
async function until(cond, ms = 5000) { for (let t = 0; t < ms; t += 100) { if (cond()) return true; await soon(); } return cond(); }

before(async () => {
  k8s = http.createServer((q, s) => {
    let b = ''; q.on('data', (c) => (b += c));
    q.on('end', () => {
      const base = `/apis/apps/v1/namespaces/${NS}/statefulsets`;
      const u = new URL(q.url, 'http://x');
      if (q.method === 'GET' && u.pathname === base) { s.writeHead(200).end(JSON.stringify({ items: Object.values(sts) })); return; }
      const m = u.pathname.match(new RegExp(`^${base}/([^/]+)/scale$`));
      if (q.method === 'PATCH' && m && sts[m[1]]) { const n = JSON.parse(b).spec.replicas; sts[m[1]].spec.replicas = n; patches.push([m[1], n]); s.writeHead(200).end('{}'); return; }
      s.writeHead(404).end('{}');
    });
  }).listen(K8S);
  gitea = http.createServer((q, s) => {
    const u = new URL(q.url, 'http://x');
    giteaCalls.push({ path: u.pathname, q: Object.fromEntries(u.searchParams), auth: q.headers.authorization });
    if (giteaDown) { s.writeHead(500).end('{}'); return; }
    if (q.headers.authorization !== 'token poll-token') { s.writeHead(401).end('{}'); return; }
    if (u.pathname === '/api/v1/repos/owner/playground/issues') {
      const who = u.searchParams.get('assigned_by'); const since = Date.parse(u.searchParams.get('since'));
      s.writeHead(200).end(JSON.stringify((issues[who] || []).filter((i) => Date.parse(i.updated_at) > since))); return;
    }
    if (u.pathname === '/api/v1/repos/owner/playground/pulls') { s.writeHead(200).end(JSON.stringify(pulls)); return; }
    s.writeHead(404).end('[]');
  }).listen(GITEA);
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'poll-'));
  fs.writeFileSync(path.join(dir, 'gitea-token'), 'poll-token\n');
  // the agents stopped before this router started (a restart): the poll looks back one interval
  mk('agent-alice', 'alice', 0); mk('agent-bob', 'bob', 0); mk('agent-carol', 'carol', 0);
  mk('agent-ivy', 'ivy', 0, { 'agentdev/blocked': 'true' }); mk('agent-dan', 'dan', 1);
  issues = { alice: [{ number: 1, updated_at: ago(-10) }], ivy: [{ number: 2, updated_at: ago(-10) }], dan: [{ number: 3, updated_at: ago(-10) }],
    bob: [{ number: 4, updated_at: ago(3600) }] };   // bob's issue is old: seen before he stopped
  pulls = [{ number: 9, head: { ref: 'agent/carol/issue-5' }, updated_at: ago(-10) }, { number: 10, head: { ref: 'feature/x' }, updated_at: ago(-10) }];
  router = spawn(process.execPath, [path.join(__dirname, '..', 'router.js')], { stdio: 'ignore',
    env: { ...process.env, PORT, FORGE_SECRETS: 'gitea=s', REGISTER_TOKEN: 'r', K8S_NAMESPACE: NS, K8S_API_URL: `http://127.0.0.1:${K8S}`,
      WAKE_POLL_SECONDS: '1', GITEA_URL: `http://127.0.0.1:${GITEA}/`, FORGE_POLL_DIR: dir } });
});
after(() => { router.kill(); k8s.close(); gitea.close(); });

test('the poll starts a stopped agent with a new issue, with the read-only token', async () => {
  assert.ok(await until(() => sts['agent-alice'].spec.replicas === 1), 'alice not started');
  const c = giteaCalls.find((x) => x.q.assigned_by === 'alice');
  assert.equal(c.auth, 'token poll-token'); assert.equal(c.q.state, 'open'); assert.equal(c.q.type, 'issues'); assert.ok(c.q.since);
});

test('the poll starts a stopped agent with new activity on its own PR', async () => {
  assert.ok(await until(() => sts['agent-carol'].spec.replicas === 1), 'carol not started');
});

test('no new work, a blocked agent, or a running agent: nothing is scaled', async () => {
  await new Promise((r) => setTimeout(r, 2500));
  assert.equal(sts['agent-bob'].spec.replicas, 0);
  assert.equal(sts['agent-ivy'].spec.replicas, 0);
  assert.deepEqual(patches.filter((p) => !['agent-alice', 'agent-carol'].includes(p[0])), []);
  assert.ok(!giteaCalls.some((x) => x.q.assigned_by === 'ivy' || x.q.assigned_by === 'dan'), 'blocked or running agent polled');
});

test('a forge error is logged and does not start anything or stop the router', async () => {
  giteaDown = true; issues.bob = [{ number: 5, updated_at: ago(-10) }];
  await new Promise((r) => setTimeout(r, 1500));
  assert.equal(sts['agent-bob'].spec.replicas, 0);
  giteaDown = false;
  assert.ok(await until(() => sts['agent-bob'].spec.replicas === 1), 'bob not started after the forge came back');
  const st = await new Promise((res) => http.get(`http://127.0.0.1:${PORT}/status`, (r) => { let b = ''; r.on('data', (x) => (b += x)); r.on('end', () => res(JSON.parse(b))); }));
  assert.ok(st.stats.pollErrors > 0 && st.stats.polledUp === 3);
});
