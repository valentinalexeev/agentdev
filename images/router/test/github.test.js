// Router on GitHub (#2): node --test images/router/test/github.test.js  (no dependencies)
// All agents are one App bot, so routing is by the label agent:<name> and the head branch agent/<name>/...
'use strict';
const { test, before, after } = require('node:test');
const assert = require('node:assert');
const http = require('node:http');
const crypto = require('node:crypto');
const { spawn } = require('node:child_process');
const path = require('node:path');

const PORT = 18180, SECRET = 'gh-secret', REG = 'reg-token';
let router; const wakes = { alice: 0, bob: 0 }; const servers = [];
const req = (method, p, body, headers = {}) => new Promise((res, rej) => {
  const data = body === undefined ? '' : (typeof body === 'string' ? body : JSON.stringify(body));
  const r = http.request({ host: '127.0.0.1', port: PORT, method, path: p, headers: { 'content-type': 'application/json', 'content-length': Buffer.byteLength(data), ...headers } },
    (x) => { let b = ''; x.on('data', (c) => (b += c)); x.on('end', () => res({ status: x.statusCode, body: b ? JSON.parse(b) : null })); });
  r.on('error', rej); r.end(data);
});
const hook = (payload, event = 'issues', secret = SECRET) => {
  const b = JSON.stringify(payload);
  return req('POST', '/', b, { 'x-github-event': event, 'x-hub-signature-256': `sha256=${crypto.createHmac('sha256', secret).update(b).digest('hex')}` });
};
const settle = () => new Promise((r) => setTimeout(r, 150));
const repo = { full_name: 'octo-org/repo' };
const bot = { login: 'my-agentdev[bot]' };

before(async () => {
  router = spawn(process.execPath, [path.join(__dirname, '..', 'router.js')], {
    env: { ...process.env, PORT, FORGE_SECRETS: `github=${SECRET}`, REGISTER_TOKEN: REG }, stdio: 'ignore' });
  for (const [name, port] of [['alice', 18181], ['bob', 18182]]) {
    servers.push(http.createServer((q, s) => { wakes[name]++; q.resume(); s.writeHead(204).end(); }).listen(port));
  }
  for (let i = 0; i < 50; i++) { try { await req('GET', '/healthz'); break; } catch { await settle(); } }
  for (const [name, port] of [['alice', 18181], ['bob', 18182]]) {
    const r = await req('POST', '/register', { agent: name, forge: 'github', user: name, repos: ['octo-org/repo'], url: `http://127.0.0.1:${port}/`, token: `w-${name}` },
      { authorization: `Bearer ${REG}` });
    assert.strictEqual(r.status, 200);
  }
});
after(() => { router.kill(); servers.forEach((s) => s.close()); });
const reset = () => { wakes.alice = 0; wakes.bob = 0; };

test('a labelled issue wakes that agent only', async () => {
  reset();
  const r = await hook({ action: 'labeled', repository: repo, label: { name: 'agent:alice' },
    issue: { number: 12, user: { login: 'octo-dev' }, labels: [{ name: 'agent:alice' }], assignees: [] } });
  await settle();
  assert.strictEqual(r.body.routed, 1); assert.deepStrictEqual(wakes, { alice: 1, bob: 0 });
});

test('a review on an agent PR routes by the head branch, not the shared bot login', async () => {
  reset();
  const r = await hook({ action: 'submitted', repository: repo, review: { user: { login: 'octo-dev' } },
    pull_request: { number: 14, user: bot, head: { ref: 'agent/bob/issue-9' }, labels: [], requested_reviewers: [] } }, 'pull_request_review');
  await settle();
  assert.strictEqual(r.body.routed, 1); assert.deepStrictEqual(wakes, { alice: 0, bob: 1 });
});

test('a check run on an agent PR wakes its agent', async () => {
  reset();
  await hook({ action: 'completed', repository: repo, check_run: { pull_requests: [{ number: 14, head: { ref: 'agent/alice/issue-12' } }] } }, 'check_run');
  await settle();
  assert.deepStrictEqual(wakes, { alice: 1, bob: 0 });
});

test('logins, bad names and other repos do not route', async () => {
  reset();
  // the bot login is shared, an assignee named alice is a person, not the agent
  await hook({ action: 'assigned', repository: repo, issue: { number: 1, user: bot, assignees: [{ login: 'alice' }], labels: [] } });
  await hook({ action: 'labeled', repository: repo, issue: { number: 2, labels: [{ name: 'agent:Alice/../bob' }, { name: 'agent:' }] } });
  await hook({ action: 'labeled', repository: { full_name: 'other/repo' }, issue: { number: 3, labels: [{ name: 'agent:alice' }] } });
  await settle();
  assert.deepStrictEqual(wakes, { alice: 0, bob: 0 });
});

test('a wrong GitHub signature is rejected', async () => {
  reset();
  const r = await hook({ repository: repo, issue: { labels: [{ name: 'agent:alice' }] } }, 'issues', 'wrong');
  assert.strictEqual(r.status, 401); assert.deepStrictEqual(wakes, { alice: 0, bob: 0 });
});
