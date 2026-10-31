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
// Scale to zero (#35): agents whose StatefulSet is annotated agentdev/scale-to-zero="true" may be stopped.
//   - An agent idle for its IDLE_SECONDS asks POST /idle (with its own wake token); the router scales its StatefulSet
//     to 0 unless a wake was forwarded to it since it became idle.
//   - A webhook for a person with no live agent: the router looks up stopped agents (annotations agentdev/user,
//     agentdev/repos, agentdev/forge) and scales the matching one to 1. Nothing is buffered: a starting agent
//     polls the forge first, so the event's work is found anyway.
//   The router keeps no list of stopped agents; Kubernetes is the source of truth, so a router restart loses nothing.
//
// Env: PORT (8080), FORGE_SECRETS ("gitea=<secret>,github=<secret>"), REGISTER_TOKEN (agents -> router),
//      REGISTRATION_TTL_SECONDS (300), K8S_NAMESPACE (agents namespace; scaling is off without it),
//      K8S_API_URL (default: in-cluster API with the pod's service account).
'use strict';
const http = require('node:http');
const crypto = require('node:crypto');

const port = Number(process.env.PORT || 8080);
const ttlMs = Number(process.env.REGISTRATION_TTL_SECONDS || 300) * 1000;
const registerToken = process.env.REGISTER_TOKEN || '';
const forgeSecrets = Object.fromEntries((process.env.FORGE_SECRETS || '').split(',').filter(Boolean)
  .map((kv) => { const i = kv.indexOf('='); return [kv.slice(0, i), kv.slice(i + 1)]; }));

const log = (m) => console.log(`${new Date().toISOString().slice(11, 19)} router: ${m}`);
const stats = { delivered: 0, rejected: 0, unrouted: 0, forwarded: 0, forwardErrors: 0, scaledUp: 0, scaledDown: 0, scaleErrors: 0 };

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

