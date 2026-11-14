#!/usr/bin/env node
// chat-session-server (#346): the session server of a console chat session. Runs in the chat pod
// (manifests/chat.yaml.tmpl, API-started Jobs) as the container command, through agent-chat-session.
//
// It runs Claude Code with --input-format stream-json --output-format stream-json and the stdio permission prompt,
// keeps an append-only event log (JSON lines in /state/chat/events.jsonl) and serves an internal HTTP API that only
// agentdev-api may call:
//   - NetworkPolicy chat-session-server: ingress to port 7681 only from the API pod;
//   - a bearer token: the API derives it per session (HMAC); the pod only has its SHA-256 (CHAT_CONTROL_SHA256), so
//     the model cannot learn it from the environment;
//   - requests from an address of this pod are refused, so a command in the pod cannot answer its own permission
//     request through the pod IP or localhost.
//
// Routes (all JSON, bearer token):
//   GET  /events?after=N&limit=M   events with seq > N (max 500) and the state
//   GET  /state                    state only
//   POST /messages                 {"author","text"}: a person's message, written to Claude's stdin
//   POST /permissions/{id}         {"author","behavior":"allow"|"deny"}: answer a pending permission request (once)
//   POST /presence                 {"viewers":["name",...]}: who has the session open in the console (from the API)
//   POST /end                      {"author"}: end the session now
//
// Issue drafts: the model writes a fenced ```agentdev-draft block with JSON {title, what, acceptance, decisions}.
// A valid block becomes a "draft" event; anything else is dropped. The server never creates issues and holds no forge
// token (the clone runs in the initContainer).
//
// `chat-session-server render` prints the log as text and follows it: the tmux view (Terminal) of the same session.
'use strict';
const http = require('http');
const fs = require('fs');
const os = require('os');
const path = require('path');
const crypto = require('crypto');
const { spawn } = require('child_process');

const LOG_DIR = process.env.CHAT_LOG_DIR || '/state/chat';
const LOG_FILE = path.join(LOG_DIR, 'events.jsonl');
const MAX_TEXT = 64 * 1024; // one message from a person
const MAX_OUT = 16 * 1024; // tool output kept per event
const MAX_EVENTS_PAGE = 500;

// ---------- pure helpers (exported for tests) ----------

const DRAFT_RE = /```agentdev-draft[ \t]*\n([\s\S]*?)\n```/g;

function clip(s, n) {
  s = typeof s === 'string' ? s : JSON.stringify(s ?? '');
  return s.length > n ? s.slice(0, n) + `\n[... ${s.length - n} more characters]` : s;
}

// parseDrafts returns the valid drafts and the text with the draft blocks removed.
function parseDrafts(text) {
  const drafts = [];
  const rest = text.replace(DRAFT_RE, (_, body) => {
    let d;
    try { d = JSON.parse(body); } catch { return ''; }
    const str = (v, n) => (typeof v === 'string' && v.trim() !== '' && v.length <= n ? v.trim() : null);
    const title = str(d && d.title, 200);
    const what = str(d && d.what, 20000);
    const acceptance = str(d && d.acceptance, 20000);
    const decisions = d && d.decisions === undefined ? '' : str(d && d.decisions, 20000);
    if (!title || !what || !acceptance || decisions === null || /[\r\n]/.test(title)) return '';
    drafts.push({ title, what, acceptance, decisions: decisions || '' });
    return '';
  });
  return { drafts, text: rest.trim() };
}

// tokenOK compares sha256(token) with the expected hex digest in constant time.
function tokenOK(header, wantHex) {
  if (!wantHex || !/^[0-9a-f]{64}$/.test(wantHex)) return false;
  const m = /^Bearer (\S+)$/.exec(header || '');
  if (!m) return false;
  const got = crypto.createHash('sha256').update(m[1]).digest();
  return crypto.timingSafeEqual(got, Buffer.from(wantHex, 'hex'));
}

