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
//   - Wake poll (#181), for instances without webhooks: every WAKE_POLL_SECONDS the router asks the forge, with a
//     read-only credential of its own, whether a stopped agent has new work (an open issue given to it, or one of its
//     open PRs, changed since it stopped) and scales only those agents to 1, as the webhook path does.
//   The router keeps no list of stopped agents; Kubernetes is the source of truth, so a router restart loses nothing.
//
// Listeners (#214): PORT (8080) is the public webhook port: it serves only the deliveries (POST / and /hook...) and
// answers 404 for anything else, so a Gateway or reverse proxy that publishes it cannot expose the rest. INTERNAL_PORT
// (8081) serves everything for in-cluster callers: /healthz, /status, /register, /idle (and the deliveries too).
// HEALTH_PORT (8082) serves only GET /healthz (#218): the load balancer health check and the kubelet probes use it, so
// the Google Front End ranges that the NetworkPolicy must let in never reach /status on the internal port.
// Env: PORT (8080), INTERNAL_PORT (8081), HEALTH_PORT (8082), FORGE_SECRETS ("gitea=<secret>,github=<secret>"), REGISTER_TOKEN (agents -> router),
//      REGISTRATION_TTL_SECONDS (300), K8S_NAMESPACE (agents namespace; scaling is off without it),
//      K8S_API_URL (default: in-cluster API with the pod's service account),
//      WAKE_POLL_SECONDS (0 = off), GITEA_URL, GITHUB_API_URL (https://api.github.com), FORGE_POLL_DIR (files of the
//      read-only poll credential: gitea-token, or github-app-id, private-key.pem and optional github-installation-id).
'use strict';
const http = require('node:http');
const crypto = require('node:crypto');

const port = Number(process.env.PORT || 8080);
const internalPort = Number(process.env.INTERNAL_PORT || 8081);
const healthPort = Number(process.env.HEALTH_PORT || 8082);
const ttlMs = Number(process.env.REGISTRATION_TTL_SECONDS || 300) * 1000;
const registerToken = process.env.REGISTER_TOKEN || '';
const forgeSecrets = Object.fromEntries((process.env.FORGE_SECRETS || '').split(',').filter(Boolean)
  .map((kv) => { const i = kv.indexOf('='); return [kv.slice(0, i), kv.slice(i + 1)]; }));

const log = (m) => console.log(`${new Date().toISOString().slice(11, 19)} router: ${m}`);
const stats = { delivered: 0, rejected: 0, unrouted: 0, forwarded: 0, forwardErrors: 0, scaledUp: 0, scaledDown: 0, scaleErrors: 0,
  polls: 0, polledUp: 0, pollErrors: 0, prLookups: 0, prLookupErrors: 0 };

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

