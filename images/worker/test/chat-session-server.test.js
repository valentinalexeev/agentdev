// Tests of chat-session-server (#346): node --test images/worker/test/chat-session-server.test.js
'use strict';
const test = require('node:test');
const assert = require('node:assert');
const http = require('http');
const os = require('os');
const fs = require('fs');
const path = require('path');
const crypto = require('crypto');

process.env.CHAT_LOG_DIR = fs.mkdtempSync(path.join(os.tmpdir(), 'chat-'));
const { Session, parseDrafts, tokenOK, handler, startSettings } = require('../chat-session-server.js');

const TOKEN = 'secret-token-1';
const SHA = crypto.createHash('sha256').update(TOKEN).digest('hex');

function newSession(extra = {}) {
  const writes = [];
  const s = new Session({ name: 's1', repo: 'o/r', agent: 'a', idleSeconds: 1800, write: (l) => writes.push(JSON.parse(l)), ...extra });
  return { s, writes };
}

test('drafts: a valid block becomes a draft, anything else is dropped', () => {
  const ok = 'Here:\n```agentdev-draft\n{"title":"Add X","what":"w","acceptance":"a"}\n```\nend';
  const r = parseDrafts(ok);
  assert.deepStrictEqual(r.drafts, [{ title: 'Add X', what: 'w', acceptance: 'a', decisions: '' }]);
  assert.strictEqual(r.text, 'Here:\n\nend');
  for (const bad of ['{"title":"x"}', 'not json', '{"title":"a\\nb","what":"w","acceptance":"a"}', '{"title":"x","what":"w","acceptance":"a","decisions":5}']) {
    const b = parseDrafts('```agentdev-draft\n' + bad + '\n```');
    assert.deepStrictEqual(b.drafts, [], bad);
    assert.strictEqual(b.text, '');
  }
});

test('event log: stream-json becomes events, appended in order and to the file', () => {
  const { s } = newSession();
  s.onClaude(JSON.stringify({ type: 'system', subtype: 'init', model: 'm', tools: ['Bash'] }));
  s.onClaude(JSON.stringify({ type: 'assistant', message: { content: [{ type: 'text', text: 'hi\n```agentdev-draft\n{"title":"T","what":"w","acceptance":"a"}\n```' }, { type: 'tool_use', id: 't1', name: 'Bash', input: { command: 'ls' } }] } }));
  s.onClaude(JSON.stringify({ type: 'user', message: { content: [{ type: 'tool_result', tool_use_id: 't1', content: 'out' }] } }));
  s.onClaude(JSON.stringify({ type: 'result', usage: { input_tokens: 10, output_tokens: 5 } }));
  s.onClaude('garbage');
  assert.deepStrictEqual(s.events.map((e) => e.type), ['ready', 'assistant', 'draft', 'tool', 'tool-result', 'turn']);
  assert.deepStrictEqual(s.events.map((e) => e.seq), [1, 2, 3, 4, 5, 6]);
  assert.strictEqual(s.events[4].data.untrusted, true);
  assert.deepStrictEqual(s.tokens, { input: 10, output: 5 });
  const lines = fs.readFileSync(path.join(process.env.CHAT_LOG_DIR, 'events.jsonl'), 'utf8').trim().split('\n');
  assert.ok(lines.length >= 6);
});

test('permissions: pending until a person answers, once; deny on end', () => {
  const { s, writes } = newSession();
  s.onClaude(JSON.stringify({ type: 'control_request', request_id: 'r1', request: { subtype: 'can_use_tool', tool_name: 'Bash', input: { command: 'rm x' } } }));
  const p = s.events.at(-1);
  assert.strictEqual(p.type, 'permission');
  assert.strictEqual(writes.length, 0, 'nothing is answered by itself');
  s.answer(p.data.id, 'alice', 'allow');
  assert.deepStrictEqual(writes[0].response, { subtype: 'success', request_id: 'r1', response: { behavior: 'allow', updatedInput: { command: 'rm x' } } });
  assert.throws(() => s.answer(p.data.id, 'bob', 'deny'), /no pending/);
  s.onClaude(JSON.stringify({ type: 'control_request', request_id: 'r2', request: { subtype: 'can_use_tool', tool_name: 'Bash', input: {} } }));
  s.end('test');
  assert.strictEqual(s.events.at(-2).data.behavior, 'deny');
  assert.throws(() => s.message('alice', 'hi'), /ended/);
});

test('messages carry the author', () => {
  const { s, writes } = newSession();
  const ev = s.message('alice', 'hello');
  assert.strictEqual(ev.author, 'alice');
  assert.strictEqual(writes[0].message.content, '[alice] hello');
  assert.throws(() => s.message('', 'x'), /author/);
  assert.throws(() => s.message('a b', 'x'), /author/);
});