function localAddresses() {
  const out = new Set(['127.0.0.1', '::1']);
  for (const list of Object.values(os.networkInterfaces())) for (const a of list || []) out.add(a.address);
  return out;
}

function normAddr(a) { return (a || '').replace(/^::ffff:/, ''); }

const NAME_RE = /^[A-Za-z0-9][A-Za-z0-9._@-]{0,99}$/;

// ---------- the session ----------

class Session {
  constructor(opts) {
    this.opts = opts;
    this.events = [];
    this.seq = 0;
    this.pending = new Map(); // permission id -> {request_id, input}
    this.viewers = [];
    this.lastViewer = Date.now();
    // lastConsole: last time the API reported a console viewer. Only the API can write that. Terminal (tmux) clients
    // run in the same container and UID as Claude, so the model could fake one; they extend the session for at most
    // terminalMaxSeconds after the last console viewer (#389 review).
    this.lastConsole = this.lastViewer;
    this.ended = null;
    this.tokens = { input: 0, output: 0 };
    this.drafts = 0;
    this.perms = 0;
    this.claude = null;
    this.write = opts.write || ((line) => this.claude && this.claude.stdin.write(line + '\n'));
    fs.mkdirSync(LOG_DIR, { recursive: true });
    this.logFd = opts.noLog ? null : fs.openSync(LOG_FILE, 'a', 0o600);
  }

  append(type, data, author) {
    const ev = { seq: ++this.seq, time: new Date().toISOString(), type, data };
    if (author) ev.author = author;
    this.events.push(ev);
    if (this.logFd !== null) fs.writeSync(this.logFd, JSON.stringify(ev) + '\n');
    return ev;
  }

  state() {
    const idle = this.opts.idleSeconds * 1000;
    return {
      session: this.opts.name, repo: this.opts.repo, agent: this.opts.agent,
      viewers: this.viewers, tokens: this.tokens, lastSeq: this.seq,
      pendingPermissions: [...this.pending.keys()],
      ended: this.ended,
      idleEndsAt: this.ended || this.viewerCount() > 0 ? null : new Date(this.lastViewer + idle).toISOString(),
    };
  }

  terminalMaxMs() { return (this.opts.terminalMaxSeconds ?? 7200) * 1000; }

  // viewerCount counts console viewers (from the API) and terminal clients, the latter only while they are inside
  // the bounded window after the last console viewer.
  viewerCount(nowMs = Date.now()) {
    if (this.viewers.length > 0) return this.viewers.length;
    if (nowMs - this.lastConsole >= this.terminalMaxMs()) return 0;
    return this.opts.tmuxClients ? this.opts.tmuxClients() : 0;
  }

  // onClaude handles one stream-json line from Claude Code's stdout.
  onClaude(line) {
    let m;
    try { m = JSON.parse(line); } catch { return; }
    if (!m || typeof m !== 'object') return;
    switch (m.type) {
      case 'system':
        if (m.subtype === 'init') this.append('ready', { model: m.model || '', tools: (m.tools || []).length });
        break;
      case 'assistant':
        for (const c of (m.message && m.message.content) || []) {
          if (c.type === 'text') {
            const { drafts, text } = parseDrafts(c.text || '');
            if (text) this.append('assistant', { text: clip(text, MAX_OUT * 4) });
            for (const d of drafts) this.append('draft', { id: `d${++this.drafts}`, ...d });
          } else if (c.type === 'tool_use') {
            this.append('tool', { id: c.id || '', name: String(c.name || ''), input: clip(c.input, 4096) });
          }
        }
        break;
      case 'user': // tool results come back as user messages
        for (const c of (m.message && m.message.content) || []) {
          if (c && c.type === 'tool_result') {
            const out = Array.isArray(c.content) ? c.content.map((x) => x.text || '').join('\n') : c.content;
            this.append('tool-result', { id: c.tool_use_id || '', error: !!c.is_error, output: clip(out, MAX_OUT), untrusted: true });
          }
        }
        break;
      case 'result': {
        const u = m.usage || {};
        this.tokens.input += (u.input_tokens || 0) + (u.cache_read_input_tokens || 0) + (u.cache_creation_input_tokens || 0);
        this.tokens.output += u.output_tokens || 0;
        this.append('turn', { error: !!m.is_error, tokens: { ...this.tokens } });
        break;
      }
      case 'control_request': {
        const r = m.request || {};
        if (r.subtype !== 'can_use_tool' || typeof m.request_id !== 'string') {
          // anything else we do not support: answer with an error so Claude does not hang
          this.write(JSON.stringify({ type: 'control_response', response: { subtype: 'error', request_id: m.request_id, error: 'not supported' } }));
          return;
        }
        const id = `p${++this.perms}`;
        this.pending.set(id, { request_id: m.request_id, input: r.input || {} });
        this.append('permission', { id, tool: String(r.tool_name || ''), input: clip(r.input, 4096) });
        break;
      }
      default:
    }
  }

