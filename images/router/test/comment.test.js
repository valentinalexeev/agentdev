// Comment wake-ups (#227): a comment on an agent's PR or issue wakes that agent at once, on GitHub and on Gitea.
//   node --test images/router/test/comment.test.js  (no dependencies)
// GitHub: a comment webhook (issue_comment) has no PR head branch. A PR labelled agent:<name> (the loop labels its own
// PRs since #227) routes by the label; an older PR without it is looked up with the router's read-only App credential.
'use strict';
const { test, before, after } = require('node:test');
const assert = require('node:assert');
const http = require('node:http');
const crypto = require('node:crypto');
const fs = require('node:fs');
const os = require('node:os');
const { spawn } = require('node:child_process');
const path = require('node:path');

const PORT = 18380, GH = 18383, K8S = 18384, NS = 'coding-agents';
const SECRETS = { github: 'gh-secret', gitea: 'gt-secret' }, REG = 'reg-token';
let router; const wakes = { alice: 0, bob: 0, carol: 0 }; const servers = []; const ghCalls = []; const patches = [];
const sts = {};
const req = (method, p, body, headers = {}) => new Promise((res, rej) => {
  const data = body === undefined ? '' : (typeof body === 'string' ? body : JSON.stringify(body));
  const r = http.request({ host: '127.0.0.1', port: PORT, method, path: p, headers: { 'content-type': 'application/json', 'content-length': Buffer.byteLength(data), ...headers } },
    (x) => { let b = ''; x.on('data', (c) => (b += c)); x.on('end', () => res({ status: x.statusCode, body: b ? JSON.parse(b) : null })); });
  r.on('error', rej); r.end(data);
});
const hook = (forge, event, payload) => {
  const b = JSON.stringify(payload);
  const sig = crypto.createHmac('sha256', SECRETS[forge]).update(b).digest('hex');
  return req('POST', '/', b, forge === 'github' ? { 'x-github-event': event, 'x-hub-signature-256': `sha256=${sig}` }
    : { 'x-gitea-event': event, 'x-gitea-signature': sig });
};
const settle = () => new Promise((r) => setTimeout(r, 200));
const reset = () => { for (const k of Object.keys(wakes)) wakes[k] = 0; ghCalls.length = 0; patches.length = 0; };
const ghRepo = { full_name: 'octo-org/repo' }, gtRepo = { full_name: 'owner/playground' };
const bot = { login: 'my-agentdev[bot]' };
// PRs on the fake GitHub: 14 is alice's, 15 is a person's branch
const own = { full_name: 'octo-org/repo' };
const ghPulls = { 14: { number: 14, head: { ref: 'agent/alice/issue-12', repo: own } }, 15: { number: 15, head: { ref: 'feature/x', repo: own } },
  21: { number: 21, head: { ref: 'agent/carol/issue-3', repo: own } },
  22: { number: 22, head: { ref: 'agent/alice/issue-12', repo: { full_name: 'mallory/repo' } } } };   // a fork