test('idle: ends 30 minutes after the last viewer, not before', () => {
  const { s } = newSession();
  const t0 = Date.now();
  s.presence(['alice']);
  assert.strictEqual(s.tick(t0 + 3600e3), false, 'a viewer keeps it alive');
  s.presence([]);
  const left = s.lastViewer;
  assert.strictEqual(s.tick(left + 29 * 60e3), false);
  assert.ok(s.state().idleEndsAt);
  assert.strictEqual(s.tick(left + 30 * 60e3), true);
});

test('idle: a terminal client (which the model could fake) extends the session only for a bounded time', () => {
  const { s } = newSession({ terminalMaxSeconds: 7200, tmuxClients: () => 1 });
  const t0 = s.lastConsole;
  assert.strictEqual(s.tick(t0 + 3600e3), false, 'terminal client inside the window keeps it alive');
  assert.strictEqual(s.tick(t0 + 7199e3), false, 'still inside the window');
  const last = s.lastViewer;
  assert.strictEqual(s.tick(last + 29 * 60e3), false, 'window over, idle clock runs from the last counted terminal tick');
  assert.strictEqual(s.tick(last + 30 * 60e3), true, 'ends 30 min later even with a terminal client attached');
  // a console viewer renews the window
  const { s: s2 } = newSession({ terminalMaxSeconds: 7200, tmuxClients: () => 1 });
  s2.presence(['alice']); s2.presence([]);
  assert.strictEqual(s2.tick(s2.lastConsole + 7000e3), false);
});

test('http: token required, requests from the pod itself refused', async () => {
  const { s } = newSession();
  const srv = http.createServer(handler(s, { tokenSha256: SHA, localAddresses: new Set(['10.9.9.9']) }));
  await new Promise((r) => srv.listen(0, '127.0.0.1', r));
  const port = srv.address().port;
  const call = (method, p, token, body) => new Promise((resolve, reject) => {
    const req = http.request({ host: '127.0.0.1', port, method, path: p, headers: token ? { Authorization: `Bearer ${token}` } : {} }, (res) => {
      let b = ''; res.on('data', (c) => { b += c; }); res.on('end', () => resolve({ code: res.statusCode, body: JSON.parse(b) }));
    });
    req.on('error', reject); if (body) req.write(JSON.stringify(body)); req.end();
  });
  assert.strictEqual((await call('GET', '/events', null)).code, 401);
  assert.strictEqual((await call('GET', '/events', 'wrong')).code, 401);
  const m = await call('POST', '/messages', TOKEN, { author: 'alice', text: 'hi' });
  assert.strictEqual(m.code, 200);
  const ev = await call('GET', '/events?after=0', TOKEN);
  assert.strictEqual(ev.body.events[0].type, 'user');
  assert.strictEqual((await call('GET', '/events?after=x', TOKEN)).code, 400);
  srv.close();
  // the same server seen from inside the pod
  const srv2 = http.createServer(handler(s, { tokenSha256: SHA, localAddresses: new Set(['127.0.0.1']) }));
  await new Promise((r) => srv2.listen(0, '127.0.0.1', r));
  const r2 = await new Promise((resolve) => http.get({ host: '127.0.0.1', port: srv2.address().port, path: '/events', headers: { Authorization: `Bearer ${TOKEN}` } }, (res) => resolve(res.statusCode)));
  assert.strictEqual(r2, 403);
  srv2.close();
  assert.strictEqual(tokenOK(`Bearer ${TOKEN}`, SHA), true);
  assert.strictEqual(tokenOK(`Bearer ${TOKEN}`, ''), false);
});

// #570: model and effort. The message shapes are the stream-json control protocol of the pinned Claude Code (2.1.312):
// set_model {model} answers {model: <effective>}; apply_flag_settings {settings: {effortLevel}} answers {}.
test('settings: start flags from CLAUDE_MODEL and CLAUDE_EFFORT, bad values dropped', () => {
  assert.deepStrictEqual(startSettings({ CLAUDE_MODEL: 'sonnet', CLAUDE_EFFORT: 'high' }),
    { args: ['--model', 'sonnet', '--effort', 'high'], settings: { model: 'sonnet', effort: 'high' } });
  assert.deepStrictEqual(startSettings({}), { args: [], settings: { model: '', effort: '' } });
  for (const bad of [{ CLAUDE_MODEL: 'gpt-5' }, { CLAUDE_MODEL: '--dangerously-skip-permissions' }, { CLAUDE_EFFORT: 'ultra' }, { CLAUDE_MODEL: 'opus --verbose' }]) {
    assert.deepStrictEqual(startSettings(bad).args, [], JSON.stringify(bad));
  }
});