// GitHub (#2): all agents act as one App bot, so logins do not say which agent is meant. The agent names come from
// the labels agent:<name> on the issue or PR and from the PR head branch agent/<name>/...; names are checked.
const agentName = /^[a-z0-9][a-z0-9-]{0,40}$/;
function labelAgents(p) {
  const out = new Set();
  const item = p.issue || p.pull_request || {};
  const labels = [...(item.labels || []), ...(p.label ? [p.label] : [])];
  for (const l of labels) {
    const m = /^agent:(.+)$/.exec((l && l.name) || '');
    if (m && agentName.test(m[1])) out.add(m[1]);
  }
  return out;
}
function agentsOf(p) {
  const out = labelAgents(p);
  const ref = (p.pull_request && p.pull_request.head && p.pull_request.head.ref) || '';
  const m = /^agent\/([^/]+)\//.exec(ref);
  if (m && agentName.test(m[1])) out.add(m[1]);
  // a review thread or a check run on an agent's PR names the branch in pull_requests[] / check_suite
  for (const pr of (p.check_run && p.check_run.pull_requests) || (p.check_suite && p.check_suite.pull_requests) || []) {
    const r = /^agent\/([^/]+)\//.exec((pr.head && pr.head.ref) || '');
    if (r && agentName.test(r[1])) out.add(r[1]);
  }
  return out;
}
// Gitea (#241 P7): logins, plus the agent:<name> labels (the agent labels its own PRs since #241 P5; there the agent
// name is its forge login). A label only wakes an agent, which re-reads the forge; it never gives work.
const routingFacts = (forge, p) => (forge === 'github' ? agentsOf(p) : new Set([...people(p), ...labelAgents(p)]));

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
        try { resolve(b ? JSON.parse(b) : {}); } catch { reject(new Error(`k8s ${method} ${u.pathname}: bad JSON`)); }
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
      blocked: a['agentdev/blocked'] === 'true',
      forge: a['agentdev/forge'] || 'gitea', repos: (a['agentdev/repos'] || '').split(',').filter(Boolean), replicas: s.spec.replicas };
  }).filter((s) => s.enabled);
}
const waking = new Map();   // sts name -> time scaled up (no double scale-ups while the pod starts)
const stoppedAt = new Map(); // sts name -> time the router stopped it (idle); the wake poll looks for changes after it
// start these stopped agents (the one way up for both the webhook and the poll path)
async function start(todo, why) {
  const results = await Promise.allSettled(todo.map((s) => scaleTo(s.name, 1).then(() => {
    waking.set(s.name, Date.now()); stoppedAt.delete(s.name); stats.scaledUp++;
    log(`scaled ${s.name} -> 1 (${why(s)})`); return s.name;
  })));
  results.filter((r) => r.status === 'rejected').forEach((r) => { stats.scaleErrors++; log(r.reason.message); });
  return results.filter((r) => r.status === 'fulfilled').map((r) => r.value);
}
// opted-in agents that may be started now: not blocked (#27), not live, not started in the last 2 minutes
async function startable() {
  const liveNames = new Set(live().map((r) => `agent-${r.agent}`));
  return (await scalable()).filter((s) => !s.blocked && !liveNames.has(s.name) && Date.now() - (waking.get(s.name) || 0) >= 120e3);
}
async function wakeStopped(forge, repo, who) {
  if (!ns) return [];
  // Every opted-in agent that is not live is a candidate, whatever its replica count: an agent being scaled down
  // (replicas already 0 or pod terminating) must come back for this event. scaleTo(1) is idempotent.
  const todo = (await startable()).filter((s) => s.forge === forge && s.repos.includes(repo) && who.has(s.user));
  return start(todo, (s) => `${forge} event for ${s.user} in ${repo}`);
}

