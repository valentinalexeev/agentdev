#!/usr/bin/env node
// Fake Anthropic Messages API for the compatibility e2e (#178, ADR 0006). No model, no credential, no dependencies.
// claude-proxy forwards to it (UPSTREAM=http://claude-stub...:8080, ALLOW_HTTP_UPSTREAM=true), so the real Claude
// Code CLI in the worker image, the proxy key and the proxy usage metering all run as in production.
//
// Script of the fake model, per request to /v1/messages:
//   - the prompt asks "Reply with exactly: OK" (readiness check), or the request has no Write tool -> text "OK"
//   - the last user turn holds a tool_result                    -> final text "Stub change done." + "STATUS: done"
//   - else                                                       -> tool_use Write <cwd>/STUB_CHANGE.md
// Every response has a usage block, so the proxy prints its AGENTDEV_USAGE line. GET /stats gives the counters.
'use strict';
const http = require('http');

const stats = { messages: 0, toolUse: 0, final: 0, plain: 0, other: 0 };
let seq = 0;

const text = (c) => (typeof c === 'string' ? c : Array.isArray(c) ? c.map((b) => (b && b.type === 'text' ? b.text : '')).join('\n') : '');
const systemText = (s) => text(s);

// decide returns the content blocks of the answer.
function decide(req) {
  const msgs = Array.isArray(req.messages) ? req.messages : [];
  const allText = msgs.map((x) => text(x && x.content)).join('\n');
  const tools = Array.isArray(req.tools) ? req.tools.map((t) => t && t.name) : [];
  // one tool call per conversation: after any tool_result the model finishes
  const hasResult = msgs.some((x) => x && Array.isArray(x.content) && x.content.some((b) => b && b.type === 'tool_result'));
  if (hasResult) { stats.final++; return [{ type: 'text', text: 'Stub change done: added STUB_CHANGE.md.\n\nSTATUS: done' }]; }
  if (/Reply with exactly: OK/.test(allText) || !tools.includes('Write')) { stats.plain++; return [{ type: 'text', text: 'OK' }]; }
  const m = /working directory:\s*(\S+)/i.exec(systemText(req.system)) || /working directory:\s*(\S+)/i.exec(allText);
  const cwd = m ? m[1] : '/tmp';
  stats.toolUse++;
  return [
    { type: 'text', text: 'I will add a file.' },
    { type: 'tool_use', id: `toolu_stub_${++seq}`, name: 'Write',
      input: { file_path: `${cwd}/STUB_CHANGE.md`, content: `Change by the agentdev compatibility stub (${new Date().toISOString()}).\n` } },
  ];
}

const usage = { input_tokens: 10, output_tokens: 5, cache_creation_input_tokens: 0, cache_read_input_tokens: 0 };

function sse(res, model, blocks) {
  res.writeHead(200, { 'content-type': 'text/event-stream', 'cache-control': 'no-cache' });
  const ev = (type, data) => res.write(`event: ${type}\ndata: ${JSON.stringify({ type, ...data })}\n\n`);
  const id = `msg_stub_${++seq}`;
  ev('message_start', { message: { id, type: 'message', role: 'assistant', model, content: [], stop_reason: null, stop_sequence: null, usage: { ...usage, output_tokens: 1 } } });
  blocks.forEach((b, index) => {
    if (b.type === 'text') {
      ev('content_block_start', { index, content_block: { type: 'text', text: '' } });
      ev('content_block_delta', { index, delta: { type: 'text_delta', text: b.text } });
    } else {
      ev('content_block_start', { index, content_block: { type: 'tool_use', id: b.id, name: b.name, input: {} } });
      ev('content_block_delta', { index, delta: { type: 'input_json_delta', partial_json: JSON.stringify(b.input) } });
    }
    ev('content_block_stop', { index });
  });
  const stop = blocks.some((b) => b.type === 'tool_use') ? 'tool_use' : 'end_turn';
  ev('message_delta', { delta: { stop_reason: stop, stop_sequence: null }, usage: { output_tokens: usage.output_tokens } });
  ev('message_stop', {});
  res.end();
}

function json(res, code, body) { res.writeHead(code, { 'content-type': 'application/json' }); res.end(JSON.stringify(body)); }

const server = http.createServer((req, res) => {
  const path = req.url.split('?')[0];
  if (req.method === 'GET' && path === '/healthz') return json(res, 200, { ok: true });
  if (req.method === 'GET' && path === '/stats') return json(res, 200, stats);
  let body = '';
  req.on('data', (c) => { if (body.length < 8e6) body += c; });
  req.on('end', () => {
    if (req.method === 'POST' && path === '/v1/messages/count_tokens') return json(res, 200, { input_tokens: 10 });
    if (req.method !== 'POST' || path !== '/v1/messages') { stats.other++; return json(res, 200, {}); }
    let r; try { r = JSON.parse(body); } catch { return json(res, 400, { type: 'error', error: { type: 'invalid_request_error', message: 'bad json' } }); }
    stats.messages++;
    const blocks = decide(r);
    const model = r.model || 'claude-stub';
    if (r.stream) return sse(res, model, blocks);
    const stop = blocks.some((b) => b.type === 'tool_use') ? 'tool_use' : 'end_turn';
    return json(res, 200, { id: `msg_stub_${++seq}`, type: 'message', role: 'assistant', model, content: blocks, stop_reason: stop, stop_sequence: null, usage });
  });
});

if (require.main === module) {
  const port = Number(process.env.PORT || 8080);
  server.listen(port, () => console.log(`claude-stub listening on :${port}`));
  process.on('SIGTERM', () => server.close(() => process.exit(0)));
}
module.exports = { decide, server, stats };
