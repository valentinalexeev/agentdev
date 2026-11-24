#!/usr/bin/env node
// permission-host (#539, #503 step 1): runs Claude Code for agent-loop with the can_use_tool host protocol, so a
// permission prompt reaches the forge sidecar with Claude Code's own context (decision_reason, decision_reason_type,
// blocked_path, permission_suggestions, description), not only the tool name and input of the MCP prompt tool.
//
//   permission-host <prompt> <claude> [claude args...]
//
// The loop gives the claude command without a prompt; this host adds `-p --input-format stream-json
// --permission-prompt-tool stdio`, writes the prompt as the first user message, passes Claude's stdout through
// unchanged (stream-json, one event per line: the loop's parser and run log stay the same) and answers each
// `control_request` itself:
//   - subtype can_use_tool: asks the forge sidecar, POST {FORGE_API}/mcp tools/call approve with every field of the
//     request and the job header, the same endpoint and decision rules as the MCP prompt tool (agents never decide, a
//     decision binds to the tool and input, job-scoped allow). The answer is passed back as Claude Code's response.
//   - any other subtype: an error response, so Claude does not wait.
// Fails closed: a sidecar error, an unreadable answer or anything but behavior allow/deny is a deny. The fields from
// Claude Code are only forwarded; the sidecar treats them as untrusted text (images/forge/approvals.go).
// stdin is closed after the first `result` event, which ends the run; the exit code is Claude's.
// Verified against Claude Code 2.1.328 and 2.1.312, the pinned version (control_request/control_response envelopes,
// can_use_tool fields,
// behavior allow {updatedInput} | deny {message}).
'use strict';
const { spawn } = require('child_process');
const http = require('http');
const os = require('os');
const { StringDecoder } = require('string_decoder');

// MAX_LINE bounds one stdout line kept for parsing (a can_use_tool request with a large input stays far below it). A
// longer line cannot be read, so a request in it could not be answered: the run fails closed (#540 review).
const MAX_LINE = /^[0-9]{3,9}$/.test(process.env.PERMISSION_HOST_MAX_LINE || '') ? Number(process.env.PERMISSION_HOST_MAX_LINE) : 16 << 20;

const FORWARD = ['tool_name', 'input', 'tool_use_id', 'description', 'decision_reason', 'decision_reason_type',
  'blocked_path', 'permission_suggestions'];

function deny(message) { return { behavior: 'deny', message }; }

// decide asks the sidecar; resolves to a can_use_tool response, never rejects.
function decide(req, { forgeAPI, job, timeoutMs }) {
  const args = {};
  for (const k of FORWARD) if (req[k] !== undefined) args[k] = req[k];
  if (typeof args.tool_name !== 'string' || !args.tool_name) return Promise.resolve(deny('no tool name'));
  if (args.input === null || typeof args.input !== 'object') args.input = {};
  const body = JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'tools/call', params: { name: 'approve', arguments: args } });
  return new Promise((resolve) => {
    let u;
    try { u = new URL('/mcp', forgeAPI); } catch { resolve(deny('approval request failed: bad FORGE_API')); return; }
    const r = http.request(u, { method: 'POST', timeout: timeoutMs, headers: { 'Content-Type': 'application/json',
      'Content-Length': Buffer.byteLength(body), ...(job ? { 'X-Agentdev-Job': job } : {}) } }, (res) => {
      let b = '';
      res.setEncoding('utf8');
      res.on('data', (c) => { if (b.length < 1 << 20) b += c; });
      res.on('end', () => resolve(parseAnswer(b, args.input)));
    });
    r.on('timeout', () => r.destroy(new Error('timeout')));
    r.on('error', (e) => resolve(deny(`approval request failed: ${e.message}`)));
    r.end(body);
  });
}

// parseAnswer reads the MCP tools/call result: content[0].text is {"behavior":"allow","updatedInput":...} or deny.
function parseAnswer(text, input) {
  try {
    const m = JSON.parse(text);
    const out = JSON.parse(m.result.content[0].text);
    if (out.behavior === 'allow') {
      const ui = out.updatedInput && typeof out.updatedInput === 'object' ? out.updatedInput : input;
      return { behavior: 'allow', updatedInput: ui };
    }
    if (out.behavior === 'deny') return deny(typeof out.message === 'string' && out.message ? out.message : 'denied');
  } catch { /* fall through */ }
  return deny('approval answer not readable: denied');
}

function main() {
  const [prompt, cmd, ...rest] = process.argv.slice(2);
  if (typeof prompt !== 'string' || !cmd) { console.error('usage: permission-host <prompt> <claude> [args...]'); process.exit(2); }
  const opts = { forgeAPI: process.env.FORGE_API || 'http://127.0.0.1:7070', job: process.env.JOB || '',
    timeoutMs: 26 * 60 * 60 * 1000 }; // longer than any approval timeout (max 24 h): the sidecar ends the wait
  const args = ['-p', ...rest, '--input-format', 'stream-json', '--permission-prompt-tool', 'stdio'];
  const claude = spawn(cmd, args, { stdio: ['pipe', 'pipe', 'inherit'] });
  const write = (o) => { if (!claude.stdin.destroyed && claude.stdin.writable) claude.stdin.write(JSON.stringify(o) + '\n'); };
  claude.stdin.on('error', () => { /* claude exited */ });
  write({ type: 'user', message: { role: 'user', content: prompt }, parent_tool_use_id: null });
  let buf = '';
  let done = false;
  let failed = false;
  const dec = new StringDecoder('utf8'); // a character split across chunks is joined, not mangled (#540 review)
  const onLine = (line) => {
    let m;
    try { m = JSON.parse(line); } catch { return; }
    if (!m || typeof m !== 'object') return;
    if (m.type === 'control_request') {
      const id = m.request_id;
      const r = m.request || {};
      if (typeof id !== 'string') return;
      if (r.subtype !== 'can_use_tool') {
        write({ type: 'control_response', response: { subtype: 'error', request_id: id, error: 'not supported by agentdev' } });
        return;
      }
      decide(r, opts).then((response) => write({ type: 'control_response', response: { subtype: 'success', request_id: id, response } }));
    } else if (m.type === 'result' && !done) {
      done = true;
      claude.stdin.end();
    }
  };
  claude.stdout.on('data', (c) => {
    process.stdout.write(c); // the raw bytes, unchanged
    if (failed) return;
    buf += dec.write(c);
    let i;
    while ((i = buf.indexOf('\n')) >= 0) { const l = buf.slice(0, i); buf = buf.slice(i + 1); if (l.trim()) onLine(l); }
    if (buf.length > MAX_LINE) {
      failed = true; buf = '';
      console.error(`permission-host: a line of Claude's output is over ${MAX_LINE} bytes; stopping the run (fail closed)`);
      claude.kill('SIGTERM');
    }
  });
  for (const s of ['SIGTERM', 'SIGINT']) process.on(s, () => claude.kill(s));
  claude.on('error', (e) => { console.error(`permission-host: ${e.message}`); process.exit(127); });
  // 'close', not 'exit': stdout is fully read and passed on; then flush our stdout before exiting (#540 review)
  claude.on('close', (code, sig) => {
    let rc = code;
    if (rc === null) rc = sig && os.constants.signals[sig] ? 128 + os.constants.signals[sig] : 1;
    if (failed && rc === 0) rc = 1;
    process.stdout.write('', () => process.exit(rc));
  });
}

if (require.main === module) main();
module.exports = { decide, parseAnswer, FORWARD };