// ---------- wake poll (#181): scale to zero without webhooks ----------
const pollMs = Number(process.env.WAKE_POLL_SECONDS || 0) * 1000;
const pollDir = process.env.FORGE_POLL_DIR || '/var/run/agentdev/forge-poll';
const giteaUrl = (process.env.GITEA_URL || '').replace(/\/+$/, '');
const githubApi = (process.env.GITHUB_API_URL || 'https://api.github.com').replace(/\/+$/, '');
const startedAt = Date.now();
const readCred = (f) => { try { return require('node:fs').readFileSync(`${pollDir}/${f}`, 'utf8').trim(); } catch { return ''; } };
async function getJSON(url, headers) {
  const r = await fetch(url, { headers: { accept: 'application/json', ...headers }, redirect: 'error', signal: AbortSignal.timeout(15e3) });
  if (!r.ok) throw new Error(`GET ${new URL(url).pathname}: HTTP ${r.status}`);
  return r.json();
}
// GitHub App (#2): a JWT, then an installation token narrowed to one repo and to read access
const b64url = (b) => Buffer.from(b).toString('base64url');
function appJwt(appId, key) {
  const now = Math.floor(Date.now() / 1000);
  const data = `${b64url(JSON.stringify({ alg: 'RS256', typ: 'JWT' }))}.${b64url(JSON.stringify({ iat: now - 60, exp: now + 540, iss: appId }))}`;
  return `${data}.${crypto.createSign('RSA-SHA256').update(data).sign(key, 'base64url')}`;
}
async function githubToken(repo) {
  const appId = readCred('github-app-id'); const key = readCred('private-key.pem');
  if (!/^[1-9][0-9]{0,15}$/.test(appId) || !key) throw new Error('wake poll: no GitHub App credential');
  const jwt = { authorization: `Bearer ${appJwt(appId, key)}`, 'x-github-api-version': '2022-11-28' };
  let id = readCred('github-installation-id');
  if (!/^[1-9][0-9]{0,15}$/.test(id)) id = String((await getJSON(`${githubApi}/repos/${repo}/installation`, jwt)).id);
  const r = await fetch(`${githubApi}/app/installations/${id}/access_tokens`, { method: 'POST', redirect: 'error',
    headers: { ...jwt, accept: 'application/json', 'content-type': 'application/json' }, signal: AbortSignal.timeout(15e3),
    body: JSON.stringify({ repositories: [repo.split('/')[1]], permissions: { issues: 'read', pull_requests: 'read', metadata: 'read' } }) });
  if (!r.ok) throw new Error(`wake poll: installation token: HTTP ${r.status}`);
  const t = await r.json();
  const exp = Date.parse(t.expires_at);
  // cached until 5 minutes before it expires (#227: one PR lookup per comment must not mint a token each time)
  if (typeof t.token === 'string' && Number.isFinite(exp)) ghTokens.set(repo, { token: t.token, until: exp - 300e3 });
  return t.token;
}
const ghTokens = new Map();   // repo -> { token, until }
async function githubTokenCached(repo) {
  const c = ghTokens.get(repo);
  if (c && Date.now() < c.until) return c.token;
  return githubToken(repo);
}
// Does this stopped agent have work in repo changed after `since` (ms)? Same rules as the agent loop: on Gitea issues
// assigned to it, on GitHub issues labelled agent:<name>; its PRs are the open ones from branches agent/<name>/...
async function hasWork(s, repo, since) {
  const iso = new Date(since).toISOString();
  const after = (x) => Date.parse(x.updated_at) > since;
  const mine = (p) => ((p.head && p.head.ref) || '').startsWith(`agent/${s.user}/`) && after(p);
  if (!/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(repo)) return false;
  if (s.forge === 'github') {
    const h = { authorization: `Bearer ${await githubToken(repo)}`, 'x-github-api-version': '2022-11-28' };
    const q = new URLSearchParams({ state: 'open', labels: `agent:${s.user}`, since: iso, per_page: '20' });
    if ((await getJSON(`${githubApi}/repos/${repo}/issues?${q}`, h)).some(after)) return true;
    return (await getJSON(`${githubApi}/repos/${repo}/pulls?state=open&sort=updated&direction=desc&per_page=50`, h)).some(mine);
  }
  const tok = readCred('gitea-token');
  if (!giteaUrl || !tok) throw new Error('wake poll: no GITEA_URL or gitea-token');
  const h = { authorization: `token ${tok}` };
  const q = new URLSearchParams({ state: 'open', type: 'issues', assigned_by: s.user, since: iso, limit: '20' });
  if ((await getJSON(`${giteaUrl}/api/v1/repos/${repo}/issues?${q}`, h)).some(after)) return true;
  return (await getJSON(`${giteaUrl}/api/v1/repos/${repo}/pulls?state=open&sort=recentupdate&limit=50`, h)).some(mine);
}
let polling = false;
async function pollOnce() {
  if (!ns || polling) return [];
  polling = true; stats.polls++;
  try {
    // only agents that are really down (replicas 0); after a router restart the stop time is unknown: look back one
    // poll interval before the start (an agent stops only when it found no work, so older changes were seen)
    const todo = [];
    for (const s of (await startable()).filter((x) => x.replicas === 0)) {
      const since = stoppedAt.get(s.name) || (startedAt - pollMs);
      for (const repo of s.repos) {
        try { if (await hasWork(s, repo, since)) { todo.push(s); break; } } catch (e) { stats.pollErrors++; log(`wake poll ${s.name}: ${e.message}`); }
      }
    }
    const started = await start(todo, () => 'wake poll: new work on the forge');
    stats.polledUp += started.length;
    return started;
  } catch (e) { stats.pollErrors++; log(`wake poll: ${e.message}`); return []; } finally { polling = false; }
}
if (pollMs > 0) setInterval(pollOnce, pollMs).unref();

