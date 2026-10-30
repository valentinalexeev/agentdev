// webhook-listener: receives "wake" calls from the cluster's webhook router (#3) and keeps this agent
// registered there.
//
// Forges never call agents directly: images/router verifies forge signatures and routes. Here we only
// check the router's bearer token (WAKE_TOKEN, unique per agent), then append a line to WAKE_FILE;
// agent-loop wakes when the line count grows, and re-reads state from the forge API.
//
// Registration: every REGISTER_EVERY seconds, POST ROUTER_URL/register with this agent's identity and
// forward URL, so a restarted router or agent recovers on its own. Without ROUTER_URL the listener exits
// and the agent just polls.
'use strict';
const http = require('node:http');
const crypto = require('node:crypto');
const fs = require('node:fs');

const env = process.env;
const port = Number(env.WEBHOOK_PORT || 8080);
const wakeFile = env.WAKE_FILE || '/tmp/agent-wake';
const me = env.FORGE_USER || '';
const log = (m) => console.log(`${new Date().toISOString().slice(11, 19)} [${me}] webhook: ${m}`);

if (!env.ROUTER_URL || !env.WAKE_TOKEN || !env.REGISTER_TOKEN || !env.SELF_URL) {
  log('ROUTER_URL, WAKE_TOKEN, REGISTER_TOKEN or SELF_URL not set: webhooks off, polling only');
  process.exit(0);
}
const want = `Bearer ${env.WAKE_TOKEN}`;
const ok = (h) => h && h.length === want.length && crypto.timingSafeEqual(Buffer.from(h), Buffer.from(want));

http.createServer((req, res) => {
  if (req.method === 'GET' && req.url === '/healthz') { res.writeHead(200).end('ok'); return; }
  if (req.method !== 'POST') { res.writeHead(405).end(); return; }
  if (!ok(req.headers.authorization)) { log('rejected wake (token)'); res.writeHead(401).end(); req.resume(); return; }
  let body = '';
  req.on('data', (c) => { if (body.length < 4096) body += c; });
  req.on('end', () => {
    let ev = 'unknown'; try { ev = JSON.parse(body).event || ev; } catch { /* wake anyway */ }
    // one line per wake: the loop compares the line count, so two wakes in the same second are not merged
    fs.appendFileSync(wakeFile, `${Date.now()} ${ev}\n`);
    log(`wake (${ev})`);
    res.writeHead(204).end();
  });
}).listen(port, () => log(`listening on :${port}`));

let registered = false;
function register() {
  const body = JSON.stringify({ agent: env.AGENT_NAME || me, forge: env.FORGE_KIND || 'gitea', user: me,
    repos: (env.REPO || '').split(',').filter(Boolean), url: env.SELF_URL, token: env.WAKE_TOKEN });
  const u = new URL('/register', env.ROUTER_URL);
  const req = http.request({ hostname: u.hostname, port: u.port || 80, path: u.pathname, method: 'POST', timeout: 5000,
    headers: { 'content-type': 'application/json', 'content-length': Buffer.byteLength(body), authorization: `Bearer ${env.REGISTER_TOKEN}` } },
    (res) => {
      res.resume();
      if (res.statusCode === 200) { if (!registered) log(`registered with router ${env.ROUTER_URL}`); registered = true; }
      else { registered = false; log(`router registration: HTTP ${res.statusCode}`); }
    });
  req.on('error', (e) => { if (registered) log(`router unreachable: ${e.message}`); registered = false; });
  req.on('timeout', () => req.destroy(new Error('timeout')));
  req.end(body);
}
register();
// refresh well inside the router's TTL; faster while not yet registered
setInterval(() => register(), Number(env.REGISTER_EVERY || 60) * 1000);
setInterval(() => { if (!registered) register(); }, 5000);
