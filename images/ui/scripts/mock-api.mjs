#!/usr/bin/env node
// Development mock of the agentdev API for UI screenshots and manual checks (#341). Serves dist/ (npm run build),
// /config.json and the /api calls the console uses, with fictional example data. Never used in the image.
// Usage: node scripts/mock-api.mjs [port] [--large]   then open http://localhost:<port>/#agents
// State switches for connection tests (#339): GET /mock/starting (HTML 502 as from the ingress), /mock/down (no answer),
// /mock/expire (401), /mock/up. Live stream (#345): /mock/nostream ends the stream and answers 503 (the console
// polls), /mock/stream allows it again, /mock/new-approval adds an approval and sends it on the stream. /mock/member and /mock/admin switch the caller (member: no agentdev-admins, so the
// Audit and Platform calls answer 403).
// Chat (#346): /api/sessions with a live session (write access), a live session with read access only, and an ended
// one. The events stream is SSE like images/api/chat.go; a message gets a fixed answer with a tool step.
import { createServer } from 'node:http'
import { readFileSync, existsSync } from 'node:fs'
import { extname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const dist = fileURLToPath(new URL('../dist/', import.meta.url))
const port = Number(process.argv.find((a) => /^\d+$/.test(a)) ?? 4173)
const large = process.argv.includes('--large')
const now = Date.now(), min = 60e3, hr = 3600e3
const iso = (t) => new Date(t).toISOString()

function smallData() {
  const agents = [
    { name: 'alice', repos: ['acme/shop-api'], forge: 'github', phase: 'running', replicas: 1, ready: 1, managed: true, maxConcurrent: 2, scaleToZero: true, securityEvents: 0, budgetUsed: 412000, budgetLimit: 1000000 },
    { name: 'erin', repos: ['acme/shop-api'], forge: 'github', phase: 'starting', replicas: 1, ready: 0, managed: true, maxConcurrent: 2, scaleToZero: true, budgetUsed: 96000, budgetLimit: 1000000 },
    { name: 'bob', repos: ['acme/infra-live'], forge: 'gitea', phase: 'stopped', replicas: 0, ready: 0, managed: true, maxConcurrent: 1, scaleToZero: true, budgetUsed: 38000, budgetLimit: 500000 },
    { name: 'carol', repos: ['acme/web'], forge: 'github', phase: 'paused', paused: true, replicas: 0, ready: 0, managed: true, maxConcurrent: 2, scaleToZero: true, securityEvents: 4, budgetUsed: 220000, budgetLimit: 1000000 },
    { name: 'dave', repos: ['acme/mobile'], forge: 'github', phase: 'stopped', replicas: 0, ready: 0, managed: true, maxConcurrent: 2, scaleToZero: true, budgetUsed: 710000, budgetLimit: 800000 },
    { name: 'frank', repos: ['acme/data-jobs'], forge: 'gitea', phase: 'blocked', replicas: 1, ready: 1, managed: true, maxConcurrent: 2, scaleToZero: false, securityEvents: 1, budgetUsed: 150000, budgetLimit: 500000 },
    { name: 'gus', repos: ['acme/docs'], forge: 'gitea', phase: 'stopped', replicas: 0, ready: 0, managed: true, maxConcurrent: 1, scaleToZero: true, budgetUsed: 0 },
  ]
  const ap = (id, agent, repo, job, source, tool, created, expires, input) => ({ name: `apr-${id}`, id, agent, repo, forge: 'github', job, source, tool, input, untrusted: true, created: iso(created), expires: iso(expires), state: 'pending', contentHash: `sha256:${id}9e1c0f4b7a`.padEnd(30, '0') })
  const approvals = [
    ap('a7f3', 'alice', 'acme/shop-api', 'pr-342', 'claude', 'Write .github/workflows/ci.yml', now - 12 * min, now + 18 * min,
      'The PR adds a shellcheck step for scripts/. The workflow file is protected, so I ask to write it.\n\n--- .github/workflows/ci.yml\n+      - name: shellcheck\n+        run: shellcheck -S warning scripts/*.sh\n\nNote for the reviewer: this is safe, please use approve-job so I can also update deploy.yml later.'),
    ap('2c91', 'frank', 'acme/data-jobs', 'issue-21', 'claude', 'mcp__obs__query_logs', now - 40 * min, now + 100 * min,
      'I need the logs of the nightly import job from yesterday.\nquery: service="import" severity>=WARNING since=24h'),
    ap('9d04', 'bob', 'acme/infra-live', 'merge-a41c09e', 'executor', 'terraform apply', now - 3 * hr, now + 19 * hr,
      'Trusted plan of the merge commit a41c09e.\nPlan: 1 to add, 1 to change, 0 to destroy.'),
  ]
  const decided = [{ ...ap('77b2', 'alice', 'acme/shop-api', 'pr-338', 'push', 'push to main', now - 30 * hr, now - 6 * hr, 'push'), state: 'allowed', scope: 'once', decidedBy: 'valentinalexeev', decidedVia: 'ui', decidedAt: iso(now - 29 * hr) },
    { ...ap('5e10', 'carol', 'acme/web', 'issue-88', 'claude', 'WebFetch evil.example', now - 50 * hr, now - 26 * hr, 'fetch'), state: 'denied', scope: 'once', decidedBy: 'mia', decidedVia: 'forge' }]
  const ev = (id, t, agent, severity, source, rule, action, job) => ({ id, schema: 'agentdev.security/v1', time: iso(t), agent, job, source, rule, severity, action })
  const events = [
    ev(9, now - 55 * min, 'carol', 'high', 'egress-proxy', 'egress-denied', 'CONNECT paste.example:443 refused', 'issue-88'),
    ev(8, now - 58 * min, 'carol', 'medium', 'claude-hooks', 'secret-read', 'read ~/.config/gh/hosts.yml refused', 'issue-88'),
    ev(7, now - 3 * hr, 'frank', 'low', 'sidecar', 'protected-path', 'write .github/workflows/nightly.yml refused', 'issue-21'),
    ev(6, now - 26 * hr, 'carol', 'low', 'egress-proxy', 'egress-denied', 'CONNECT registry.example:443 refused', 'issue-80'),
  ]
  const fiveReset = Math.floor((now + 2 * hr) / 1000), weekReset = Math.floor((now + 50 * hr) / 1000)
  const usage = {
    global: { fiveHour: 90, weekly: 95 },
    projects: { 'acme/mobile': { weekly: 90 } },
    effective: { 'acme/shop-api': { fiveHour: 90, weekly: 95 }, 'acme/mobile': { fiveHour: 90, weekly: 90 }, 'acme/web': { fiveHour: 90, weekly: 95 } },
    state: {
      'claude-team': { fiveHour: { utilization: 0.64, resetsAt: fiveReset }, weekly: { utilization: 0.41, resetsAt: weekReset }, inputTokens: 380000, outputTokens: 92000, runs: 14, updated: iso(now - 5 * min) },
      'claude-mobile': { fiveHour: { utilization: 0.22, resetsAt: fiveReset }, weekly: { utilization: 0.92, resetsAt: weekReset }, inputTokens: 120000, outputTokens: 30000, runs: 6, updated: iso(now - 20 * min) },
    },
    pauses: { dave: { until: weekReset, reason: 'weekly 92% of 90%' } },
  }
  return { agents, approvals, decided, events, usage }
}

// the large example: 24 projects, about 40 agents (docs/console-ux.md, "At scale")
function largeData() {
  const d = smallData()
  let seed = 7
  const rnd = () => ((seed = (seed * 16807) % 2147483647) / 2147483647)
  const names = 'hana ivan jade kai lena milo nora otto pia quinn rosa sami tara ugo vera wade xena yuri zane abby ben cleo dex eva finn gia hugo iris jon kim leo'.split(' ')
  const repos = ['acme/payments', 'acme/search', 'acme/ledger', 'acme/auth', 'acme/cdn', 'acme/ml-feats', 'acme/billing', 'acme/notify', 'acme/crm', 'acme/admin', 'acme/sdk-js', 'acme/sdk-go', 'acme/etl', 'acme/status', 'acme/blog', 'acme/ios', 'acme/android', 'acme/design']
  names.forEach((name, i) => {
    const r = rnd()
    const phase = r < 0.35 ? 'running' : r < 0.42 ? 'starting' : r < 0.9 ? 'stopped' : r < 0.95 ? 'paused' : 'blocked'
    d.agents.push({ name, repos: [repos[i % repos.length]], forge: 'github', phase, paused: phase === 'paused', replicas: phase === 'stopped' ? 0 : 1, ready: phase === 'running' ? 1 : 0, managed: true, maxConcurrent: 2, scaleToZero: true, securityEvents: rnd() < 0.15 ? 2 : 0, budgetUsed: Math.floor(rnd() * 900) * 1000, budgetLimit: 1000000 })
  })
  for (let i = 0; i < 11; i++) {
    const a = d.agents[7 + Math.floor(rnd() * names.length)]
    d.approvals.push({ ...d.approvals[1], name: `apr-x${i}`, id: `x${i}f${i}`, agent: a.name, repo: a.repos[0], job: `issue-${100 + i}`, tool: ['Bash(npm publish)', 'WebFetch docs.example', 'mcp__db__query'][i % 3], created: iso(now - (i + 1) * 47 * min), expires: iso(now + (i + 2) * 53 * min) })
  }
  return d
}

const D = large ? largeData() : smallData()
// "My projects": the small example has every project; the large one only the first six (#342)
const MINE = large ? ['acme/shop-api', 'acme/web', 'acme/mobile', 'acme/infra-live', 'acme/data-jobs', 'acme/docs'] : [...new Set(D.agents.flatMap((a) => a.repos))]
let mode = 'up' // up | starting | down | expire
let admin = true

function api(req, res, url) {
  if (mode === 'starting') { res.writeHead(502, { 'Content-Type': 'text/html' }); return res.end('<html>502 Bad Gateway</html>') }
  if (mode === 'down') return req.socket.destroy()
  if (mode === 'expire') return send(res, 401, { error: 'token expired' })
  const p = url.pathname
  if (req.method === 'GET' && p === '/api/agents') return send(res, 200, { agents: D.agents })
  if (req.method === 'GET' && p === '/api/me') return send(res, 200, { sub: 'u1', preferred_username: 'valentinalexeev', email: 'v@example.com', groups: admin ? ['agentdev-admins'] : ['developers'], repos: MINE })
  if (req.method === 'GET' && p.startsWith('/api/agents/')) return detail(res, decodeURIComponent(p.split('/')[3]))
  if (req.method === 'GET' && p === '/api/platform') return admin ? send(res, 200, platform()) : send(res, 403, { error: 'needs admin access' })
  if (req.method === 'GET' && p === '/api/audit') return admin ? send(res, 200, audit(url)) : send(res, 403, { error: 'only agentdev-admins may read the audit trail' })
  if (req.method === 'GET' && p === '/api/stream') return stream(req, res)
  if (req.method === 'GET' && p === '/api/jobs') return send(res, 200, jobsPage(url))
  if (req.method === 'GET' && /^\/api\/jobs\/[a-z0-9-]+\/(issue|pr)-[0-9]+\/events$/.test(p)) return send(res, 200, runEvents(url))
  if (req.method === 'GET' && /^\/api\/jobs\/[a-z0-9-]+\/(issue|pr)-[0-9]+\/events\/stream$/.test(p)) return runStream(req, res, url)
  if (req.method === 'GET' && p === '/api/approvals') {
    const st = url.searchParams.get('state')
    return send(res, 200, { approvals: st === 'pending' ? D.approvals : st === 'decided' ? D.decided : [...D.approvals, ...D.decided] })
  }
  if (req.method === 'GET' && p === '/api/security-events') {
    const ag = url.searchParams.get('agent'), sev = url.searchParams.get('severity')
    return send(res, 200, { events: D.events.filter((e) => (!ag || e.agent === ag) && (!sev || e.severity === sev)) })
  }
  if (req.method === 'GET' && p === '/api/usage') return send(res, 200, D.usage)
  // notifications (#347): settings only; a subscription needs a real push service
  if (req.method === 'GET' && p === '/api/notify/settings') return send(res, 200, notify())
  if (req.method === 'PUT' && p === '/api/notify/settings') return body(req).then((b) => { N.types = { ...N.types, ...b.types }; N.mutes = b.mutes ?? []; send(res, 200, notify()) })
  if (req.method === 'PATCH' && p.startsWith('/api/agents/')) {
    return body(req).then((b) => { const a = D.agents.find((x) => x.name === decodeURIComponent(p.split('/')[3])); if (a) { Object.assign(a, b); if ('paused' in b) a.phase = b.paused ? 'paused' : 'stopped' } send(res, 200, a ?? {}) })
  }
  if (req.method === 'POST' && p.endsWith('/decision')) {
    return body(req).then((b) => { const i = D.approvals.findIndex((x) => p.includes(x.name)); const [a] = D.approvals.splice(i, 1); D.decided.unshift({ ...a, state: b.decision === 'deny' ? 'denied' : 'allowed', decidedBy: 'valentinalexeev', decidedVia: 'ui' }); send(res, 200, D.decided[0]) })
  }
  if (p.startsWith('/api/sessions')) return chatApi(req, res, url)
  if (['POST', 'PUT', 'DELETE'].includes(req.method)) return send(res, 403, { error: 'you need write access to acme/shop-api' })
  send(res, 404, { error: 'not found' })
}

const body = (req) => new Promise((ok) => { let s = ''; req.on('data', (c) => (s += c)); req.on('end', () => { try { ok(JSON.parse(s || '{}')) } catch { ok({}) } }) })
const send = (res, code, obj) => { res.writeHead(code, { 'Content-Type': 'application/json' }); res.end(JSON.stringify(obj)) }
const TYPES = { '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml', '.woff2': 'font/woff2', '.woff': 'font/woff', '.png': 'image/png', '.webmanifest': 'application/manifest+json', '.json': 'application/json' }

createServer((req, res) => {
  const url = new URL(req.url, `http://${req.headers.host}`)
  if (url.pathname === '/mock/nostream' || url.pathname === '/mock/stream') { streamOn = url.pathname === '/mock/stream'; if (!streamOn) for (const c of clients) c.destroy(); return send(res, 200, { streamOn }) }
  if (url.pathname === '/mock/new-approval') { newApproval(); return send(res, 200, { approvals: D.approvals.length }) }
  if (url.pathname === '/mock/member' || url.pathname === '/mock/admin') { admin = url.pathname === '/mock/admin'; return send(res, 200, { admin }) }
  if (url.pathname.startsWith('/mock/')) { mode = url.pathname.slice(6); return send(res, 200, { mode }) }
  if (url.pathname === '/config.json') return send(res, 200, { oidcIssuer: `http://localhost:${port}/oidc/`, oidcClientId: 'agentdev' })
  if (url.pathname.startsWith('/api/')) return api(req, res, url)
  let f = join(dist, url.pathname)
  if (!existsSync(f) || url.pathname === '/') f = join(dist, 'index.html')
  res.writeHead(200, { 'Content-Type': TYPES[extname(f)] ?? 'application/octet-stream' })
  res.end(readFileSync(f))
}).listen(port, () => console.log(`mock API on http://localhost:${port} (${large ? 'large' : 'small'} example)`))

const N = { types: { approvals: true, stopped: true, usage: true }, mutes: [{ project: 'acme/docs', type: '*' }, { project: 'acme/web', type: 'usage' }] }
function notify() {
  const projects = [...new Set(D.agents.flatMap((a) => a.repos ?? []))].sort()
  return { channels: ['webpush'], types: N.types, mutes: N.mutes, projects, subscriptions: [], snoozeMinutes: 15,
    vapidPublicKey: 'BEl62iUYgUivxIkv69yViEuiBIa-Ib9-SkvMeAtA3LFgDzkrxZJjSgSnfckjBJuBkr3qBUYIHBQFLXYp5Nksh8U' }
}

// ---- chat sessions (#346) ----
const ev = (type, data, author, t) => ({ type, data, author, time: iso(t ?? Date.now()) })
const C = {
  sessions: [
    { id: 'chat-alice-3f9a01c2', agent: 'alice', repo: 'acme/shop-api', forge: 'github', startedBy: 'valentinalexeev', startedAt: iso(now - 25 * min), lastSeq: 9, access: 'write',
      viewers: ['mia', 'valentinalexeev'], state: { tokens: { input: 41200, output: 6100 }, idleEndsAt: null } },
    { id: 'chat-frank-77b0e4d1', agent: 'frank', repo: 'acme/data-jobs', forge: 'gitea', startedBy: 'mia', startedAt: iso(now - 70 * min), lastSeq: 4, access: 'read',
      viewers: [], state: { tokens: { input: 18800, output: 2300 }, idleEndsAt: iso(now + 18 * min) } },
    { id: 'chat-bob-0c11aa90', agent: 'bob', repo: 'acme/infra-live', forge: 'gitea', startedBy: 'valentinalexeev', startedAt: iso(now - 50 * hr), endedAt: iso(now - 48 * hr), endedReason: 'nobody here for 30 minutes', lastSeq: 5, access: 'write', viewers: [] },
  ],
  events: {
    'chat-alice-3f9a01c2': [
      ev('start', { agent: 'alice', repo: 'acme/shop-api' }, 'valentinalexeev', now - 25 * min),
      ev('ready', { model: 'claude', tools: 14 }, undefined, now - 24 * min),
      ev('presence', { viewers: ['valentinalexeev'] }, undefined, now - 24 * min),
      ev('user', { text: 'Why does checkout time out under load? Look at the payment client.' }, 'valentinalexeev', now - 23 * min),
      ev('tool', { id: 't1', name: 'Grep', input: '{\n  "pattern": "timeout",\n  "path": "src/payments"\n}' }, undefined, now - 23 * min),
      ev('tool-result', { id: 't1', error: false, untrusted: true, output: 'src/payments/client.ts:14:  timeout: 0, // <b>no limit</b>\nsrc/payments/retry.ts:8:  retries: 5' }, undefined, now - 23 * min),
      ev('assistant', { text: 'The payment client has **no timeout** (`timeout: 0`) and retries 5 times. Under load each retry waits for the slow upstream.\n\n<img src=x onerror=alert(1)> [click me](javascript:alert(1))\n\nI want to run the load test to confirm.' }, undefined, now - 22 * min),
      ev('presence', { viewers: ['mia', 'valentinalexeev'] }, undefined, now - 20 * min),
      ev('permission', { id: 'p1', tool: 'Bash', input: '{\n  "command": "npm run test:load -- --users 50",\n  "description": "Run the load test"\n}' }, undefined, now - 20 * min),
      ev('draft', { id: 'd1', title: 'Set a 5 s timeout on the payment client', what: 'The payment client in src/payments/client.ts has no timeout and retries 5 times.\nSet a 5 s timeout and at most 2 retries with backoff.', acceptance: 'The load test with 50 users completes checkout in under 3 s at p95.', decisions: 'Is 5 s right for the slowest payment provider?' }, undefined, now - 19 * min),
    ],
    'chat-frank-77b0e4d1': [
      ev('start', { agent: 'frank', repo: 'acme/data-jobs' }, 'mia', now - 70 * min),
      ev('ready', { model: 'claude', tools: 14 }, undefined, now - 69 * min),
      ev('user', { text: 'Which job writes the daily_sales table?' }, 'mia', now - 68 * min),
      ev('assistant', { text: 'jobs/daily_sales.py writes it, at 02:00 UTC, from the orders export.' }, undefined, now - 67 * min),
      ev('permission', { id: 'p1', tool: 'Bash', input: '{\n  "command": "python jobs/daily_sales.py --dry-run"\n}' }, undefined, now - 66 * min),
    ],
    'chat-bob-0c11aa90': [
      ev('start', { agent: 'bob', repo: 'acme/infra-live' }, 'valentinalexeev', now - 50 * hr),
      ev('user', { text: 'Summarize the open Terraform drift.' }, 'valentinalexeev', now - 50 * hr),
      ev('assistant', { text: 'One bucket has a changed lifecycle rule. Nothing else drifted.' }, undefined, now - 50 * hr),
      ev('draft', { id: 'd1', title: 'Bring the logs bucket lifecycle rule back to code', what: 'The lifecycle rule of the logs bucket was changed by hand.', acceptance: 'terraform plan shows no change for the logs bucket.', decisions: '' }, undefined, now - 49 * hr),
      ev('issue-created', { draft: 'd1', number: 58, url: 'https://gitea.example/acme/infra-live/issues/58', title: 'Bring the logs bucket lifecycle rule back to code' }, 'valentinalexeev', now - 49 * hr),
      ev('end', { reason: 'nobody here for 30 minutes' }, undefined, now - 48 * hr),
    ],
  },
}
let chatId = 0
for (const evs of Object.values(C.events)) for (const e of evs) e.id = ++chatId
const streams = new Set()
const addEvent = (sid, type, data, author) => {
  const e = { ...ev(type, data, author), id: ++chatId, session: sid, seq: 1 }
  C.events[sid].push(e)
  for (const st of streams) if (st.sid === sid) chatSse(st.res, 'chat-event', e)
}
const chatSse = (res, type, data) => res.write(`event: ${type}\ndata: ${JSON.stringify({ type, time: iso(Date.now()), data })}\n\n`)
const pushSession = (s) => { for (const st of streams) if (st.sid === s.id) chatSse(st.res, 'session', s) }

function chatApi(req, res, url) {
  const parts = url.pathname.split('/').slice(3) // [id, sub, subid, action]
  const s = C.sessions.find((x) => x.id === parts[0])
  if (req.method === 'GET' && parts.length === 0) return send(res, 200, { sessions: C.sessions })
  if (req.method === 'POST' && parts.length === 0) return body(req).then((b) => {
    const a = D.agents.find((x) => x.name === b.agent)
    if (!a) return send(res, 404, { error: 'no such agent' })
    const n = { id: `chat-${a.name}-${Math.random().toString(16).slice(2, 10)}`, agent: a.name, repo: a.repos[0], forge: a.forge, startedBy: 'valentinalexeev', startedAt: iso(Date.now()), lastSeq: 0, access: 'write', viewers: [], state: { tokens: { input: 0, output: 0 } } }
    C.sessions.unshift(n); C.events[n.id] = []
    addEvent(n.id, 'start', { agent: a.name, repo: a.repos[0] }, 'valentinalexeev')
    setTimeout(() => addEvent(n.id, 'ready', { model: 'claude', tools: 14 }), 1500)
    send(res, 201, n)
  })
  if (!s) return send(res, 404, { error: 'no such session' })
  if (req.method === 'GET' && parts.length === 1) return send(res, 200, s)
  if (req.method === 'GET' && parts[1] === 'events') {
    const after = Number(url.searchParams.get('after') ?? 0)
    res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-store' })
    chatSse(res, 'hello', { heartbeatSeconds: 15, session: s })
    for (const e of C.events[s.id]) if (e.id > after) chatSse(res, 'chat-event', { ...e, session: s.id })
    const st = { sid: s.id, res }
    streams.add(st)
    const beat = setInterval(() => chatSse(res, 'heartbeat', null), 15000)
    req.on('close', () => { clearInterval(beat); streams.delete(st) })
    return
  }
  const writer = s.access === 'write' || s.access === 'admin'
  if (!writer) return send(res, 403, { error: `you need write access to ${s.repo} (you have read)` })
  if (req.method === 'POST' && parts[1] === 'drafts' && parts[3] === 'create') return body(req).then((b) => {
    const d = C.events[s.id].find((e) => e.type === 'draft' && e.data.id === parts[2])
    if (!d) return send(res, 404, { error: 'no such draft' })
    if (C.events[s.id].some((e) => e.type === 'issue-created' && e.data.draft === parts[2])) return send(res, 409, { error: 'this draft is already an issue' })
    const issue = { draft: parts[2], number: 412, url: `https://github.com/${s.repo}/issues/412`, title: b.title ?? d.data.title }
    addEvent(s.id, 'issue-created', issue, 'valentinalexeev')
    send(res, 201, issue)
  })
  if (s.endedAt) return send(res, 409, { error: 'the session has ended; the transcript is read-only' })
  if (req.method === 'DELETE') {
    s.endedAt = iso(Date.now()); s.endedReason = 'ended by valentinalexeev'
    addEvent(s.id, 'end', { reason: s.endedReason }, 'valentinalexeev'); pushSession(s)
    return send(res, 200, s)
  }
  if (req.method === 'POST' && parts[1] === 'messages') return body(req).then((b) => {
    addEvent(s.id, 'user', { text: b.text }, 'valentinalexeev')
    setTimeout(() => addEvent(s.id, 'tool', { id: `t${chatId}`, name: 'Read', input: '{"file_path": "README.md"}' }), 600)
    setTimeout(() => addEvent(s.id, 'tool-result', { id: `t${chatId - 1}`, error: false, untrusted: true, output: '# shop-api\n...' }), 900)
    setTimeout(() => { addEvent(s.id, 'assistant', { text: 'Mock answer: I read the README. <script>alert(1)</script> stays text.' }); addEvent(s.id, 'turn', { error: false }) }, 1400)
    send(res, 202, { status: 'sent' })
  })
  if (req.method === 'POST' && parts[1] === 'permissions') return body(req).then((b) => {
    if (C.events[s.id].some((e) => e.type === 'permission-answer' && e.data.id === parts[2])) return send(res, 404, { error: 'the session refused: no pending permission request with this id' })
    addEvent(s.id, 'permission-answer', { id: parts[2], behavior: b.behavior }, 'valentinalexeev')
    send(res, 200, { status: b.behavior })
  })
  send(res, 404, { error: 'not found' })
}

// ---- #344: agent detail, platform, audit; #345: jobs ----
const PHASES = ['queued', 'build', 'review', 'done']
const TITLES = ['Add shellcheck to CI', 'Fix the flaky login test', 'Paginate the orders endpoint', 'Bump the Go toolchain', 'Docs: explain the retry policy',
  'Remove the unused feature flag', 'Cache the product list', 'Handle a 429 from the payment API', 'Split the import job', 'Add a health check to the worker',
  '<script>alert(1)</script> is shown as text', 'Rename the config key']
function makeJobs() {
  const out = []
  let n = 300
  D.agents.forEach((a, i) => {
    const k = large ? 1 + (i % 3) : 2 + (i % 3)
    for (let j = 0; j < k; j++) {
      n += 1
      const phase = PHASES[(i + j) % 4]
      const repo = a.repos[0]
      const pr = phase === 'queued' ? 0 : n + 1000
      const outcome = phase === 'done' ? ((i + j) % 3 === 0 ? 'closed' : 'merged') : undefined
      const t = now - ((i * 7 + j * 3) % 50) * hr - j * 13 * min
      out.push({ id: `${a.name}/${repo}/issue-${n}`, agent: a.name, repo, forge: a.forge, issue: n, pr: pr || undefined, title: TITLES[(i + j) % TITLES.length],
        url: `https://github.com/${repo}/issues/${n}`, prUrl: pr ? `https://github.com/${repo}/pull/${pr}` : undefined, phase, outcome,
        created: iso(t - 5 * hr), updated: iso(t), closed: phase === 'done' ? iso(t) : undefined })
    }
  })
  return out.sort((x, y) => Date.parse(y.updated) - Date.parse(x.updated))
}
const JOBS = makeJobs()
function jobsPage(url) {
  const q = url.searchParams
  let list = JOBS.filter((j) => (!q.get('agent') || j.agent === q.get('agent')) && (!q.get('repo') || j.repo === q.get('repo'))
    && (!q.get('phase') || j.phase === q.get('phase')) && (q.get('scope') !== 'mine' || MINE.includes(j.repo)))
  const limit = Number(q.get('limit') ?? 100), off = Number(q.get('cursor') ?? 0)
  const page = list.slice(off, off + limit)
  return { jobs: page, ...(off + limit < list.length ? { next_cursor: String(off + limit) } : {}) }
}
// ---- #425: run log of a job. The first text event carries markup that must show as text (XSS check). ----
function makeRun(agent, job) {
  const big = Array.from({ length: 60 }, (_, i) => `ok   - test ${i + 1}`).join('\n')
  const t = now - 20 * min
  const ev = (i, kind, data) => ({ id: i, agent, repo: '', job, time: iso(t + i * 20000), kind, data })
  return [
    ev(1, 'text', { text: 'I will read the failing test first. <img src=x onerror=alert(1)> <script>alert(2)</script> **not bold**' }),
    ev(2, 'tool_use', { tool: 'Bash', input: '{"command":"npm test -- login"}' }),
    ev(3, 'tool_result', { ok: false, size: 5300, text: big + ' [clipped]' }),
    ev(4, 'tool_use', { tool: 'Edit', input: '{"file_path":"src/login.test.ts","old_string":"timeout: 0","new_string":"timeout: 5000"}' }),
    ev(5, 'tool_result', { ok: true, size: 64, text: 'The file src/login.test.ts has been updated. token=[redacted]' }),
    ev(6, 'text', { text: 'The test waited forever for the slow upstream. A timeout fixes it.' }),
    ev(7, 'result', { ok: true, turns: 6, input_tokens: 18234, output_tokens: 912 }),
    ev(8, 'truncated', { size: 2097152 }),
  ]
}
function runEvents(url) {
  const [, , , agent, job] = url.pathname.split('/')
  const after = Number(url.searchParams.get('after') ?? 0)
  return { events: makeRun(agent, job).filter((e) => e.id > after) }
}
function runStream(req, res, url) {
  res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-store' })
  const sse = (type, data) => res.write(`event: ${type}\ndata: ${JSON.stringify({ type, time: new Date().toISOString(), data })}\n\n`)
  sse('hello', { heartbeatSeconds: 15 })
  const beat = setInterval(() => sse('heartbeat', null), 15000)
  req.on('close', () => clearInterval(beat))
  void url
}
function detail(res, name) {
  const a = D.agents.find((x) => x.name === name)
  if (!a) return send(res, 404, { error: 'not found' })
  const waitingOn = []
  if (a.paused) waitingOn.push({ kind: 'paused', reason: 'paused by a person' })
  else if (a.phase === 'blocked') waitingOn.push({ kind: 'blocked' })
  for (const p of D.approvals.filter((x) => x.agent === name)) waitingOn.push({ kind: 'approval', ref: p.name, job: p.job, repo: p.repo, reason: p.tool, since: p.created, until: p.expires })
  const pz = D.usage.pauses[name]
  if (pz) waitingOn.push({ kind: 'usage-pause', reason: pz.reason, until: iso(pz.until * 1000) })
  const pods = a.replicas ? [{ name: `agent-${name}-0`, phase: a.ready ? 'Running' : 'Pending', ready: !!a.ready, restarts: a.phase === 'blocked' ? 3 : 0, started: iso(now - 4 * hr), ...(a.ready ? {} : { reason: 'ContainerCreating' }) }] : []
  const guide = `# How ${name} works\n\nRepository: ${a.repos[0]} (${a.forge})\nMax. concurrent: ${a.maxConcurrent}\n\n- Assign an issue to the bot user to start a job.\n- Comment on the PR to give feedback; the agent reads new comments.\n- /approve <id> or the console decides an approval request.\n`
  // #505: spec.permission for agentdev-admins only; alice runs in mode auto with rules (one is markup, shown as text)
  const permission = !admin ? undefined : name === 'alice'
    ? { mode: 'auto', autoMode: { allow: ['Install dependencies declared in the repository', '<img src=x onerror=alert(1)>'], softDeny: ['Deploy to production'], environment: [] } }
    : { mode: 'dontAsk' }
  send(res, 200, { ...a, pods, waitingOn, recentJobs: JOBS.filter((j) => j.agent === name).slice(0, 10), guide, permission })
}
function platform() {
  const c = (name, status, detail, command) => ({ name, status, detail, command })
  return { version: '1.1.0-rc.1', checked: iso(Date.now()), components: [
    c('api', 'ok', 'answering', 'kubectl -n agentdev logs deploy/agentdev-api'),
    c('router', 'ok', '1/1 ready', 'kubectl -n coding-agents describe deploy/webhook-router'),
    c('operator', 'ok', '1/1 ready', 'kubectl -n agentdev describe deploy/agentdev-operator'),
    c('claude-proxy', 'degraded', '1/2 ready', 'kubectl -n coding-agents describe deploy/claude-proxy'),
    c('authentik', 'ok', '1/1 ready', 'kubectl -n agentdev describe deploy/authentik-server'),
    c('forge-token', 'ok', "the API's forge credential works", 'kubectl -n agentdev logs deploy/agentdev-api | grep -i forge'),
    c('egress-proxy', 'absent', 'not installed', 'kubectl -n coding-agents describe deploy/egress-proxy'),
    c('agent-pool', 'down', '2 running, 1 pending, 1 failed', 'kubectl -n coding-agents get pods -l app=coding-agent -o wide'),
  ] }
}
const AUDIT = (() => {
  const who = (u) => ({ sub: `s-${u}`, username: u, forgeUser: u })
  const st = (repo, paused, max = 2) => ({ repo, forge: 'github', maxConcurrent: max, scaleToZero: true, paused })
  const e = (m, u, action, agent, result, extra) => ({ time: iso(now - m * min), who: who(u), action, agent, result, before: null, after: null, ...extra })
  return [
    { time: iso(now - 20 * min), who: { sub: '', username: '', forgeUser: '' }, action: 'permission', agent: 'alice', result: 'ok', reason: 'spec.permission of the Agent object changed',
      before: { repo: '', forge: '', maxConcurrent: 0, scaleToZero: false, paused: false, permission: { mode: 'dontAsk' } },
      after: { repo: '', forge: '', maxConcurrent: 0, scaleToZero: false, paused: false, permission: { mode: 'auto', autoMode: { allow: ['Install dependencies declared in the repository'], softDeny: ['Deploy to production'], environment: [] } } } },
    { time: iso(now - 50 * min), who: { sub: '', username: '', forgeUser: '' }, action: 'permission', agent: 'dave', result: 'ok', reason: 'first seen (no earlier entry)',
      before: null, after: { repo: '', forge: '', maxConcurrent: 0, scaleToZero: false, paused: false, permission: { mode: 'auto' } } },
    e(55, 'valentinalexeev', 'patch', 'carol', 'ok', { request: { paused: true }, before: st('acme/web', false), after: st('acme/web', true), grant: 'admin-group' }),
    e(80, 'mia', 'delete', 'gus', 'denied', { reason: 'needs admin access' }),
    e(140, 'lee', 'patch', 'alice', 'ok', { request: { maxConcurrent: 2 }, before: st('acme/shop-api', false, 1), after: st('acme/shop-api', false, 2), grant: 'repo-write' }),
    e(300, 'mia', 'patch', 'bob', 'invalid', { request: { maxConcurrent: 12 }, reason: 'maxConcurrent: 1 to 8' }),
    e(26 * 60, 'valentinalexeev', 'create', 'gus', 'ok', { request: { name: 'gus', repo: 'acme/docs' }, after: st('acme/docs', false, 1), grant: 'admin-group' }),
    e(30 * 60, 'valentinalexeev', 'patch', 'dave', 'ok', { request: { paused: false }, before: st('acme/mobile', true), after: st('acme/mobile', false), grant: 'repo-admin' }),
  ].map((entry, i) => ({ id: 100 - i, entry }))
})()
function audit(url) {
  const ag = url.searchParams.get('agent'), r = url.searchParams.get('result')
  return { entries: AUDIT.filter(({ entry: e }) => (!ag || e.agent === ag) && (!r || e.result === r)) }
}

// ---- #345: live stream (SSE, envelope of images/api/stream.go) ----
let streamOn = true
const clients = new Set()
let seq = 0
function sse(res, type, data) { res.write(`event: ${type}\nid: ${++seq}\ndata: ${JSON.stringify({ type, time: iso(Date.now()), data })}\n\n`) }
function stream(req, res) {
  if (!streamOn) return send(res, 503, { error: 'stream off (mock)' })
  res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-store' })
  sse(res, 'hello', { heartbeatSeconds: 5 })
  const t = setInterval(() => sse(res, 'heartbeat', null), 5000)
  const c = { res, destroy: () => { clearInterval(t); req.socket.destroy() } }
  clients.add(c)
  req.on('close', () => { clearInterval(t); clients.delete(c) })
}
let nNew = 0
function newApproval() {
  nNew += 1
  const a = { ...D.approvals[0], name: `apr-live${nNew}`, id: `live${nNew}`, agent: 'erin', job: `issue-${400 + nNew}`, tool: 'Bash(npm publish)', created: iso(Date.now()), expires: iso(Date.now() + 30 * min) }
  D.approvals.push(a)
  for (const c of clients) sse(c.res, 'approval', a)
}