before(async () => {
  const { privateKey } = crypto.generateKeyPairSync('rsa', { modulusLength: 2048 });
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'comment-'));
  fs.writeFileSync(path.join(dir, 'github-app-id'), '1012345');
  fs.writeFileSync(path.join(dir, 'github-installation-id'), '51234567');
  fs.writeFileSync(path.join(dir, 'private-key.pem'), privateKey.export({ type: 'pkcs1', format: 'pem' }));
  servers.push(http.createServer((q, s) => {   // fake GitHub API
    const u = new URL(q.url, 'http://x'); ghCalls.push({ method: q.method, path: u.pathname, auth: q.headers.authorization });
    if (q.method === 'POST' && u.pathname === '/app/installations/51234567/access_tokens') {
      s.writeHead(201).end(JSON.stringify({ token: 'ghs_READONLY', expires_at: new Date(Date.now() + 3600e3).toISOString() })); return;
    }
    const m = u.pathname.match(/^\/repos\/octo-org\/repo\/pulls\/(\d+)$/);
    if (q.method === 'GET' && m && ghPulls[m[1]] && q.headers.authorization === 'Bearer ghs_READONLY') {
      s.writeHead(200).end(JSON.stringify(ghPulls[m[1]])); return;
    }
    s.writeHead(404).end('{}');
  }).listen(GH));
  servers.push(http.createServer((q, s) => {   // fake Kubernetes API: carol is a stopped scale-to-zero agent
    let b = ''; q.on('data', (c) => (b += c));
    q.on('end', () => {
      const base = `/apis/apps/v1/namespaces/${NS}/statefulsets`; const u = new URL(q.url, 'http://x');
      if (q.method === 'GET' && u.pathname === base) { s.writeHead(200).end(JSON.stringify({ items: Object.values(sts) })); return; }
      const mm = u.pathname.match(new RegExp(`^${base}/([^/]+)/scale$`));
      if (q.method === 'PATCH' && mm && sts[mm[1]]) { sts[mm[1]].spec.replicas = JSON.parse(b).spec.replicas; patches.push(mm[1]); s.writeHead(200).end('{}'); return; }
      s.writeHead(404).end('{}');
    });
  }).listen(K8S));
  sts['agent-carol'] = { metadata: { name: 'agent-carol', labels: { app: 'coding-agent' }, annotations: { 'agentdev/scale-to-zero': 'true',
    'agentdev/user': 'carol', 'agentdev/repos': 'octo-org/repo', 'agentdev/forge': 'github' } }, spec: { replicas: 0 } };
  router = spawn(process.execPath, [path.join(__dirname, '..', 'router.js')], { stdio: 'ignore',
    env: { ...process.env, PORT: PORT + 5, INTERNAL_PORT: PORT, REGISTER_TOKEN: REG,
      FORGE_SECRETS: `github=${SECRETS.github},gitea=${SECRETS.gitea}`, GITHUB_API_URL: `http://127.0.0.1:${GH}`, FORGE_POLL_DIR: dir,
      K8S_NAMESPACE: NS, K8S_API_URL: `http://127.0.0.1:${K8S}` } });
  for (const [name, port] of [['alice', 18381], ['bob', 18382]]) {
    servers.push(http.createServer((q, s) => { wakes[name]++; q.resume(); s.writeHead(204).end(); }).listen(port));
  }
  for (let i = 0; i < 50; i++) { try { await req('GET', '/healthz'); break; } catch { await settle(); } }
  // alice and bob on GitHub (agent names), alice also on Gitea under her forge login
  for (const [name, forge, repos, port] of [['alice', 'github', ['octo-org/repo'], 18381], ['bob', 'github', ['octo-org/repo'], 18382]]) {
    const r = await req('POST', '/register', { agent: name, forge, user: name, repos, url: `http://127.0.0.1:${port}/`, token: `w-${name}` },
      { authorization: `Bearer ${REG}` });
    assert.strictEqual(r.status, 200);
  }
});
after(() => { router.kill(); servers.forEach((s) => s.close()); });

const ghComment = (number, labels = []) => ({ action: 'created', repository: ghRepo, comment: { user: { login: 'octo-dev' }, body: 'please fix' },
  issue: { number, user: bot, labels, assignees: [], pull_request: { url: `https://api.github.com/repos/octo-org/repo/pulls/${number}` } } });

test('GitHub: a comment on a labelled agent PR wakes that agent without a forge call', async () => {
  reset();
  const r = await hook('github', 'issue_comment', ghComment(14, [{ name: 'agent:alice' }]));
  await settle();
  assert.strictEqual(r.body.routed, 1); assert.deepStrictEqual({ a: wakes.alice, b: wakes.bob }, { a: 1, b: 0 });
  assert.strictEqual(ghCalls.length, 0);
});