test('settings: set_model and effortLevel go to Claude; the state changes only when Claude confirms', () => {
  const { s, writes } = newSession({ settings: { model: 'sonnet', effort: '' } });
  s.onClaude(JSON.stringify({ type: 'system', subtype: 'init', model: 'claude-sonnet-5-5', tools: [] }));
  assert.strictEqual(s.state().settings.model, 'claude-sonnet-5-5', 'the effective model from init');
  s.changeSettings('alice', 'opus', 'low');
  assert.strictEqual(writes.length, 2);
  assert.strictEqual(writes[0].type, 'control_request');
  assert.deepStrictEqual(writes[0].request, { subtype: 'set_model', model: 'opus' });
  assert.deepStrictEqual(writes[1].request, { subtype: 'apply_flag_settings', settings: { effortLevel: 'low' } });
  assert.notStrictEqual(writes[0].request_id, writes[1].request_id);
  assert.deepStrictEqual(s.state().settings, { model: 'claude-sonnet-5-5', effort: '' }, 'nothing changes before the answer');
  s.onClaude(JSON.stringify({ type: 'control_response', response: { subtype: 'success', request_id: writes[0].request_id, response: { model: 'claude-opus-5-5' } } }));
  s.onClaude(JSON.stringify({ type: 'control_response', response: { subtype: 'error', request_id: writes[1].request_id, error: 'effortLevel not supported' } }));
  assert.deepStrictEqual(s.state().settings, { model: 'claude-opus-5-5', effort: '' });
  const evs = s.events.filter((e) => e.type === 'settings');
  assert.deepStrictEqual(evs.map((e) => [e.data.kind, e.data.status, e.author]),
    [['model', 'requested', 'alice'], ['effort', 'requested', 'alice'], ['model', 'applied', 'alice'], ['effort', 'refused', 'alice']]);
  assert.strictEqual(evs[2].data.value, 'claude-opus-5-5');
  // an answer nobody asked for, or a second answer, changes nothing
  s.onClaude(JSON.stringify({ type: 'control_response', response: { subtype: 'success', request_id: writes[0].request_id, response: { model: 'claude-haiku-5-5' } } }));
  assert.strictEqual(s.state().settings.model, 'claude-opus-5-5');
});

test('settings: invalid values and an ended session are refused, nothing is sent', () => {
  const { s, writes } = newSession();
  assert.throws(() => s.changeSettings('alice', 'gpt-5'), /model must be/);
  assert.throws(() => s.changeSettings('alice', undefined, 'ultra'), /effort must be/);
  assert.throws(() => s.changeSettings('alice'), /model or effort/);
  assert.throws(() => s.changeSettings('', 'opus'), /author/);
  s.end('test');
  assert.throws(() => s.changeSettings('alice', 'opus'), /ended/);
  assert.strictEqual(writes.length, 0);
});

test('settings: POST /settings, and a reconnect (GET /events, /state) sees the applied setting', async () => {
  const { s, writes } = newSession();
  const srv = http.createServer(handler(s, { tokenSha256: SHA, localAddresses: new Set(['10.9.9.9']) }));
  await new Promise((r) => srv.listen(0, '127.0.0.1', r));
  const port = srv.address().port;
  const call = (method, p, body) => new Promise((resolve, reject) => {
    const req = http.request({ host: '127.0.0.1', port, method, path: p, headers: { Authorization: `Bearer ${TOKEN}` } }, (res) => {
      let b = ''; res.on('data', (c) => { b += c; }); res.on('end', () => resolve({ code: res.statusCode, body: JSON.parse(b) }));
    });
    req.on('error', reject); if (body) req.write(JSON.stringify(body)); req.end();
  });
  assert.strictEqual((await call('POST', '/settings', { author: 'alice', model: 'nope' })).code, 400);
  assert.strictEqual((await call('POST', '/settings', { author: 'alice', effort: 'max' })).code, 200);
  s.onClaude(JSON.stringify({ type: 'control_response', response: { subtype: 'success', request_id: writes[0].request_id, response: {} } }));
  const ev = await call('GET', '/events?after=0');
  assert.strictEqual(ev.body.state.settings.effort, 'max');
  assert.deepStrictEqual(ev.body.events.filter((e) => e.type === 'settings').map((e) => e.data.status), ['requested', 'applied']);
  assert.strictEqual((await call('GET', '/state')).body.settings.effort, 'max');
  srv.close();
});