// ---------- Kubernetes (scale to zero, #35) ----------
const ns = process.env.K8S_NAMESPACE || '';
const saDir = '/var/run/secrets/kubernetes.io/serviceaccount';
const k8sUrl = process.env.K8S_API_URL || 'https://kubernetes.default.svc';
function k8s(method, path, body, contentType = 'application/json') {
  const fs = require('node:fs');
  const u = new URL(path, k8sUrl);
  const tls = u.protocol === 'https:';
  const headers = { accept: 'application/json' };
  if (tls) headers.authorization = `Bearer ${fs.readFileSync(`${saDir}/token`, 'utf8').trim()}`;
  const data = body === undefined ? '' : JSON.stringify(body);
  if (data) { headers['content-type'] = contentType; headers['content-length'] = Buffer.byteLength(data); }
  const mod = tls ? require('node:https') : http;
  return new Promise((resolve, reject) => {
    const r = mod.request(u, { method, headers, timeout: 5000, ...(tls ? { ca: fs.readFileSync(`${saDir}/ca.crt`) } : {}) }, (res) => {
      let b = ''; res.on('data', (c) => (b += c));
      res.on('end', () => {
        if (res.statusCode >= 300) return reject(new Error(`k8s ${method} ${u.pathname}: HTTP ${res.statusCode}`));
        try { resolve(b ? JSON.parse(b) : {}); } catch (e) { reject(new Error(`k8s ${method} ${u.pathname}: bad JSON`)); }
      });
    });
    r.on('error', reject); r.on('timeout', () => r.destroy(new Error('k8s timeout'))); r.end(data);
  });
}
const stsPath = (name) => `/apis/apps/v1/namespaces/${ns}/statefulsets${name ? `/${name}` : ''}`;
const scaleTo = (name, n) => k8s('PATCH', `${stsPath(name)}/scale`, { spec: { replicas: n } }, 'application/merge-patch+json');
// StatefulSets that may scale to zero, as routing records
async function scalable() {
  const list = await k8s('GET', `${stsPath()}?labelSelector=app%3Dcoding-agent`);
  return (list.items || []).map((s) => {
    const a = s.metadata.annotations || {};
    return { name: s.metadata.name, enabled: a['agentdev/scale-to-zero'] === 'true', user: a['agentdev/user'],
      forge: a['agentdev/forge'] || 'gitea', repos: (a['agentdev/repos'] || '').split(',').filter(Boolean), replicas: s.spec.replicas };
  }).filter((s) => s.enabled);
}
const waking = new Map();   // sts name -> time scaled up (no double scale-ups while the pod starts)
async function wakeStopped(forge, repo, who) {
  if (!ns) return [];
  // Every opted-in agent that is not live is a candidate, whatever its replica count: an agent being scaled down
  // (replicas already 0 or pod terminating) must come back for this event. scaleTo(1) is idempotent.
  const liveNames = new Set(live().map((r) => `agent-${r.agent}`));
  const todo = (await scalable()).filter((s) => !liveNames.has(s.name) && s.forge === forge && s.repos.includes(repo)
    && who.has(s.user) && Date.now() - (waking.get(s.name) || 0) >= 120e3);
  const results = await Promise.allSettled(todo.map((s) => scaleTo(s.name, 1).then(() => {
    waking.set(s.name, Date.now()); stats.scaledUp++;
    log(`scaled ${s.name} -> 1 (${forge} event for ${s.user} in ${repo})`); return s.name;
  })));
  results.filter((r) => r.status === 'rejected').forEach((r) => { stats.scaleErrors++; log(r.reason.message); });
  return results.filter((r) => r.status === 'fulfilled').map((r) => r.value);
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
      const prev = registry.get(r.agent);
      registry.set(r.agent, { agent: r.agent, forge: r.forge || 'gitea', user: r.user, repos: new Set(r.repos), url: r.url, token: r.token, seen: Date.now(), lastWake: prev ? prev.lastWake : 0 });
      if (isNew) log(`registered ${r.agent} (${r.forge || 'gitea'}:${r.user}) for ${r.repos.join(',')} -> ${r.url}`);
      return json(res, 200, { ok: true, ttlSeconds: ttlMs / 1000 });
    });
  }

  // An idle agent asks to be stopped (#35). Body: {agent, idleSince (ms epoch)}. Authenticated with that agent's own
  // wake token (from its registration), so one agent cannot stop another.
  if (req.method === 'POST' && req.url === '/idle') {
    return readBody(req, 4e3, (body) => (async () => {
      let b; try { b = JSON.parse(body); } catch { return json(res, 400, { error: 'bad json' }); }
      const r = registry.get(b.agent);
      if (!r || !safeEqual(req.headers.authorization || '', `Bearer ${r.token}`)) return json(res, 401, { error: 'unauthorized' });
      if (!ns) return json(res, 200, { stopped: false, reason: 'scaling off (no K8S_NAMESPACE)' });
      const woken = () => r.lastWake > Number(b.idleSince || 0);
      if (woken()) return json(res, 200, { stopped: false, reason: 'woken since idle' });
      const name = `agent-${b.agent}`;
      const s = (await scalable()).find((x) => x.name === name);
      if (!s) return json(res, 200, { stopped: false, reason: 'scale-to-zero not enabled' });
      if (woken()) return json(res, 200, { stopped: false, reason: 'woken since idle' });   // a wake during the lookup
      registry.delete(b.agent);   // from here on, events for it scale it up again (wakeStopped)
      await scaleTo(name, 0); waking.delete(name); stats.scaledDown++;
      log(`scaled ${name} -> 0 (idle)`);
      return json(res, 200, { stopped: true });
    })().catch((e) => { stats.scaleErrors++; log(e.message); if (!res.headersSent) json(res, 500, { error: 'scale failed' }); }));
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
      if (!targets.length) {
        // nobody live: maybe a stopped agent is responsible (#35)
        return wakeStopped(f.forge, repo, who).then((started) => {
          if (started.length) return json(res, 202, { routed: 0, started });
          stats.unrouted++; log(`${f.forge} ${f.event} ${repo}: no agent among [${[...who].join(',')}]`);
          return json(res, 202, { routed: 0 });
        }).catch((e) => { stats.scaleErrors++; log(e.message); if (!res.headersSent) json(res, 202, { routed: 0 }); });
      }
      targets.forEach((r) => { r.lastWake = Date.now(); forward(r, { forge: f.forge, event: f.event, repo }); });
      log(`${f.forge} ${f.event} ${repo} -> ${targets.map((r) => r.agent).join(',')}`);
      return json(res, 202, { routed: targets.length });
    });
  }

  json(res, 404, { error: 'not found' });
}).listen(port, () => log(`listening on :${port}; forges: ${Object.keys(forgeSecrets).join(',') || 'none'}; scaling: ${ns ? `on (${ns})` : 'off'}`));
// never die on a stray async error: log it (an exit would drop every registration and delivery in flight)
process.on('unhandledRejection', (e) => log(`unhandled: ${e && e.message}`));