  message(author, text) {
    if (this.ended) throw httpError(409, 'the session has ended');
    if (!NAME_RE.test(author || '')) throw httpError(400, 'author is required');
    if (typeof text !== 'string' || !text.trim() || text.length > MAX_TEXT) throw httpError(400, `text must be 1 to ${MAX_TEXT} characters`);
    const ev = this.append('user', { text }, author);
    // the author is part of the text Claude sees, so it knows who speaks in a shared session
    this.write(JSON.stringify({ type: 'user', message: { role: 'user', content: `[${author}] ${text}` } }));
    return ev;
  }

  answer(id, author, behavior) {
    if (!NAME_RE.test(author || '')) throw httpError(400, 'author is required');
    if (behavior !== 'allow' && behavior !== 'deny') throw httpError(400, 'behavior must be allow or deny');
    const p = this.pending.get(id);
    if (!p) throw httpError(404, 'no pending permission request with this id');
    this.pending.delete(id);
    const response = behavior === 'allow'
      ? { behavior: 'allow', updatedInput: p.input }
      : { behavior: 'deny', message: `${author} denied this in the console` };
    this.write(JSON.stringify({ type: 'control_response', response: { subtype: 'success', request_id: p.request_id, response } }));
    return this.append('permission-answer', { id, behavior }, author);
  }

  presence(viewers) {
    if (!Array.isArray(viewers) || viewers.length > 100 || !viewers.every((v) => NAME_RE.test(v))) throw httpError(400, 'viewers must be a list of names');
    const changed = JSON.stringify(viewers) !== JSON.stringify(this.viewers);
    this.viewers = [...viewers];
    if (this.viewers.length > 0) this.lastConsole = Date.now();
    if (this.viewerCount() > 0) this.lastViewer = Date.now();
    if (changed) this.append('presence', { viewers: this.viewers });
  }

  // tick checks the idle limit; returns true when the session should end.
  tick(nowMs) {
    if (this.ended) return false;
    if (this.viewers.length > 0) this.lastConsole = nowMs;
    if (this.viewerCount(nowMs) > 0) { this.lastViewer = nowMs; return false; }
    return nowMs - this.lastViewer >= this.opts.idleSeconds * 1000;
  }

  end(reason, author) {
    if (this.ended) return;
    this.ended = reason;
    for (const [id] of this.pending) this.append('permission-answer', { id, behavior: 'deny', reason: 'session ended' });
    this.pending.clear();
    this.append('end', { reason }, author);
  }
}

function httpError(code, msg) { const e = new Error(msg); e.code = code; return e; }

function readJSON(req) {
  return new Promise((resolve, reject) => {
    let n = 0; const chunks = [];
    req.on('data', (c) => { n += c.length; if (n > MAX_TEXT * 2) { reject(httpError(413, 'body too large')); req.destroy(); } else chunks.push(c); });
    req.on('end', () => { try { const v = JSON.parse(Buffer.concat(chunks).toString('utf8') || 'null'); if (!v || typeof v !== 'object' || Array.isArray(v)) throw 0; resolve(v); } catch { reject(httpError(400, 'body must be a JSON object')); } });
    req.on('error', reject);
  });
}