function readBody(req, limit, cb) {
  const chunks = []; let size = 0;
  req.on('data', (c) => { size += c.length; if (size > limit) req.destroy(); else chunks.push(c); });
  req.on('end', () => cb(Buffer.concat(chunks)));
}

const json = (res, code, obj) => { res.writeHead(code, { 'content-type': 'application/json' }).end(JSON.stringify(obj)); };

const isDelivery = (req) => req.method === 'POST' && (req.url === '/' || req.url.startsWith('/hook'));

function handle(req, res) {
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
      await scaleTo(name, 0); waking.delete(name); stoppedAt.set(name, Date.now()); stats.scaledDown++;
      log(`scaled ${name} -> 0 (idle)`);
      return json(res, 200, { stopped: true });
    })().catch((e) => { stats.scaleErrors++; log(e.message); if (!res.headersSent) json(res, 500, { error: 'scale failed' }); }));
  }

  if (isDelivery(req)) {
    return readBody(req, 5e6, (body) => deliver(req, res, body).catch((e) => {
      log(`delivery: ${e && e.message}`); if (!res.headersSent) json(res, 500, { error: 'delivery failed' });
    }));
  }

  json(res, 404, { error: 'not found' });
}

// one verified webhook delivery -> wake the agents it is about (#3); async for the PR lookup (#227)
async function deliver(req, res, body) {
  const f = detectForge(req.headers);
  const secret = f && forgeSecrets[f.forge];
  if (!f || !secret || !f.sig || !safeEqual(f.sig, hmac(secret, body))) { stats.rejected++; log('rejected delivery (signature)'); return json(res, 401, { error: 'bad signature' }); }
  stats.delivered++;
  let p; try { p = JSON.parse(body); } catch { return json(res, 400, { error: 'bad json' }); }
  const repo = p.repository && p.repository.full_name;
  // the user an agent registered with: its forge login on Gitea, its agent name on GitHub
  const who = routingFacts(f.forge, p);
  // GitHub comment on a PR without an agent:<name> label (#227): the payload has no head branch, so read it from the
  // PR with the router's read-only credential (the wake poll's). PRs opened since #227 carry the label already.
  const prRef = f.forge === 'github' && !who.size && p.issue && p.issue.pull_request && Number.isInteger(p.issue.number);
  if (prRef && /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(repo || '')) {
    try {
      const h = { authorization: `Bearer ${await githubTokenCached(repo)}`, 'x-github-api-version': '2022-11-28' };
      const pr = await getJSON(`${githubApi}/repos/${repo}/pulls/${p.issue.number}`, h);
      // only a branch of this repository counts: a fork can name its branch agent/<name>/... too
      if (pr && pr.head && pr.head.repo && pr.head.repo.full_name === repo) agentsOf({ pull_request: pr }).forEach((a) => who.add(a));
      stats.prLookups++;
    } catch (e) { stats.prLookupErrors++; log(`PR lookup for ${repo}#${p.issue.number}: ${e.message}`); }
  }
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
}

// public: deliveries only (#214)
http.createServer((req, res) => (isDelivery(req) ? handle(req, res) : json(res, 404, { error: 'not found' })))
  .listen(port, () => log(`public (webhooks only) on :${port}`));
// health only (#218): the load balancer health check and the probes
http.createServer((req, res) => (req.method === 'GET' && req.url === '/healthz' ? json(res, 200, { ok: true }) : json(res, 404, { error: 'not found' })))
  .listen(healthPort, () => log(`health only on :${healthPort}`));
http.createServer(handle).listen(internalPort, () => log(`internal on :${internalPort}; forges: ${Object.keys(forgeSecrets).join(',') || 'none'}; scaling: ${ns ? `on (${ns})` : 'off'}; wake poll: ${pollMs && ns ? `every ${pollMs / 1000}s` : 'off'}`));
// never die on a stray async error: log it (an exit would drop every registration and delivery in flight)
process.on('unhandledRejection', (e) => log(`unhandled: ${e && e.message}`));