test('GitHub: a comment on an older agent PR without the label is routed by looking up its head branch', async () => {
  reset();
  const r = await hook('github', 'issue_comment', ghComment(14));
  await settle();
  assert.strictEqual(r.body.routed, 1); assert.deepStrictEqual({ a: wakes.alice, b: wakes.bob }, { a: 1, b: 0 });
  assert.ok(ghCalls.some((c) => c.method === 'GET' && c.path === '/repos/octo-org/repo/pulls/14' && c.auth === 'Bearer ghs_READONLY'));
});

test('GitHub: a comment on a person\'s PR, or on a plain issue without a label, wakes nobody', async () => {
  reset();
  await hook('github', 'issue_comment', ghComment(15));
  const issue = ghComment(16); delete issue.issue.pull_request;
  await hook('github', 'issue_comment', issue);
  await settle();
  assert.deepStrictEqual({ a: wakes.alice, b: wakes.bob }, { a: 0, b: 0 });
  assert.ok(!ghCalls.some((c) => c.path.endsWith('/pulls/16')), 'a plain issue is never looked up as a PR');
});

test('GitHub: a comment on a labelled issue (the agent\'s work) wakes the agent', async () => {
  reset();
  const issue = ghComment(12, [{ name: 'agent:bob' }]); delete issue.issue.pull_request;
  await hook('github', 'issue_comment', issue);
  await settle();
  assert.deepStrictEqual({ a: wakes.alice, b: wakes.bob }, { a: 0, b: 1 });
});

test('GitHub: a comment on the PR of a stopped agent starts it (scale to zero)', async () => {
  reset();
  const r = await hook('github', 'issue_comment', ghComment(21));
  await settle();
  assert.deepStrictEqual(r.body.started, ['agent-carol']); assert.deepStrictEqual(patches, ['agent-carol']);
});

test('GitHub: a fork that names its branch like an agent\'s wakes nobody', async () => {
  reset();
  const r = await hook('github', 'issue_comment', ghComment(22));
  await settle();
  assert.strictEqual(r.status, 202); assert.deepStrictEqual({ a: wakes.alice, b: wakes.bob }, { a: 0, b: 0 });
});

test('GitHub: a PR the lookup cannot find routes nowhere, and the delivery is still answered', async () => {
  reset();
  const r = await hook('github', 'issue_comment', ghComment(99));
  await settle();
  assert.strictEqual(r.status, 202); assert.strictEqual(r.body.routed, 0);
  assert.deepStrictEqual({ a: wakes.alice, b: wakes.bob }, { a: 0, b: 0 });
});

test('GitHub: the read-only token is minted once and reused for later lookups', async () => {
  reset();
  await hook('github', 'issue_comment', ghComment(14));
  await hook('github', 'issue_comment', ghComment(14));
  await settle();
  assert.strictEqual(ghCalls.filter((c) => c.path.endsWith('/access_tokens')).length, 0, 'token minted again');
  assert.strictEqual(ghCalls.filter((c) => c.path === '/repos/octo-org/repo/pulls/14').length, 2);
  assert.strictEqual(wakes.alice, 2);
});

// last: re-registers alice for Gitea (one registration per agent)
test('Gitea: a comment on an agent PR routes by the PR author (the agent login)', async () => {
  reset();
  const reg = await req('POST', '/register', { agent: 'alice', forge: 'gitea', user: 'alice', repos: ['owner/playground'],
    url: 'http://127.0.0.1:18381/', token: 'w-alice' }, { authorization: `Bearer ${REG}` });
  assert.strictEqual(reg.status, 200);
  const r = await hook('gitea', 'issue_comment', { action: 'created', repository: gtRepo, comment: { user: { login: 'owner' }, body: 'x' },
    issue: { number: 3, user: { login: 'alice' }, assignees: [], pull_request: { merged: false } }, is_pull: true });
  await settle();
  assert.strictEqual(r.body.routed, 1); assert.strictEqual(wakes.alice, 1);
});