function handler(s, opts) {
  const locals = opts.localAddresses || localAddresses();
  return async (req, res) => {
    const send = (code, v) => { res.writeHead(code, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' }); res.end(JSON.stringify(v)); };
    try {
      if (req.url === '/healthz') return send(200, { ok: true });
      if (locals.has(normAddr(req.socket.remoteAddress))) return send(403, { error: 'not from inside the session pod' });
      if (!tokenOK(req.headers.authorization, opts.tokenSha256)) return send(401, { error: 'bad token' });
      const u = new URL(req.url, 'http://x');
      if (req.method === 'GET' && u.pathname === '/state') return send(200, s.state());
      if (req.method === 'GET' && u.pathname === '/events') {
        const after = /^[0-9]{1,12}$/.test(u.searchParams.get('after') || '0') ? Number(u.searchParams.get('after') || 0) : -1;
        const lim = /^[0-9]{1,4}$/.test(u.searchParams.get('limit') || '') ? Math.min(Number(u.searchParams.get('limit')), MAX_EVENTS_PAGE) : MAX_EVENTS_PAGE;
        if (after < 0) return send(400, { error: 'after must be a number' });
        return send(200, { events: s.events.filter((e) => e.seq > after).slice(0, lim), state: s.state() });
      }
      if (req.method !== 'POST') return send(404, { error: 'not found' });
      const body = await readJSON(req);
      if (u.pathname === '/messages') return send(200, s.message(body.author, body.text));
      const pm = /^\/permissions\/(p[0-9]{1,9})$/.exec(u.pathname);
      if (pm) return send(200, s.answer(pm[1], body.author, body.behavior));
      if (u.pathname === '/presence') { s.presence(body.viewers); return send(200, s.state()); }
      if (u.pathname === '/end') {
        if (!NAME_RE.test(body.author || '')) throw httpError(400, 'author is required');
        s.end('ended by a person', body.author); send(200, s.state()); if (opts.onEnd) opts.onEnd(); return;
      }
      return send(404, { error: 'not found' });
    } catch (e) {
      send(e.code && e.code >= 400 && e.code < 600 ? e.code : 500, { error: e.code ? e.message : 'internal error' });
    }
  };
}

// ---------- render: the tmux view ----------
// The log file is written by this server, but Claude runs as the same UID and could append fake lines (for example a
// fake "? permission" line). The terminal view is display only: it cannot answer permissions or send messages, and
// the console reads events from this server's memory, not from the file. Treat the terminal text as untrusted.

function render(file) {
  let pos = 0; let buf = '';
  const line = (e) => {
    const t = e.time.slice(11, 19); const d = e.data || {};
    switch (e.type) {
      case 'user': return `${t} ${e.author}> ${d.text}`;
      case 'assistant': return `${t} claude: ${d.text}`;
      case 'tool': return `${t}   [${d.name}] ${String(d.input).slice(0, 200)}`;
      case 'tool-result': return `${t}   -> ${String(d.output).split('\n')[0].slice(0, 200)}`;
      case 'permission': return `${t} ? permission ${d.id}: ${d.tool} ${String(d.input).slice(0, 200)} (answer in the console)`;
      case 'permission-answer': return `${t} ${d.behavior} ${d.id} by ${e.author || 'system'}`;
      case 'draft': return `${t} issue draft ${d.id}: ${d.title}`;
      case 'presence': return `${t} here: ${(d.viewers || []).join(', ') || 'nobody'}`;
      case 'end': return `${t} session ended: ${d.reason}`;
      default: return null;
    }
  };
  const poll = () => {
    let st; try { st = fs.statSync(file); } catch { return; }
    if (st.size <= pos) return;
    const fd = fs.openSync(file, 'r'); const b = Buffer.alloc(st.size - pos);
    fs.readSync(fd, b, 0, b.length, pos); fs.closeSync(fd); pos = st.size;
    buf += b.toString('utf8'); const parts = buf.split('\n'); buf = parts.pop();
    // eslint-disable-next-line no-control-regex
    for (const p of parts) { try { const l = line(JSON.parse(p)); if (l) process.stdout.write(l.replace(/[\x00-\x08\x0b-\x1f\x7f]/g, '') + '\n'); } catch { /* skip */ } }
  };
  setInterval(poll, 500); poll();
}

// ---------- main ----------

function main() {
  if (process.argv[2] === 'render') return render(LOG_FILE);
  const idle = /^[0-9]{1,6}$/.test(process.env.IDLE_SECONDS || '') ? Number(process.env.IDLE_SECONDS) : 1800;
  const tokenSha256 = process.env.CHAT_CONTROL_SHA256 || '';
  if (!/^[0-9a-f]{64}$/.test(tokenSha256)) { console.error('CHAT_CONTROL_SHA256 must be a sha256 hex digest'); process.exit(2); }
  delete process.env.CHAT_CONTROL_SHA256;
  const promptFile = process.env.CHAT_PROMPT_FILE || '/state/session-prompt.md';
  const tmuxClients = () => {
    try { return require('child_process').execFileSync('tmux', ['list-clients', '-t', 'chat'], { stdio: ['ignore', 'pipe', 'ignore'] }).toString().trim().split('\n').filter(Boolean).length; } catch { return 0; }
  };
  const terminalMax = /^[0-9]{1,6}$/.test(process.env.CHAT_TERMINAL_MAX_SECONDS || '') ? Number(process.env.CHAT_TERMINAL_MAX_SECONDS) : 7200;
  const s = new Session({ name: process.env.SESSION_NAME || os.hostname(), repo: process.env.REPO || '', agent: process.env.CHAT_AGENT || '', idleSeconds: idle, terminalMaxSeconds: terminalMax, tmuxClients });
  const args = ['-p', '--input-format', 'stream-json', '--output-format', 'stream-json', '--verbose',
    '--permission-prompt-tool', 'stdio', '--append-system-prompt', fs.readFileSync(promptFile, 'utf8')];
  const env = { ...process.env };
  const claude = spawn('claude', args, { cwd: process.env.CHAT_WORKDIR || '/state/repo', env, stdio: ['pipe', 'pipe', 'inherit'] });
  s.claude = claude;
  s.append('start', { repo: s.opts.repo, agent: s.opts.agent, idleSeconds: idle });
  let buf = '';
  claude.stdout.on('data', (c) => { buf += c.toString('utf8'); let i; while ((i = buf.indexOf('\n')) >= 0) { const l = buf.slice(0, i); buf = buf.slice(i + 1); if (l.trim()) s.onClaude(l); } });
  let server;
  const finish = (reason) => { s.end(reason); try { claude.stdin.end(); claude.kill('SIGTERM'); } catch { /* gone */ } setTimeout(() => process.exit(0), 3000); };
  claude.on('exit', (code) => { if (!s.ended) s.end(`claude exited (${code})`); setTimeout(() => process.exit(0), 3000); });
  server = http.createServer(handler(s, { tokenSha256, onEnd: () => finish('ended by a person') }));
  server.requestTimeout = 30000;
  server.listen(Number(process.env.CHAT_PORT || 7681), () => console.log(`chat-session-server on :${process.env.CHAT_PORT || 7681}, idle ${idle}s`));
  setInterval(() => { if (s.tick(Date.now())) finish(`nobody here for ${Math.round(idle / 60)} min`); }, 10000);
  process.on('SIGTERM', () => finish('stopped'));
}

if (require.main === module) main();
module.exports = { Session, parseDrafts, tokenOK, handler, clip };
