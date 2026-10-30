// webhook-router: the single webhook endpoint of an agentdev cluster (#3).
//
// Forges (Gitea now, a GitHub App later) send every webhook here. The router
//   1. verifies the forge signature once (Gitea X-Gitea-Signature, GitHub X-Hub-Signature-256),
//   2. reads only routing facts from the payload: repo and the people involved
//      (issue/PR assignees, PR author, requested reviewers),
//   3. forwards a bare "wake" to every registered agent that works that repo as one of those people.
// Agents register themselves (POST /register) with their forward URL and refresh it; stale entries expire.
// The payload never reaches an agent: agents re-read state from the forge API, so a bad route costs one poll.
//
// Env: PORT (8080), FORGE_SECRETS ("gitea=<secret>,github=<secret>"), REGISTER_TOKEN (agents -> router),
//      REGISTRATION_TTL_SECONDS (300).
'use strict';
const http = require('node:http');
const crypto = require('node:crypto');

const port = Number(process.env.PORT || 8080);
const ttlMs = Number(process.env.REGISTRATION_TTL_SECONDS || 300) * 1000;
const registerToken = process.env.REGISTER_TOKEN || '';
const forgeSecrets = Object.fromEntries((process.env.FORGE_SECRETS || '').split(',').filter(Boolean)
  .map((kv) => { const i = kv.indexOf('='); return [kv.slice(0, i), kv.slice(i + 1)]; }));

const log = (m) => console.log(`${new Date().toISOString().slice(11, 19)} router: ${m}`);
const stats = { delivered: 0, rejected: 0, unrouted: 0, forwarded: 0, forwardErrors: 0 };

// agent name -> { agent, forge, user, repos: Set, url, token, seen }
const registry = new Map();

const safeEqual = (a, b) => a.length === b.length && crypto.timingSafeEqual(Buffer.from(a), Buffer.from(b));
const hmac = (secret, body) => crypto.createHmac('sha256', secret).update(body).digest('hex');

function detectForge(h) {
  if (h['x-gitea-event']) return { forge: 'gitea', event: h['x-gitea-event'], sig: h['x-gitea-signature'] };
  if (h['x-github-event']) return { forge: 'github', event: h['x-github-event'], sig: (h['x-hub-signature-256'] || '').replace(/^sha256=/, '') };
  return null;
}

// Who is this event about? Logins of everyone an agent could be acting as.
function people(p) {
  const out = new Set();
  const add = (u) => { if (u && u.login) out.add(u.login); };
  const item = p.issue || p.pull_request || {};
  (item.assignees || []).forEach(add); add(item.assignee); add(item.user);
  (item.requested_reviewers || []).forEach(add);
  add(p.assignee); add(p.requested_reviewer);
  return out;
}

function live() {
  const now = Date.now();
  for (const [k, r] of registry) if (now - r.seen > ttlMs) { registry.delete(k); log(`expired ${k}`); }
  return [...registry.values()];
}

function forward(r, info) {
  const u = new URL(r.url);
  const body = JSON.stringify(info);
  const req = http.request({ hostname: u.hostname, port: u.port || 80, path: u.pathname, method: 'POST', timeout: 3000,
    headers: { 'content-type': 'application/json', 'content-length': Buffer.byteLength(body), authorization: `Bearer ${r.token}` } },
    (res) => { res.resume(); if (res.statusCode >= 300) { stats.forwardErrors++; log(`forward to ${r.agent}: HTTP ${res.statusCode}`); } });
  req.on('error', (e) => { stats.forwardErrors++; log(`forward to ${r.agent} failed: ${e.message}`); });
  req.on('timeout', () => req.destroy(new Error('timeout')));
  req.end(body);
  stats.forwarded++;
}

function readBody(req, limit, cb) {
  const chunks = []; let size = 0;
  req.on('data', (c) => { size += c.length; if (size > limit) req.destroy(); else chunks.push(c); });
  req.on('end', () => cb(Buffer.concat(chunks)));
}

const json = (res, code, obj) => { res.writeHead(code, { 'content-type': 'application/json' }).end(JSON.stringify(obj)); };

http.createServer((req, res) => {
  if (req.method === 'GET' && req.url === '/healthz') return json(res, 200, { ok: true });
  if (req.method === 'GET' && req.url === '/status') {
    return json(res, 200, { stats, agents: live().map(({ token, ...r }) => ({ ...r, repos: [...r.repos], ageSeconds: Math.round((Date.now() - r.seen) / 1000) })) });
  }

  if (req.method === 'POST' && req.url === '/register') {
    return readBody(req, 64e3, (body) => {
      if (!registerToken || !safeEqual(req.headers.authorization || '', `Bearer ${registerToken}`)) return json(res, 401, { error: 'unauthorized' });
      let r; try { r = JSON.parse(body); } catch { return json(res, 400, { error: 'bad json' }); }
      if (!r.agent || !r.user || !r.url || !r.token || !Array.isArray(r.repos)) return json(res, 400, { error: 'agent, user, url, token, repos required' });
      const isNew = !registry.has(r.agent);
      registry.set(r.agent, { agent: r.agent, forge: r.forge || 'gitea', user: r.user, repos: new Set(r.repos), url: r.url, token: r.token, seen: Date.now() });
      if (isNew) log(`registered ${r.agent} (${r.forge || 'gitea'}:${r.user}) for ${r.repos.join(',')} -> ${r.url}`);
      return json(res, 200, { ok: true, ttlSeconds: ttlMs / 1000 });
    });
  }

  if (req.method === 'POST' && (req.url === '/' || req.url.startsWith('/hook'))) {
    return readBody(req, 5e6, (body) => {
      const f = detectForge(req.headers);
      const secret = f && forgeSecrets[f.forge];
      if (!f || !secret || !f.sig || !safeEqual(f.sig, hmac(secret, body))) { stats.rejected++; log('rejected delivery (signature)'); return json(res, 401, { error: 'bad signature' }); }
      stats.delivered++;
      let p; try { p = JSON.parse(body); } catch { return json(res, 400, { error: 'bad json' }); }
      const repo = p.repository && p.repository.full_name;
      const who = people(p);
      const targets = live().filter((r) => r.forge === f.forge && r.repos.has(repo) && who.has(r.user));
      if (!targets.length) { stats.unrouted++; log(`${f.forge} ${f.event} ${repo}: no agent among [${[...who].join(',')}]`); return json(res, 202, { routed: 0 }); }
      targets.forEach((r) => forward(r, { forge: f.forge, event: f.event, repo }));
      log(`${f.forge} ${f.event} ${repo} -> ${targets.map((r) => r.agent).join(',')}`);
      return json(res, 202, { routed: targets.length });
    });
  }

  json(res, 404, { error: 'not found' });
}).listen(port, () => log(`listening on :${port}; forges: ${Object.keys(forgeSecrets).join(',') || 'none'}`));
