// webhook-listener: wakes the agent loop when the forge sends a webhook (#3).
//
// The payload is never used for content: the loop re-reads state from the forge API, so a forged or
// replayed delivery can at most cause one extra poll. Deliveries must still carry a valid HMAC-SHA256
// signature (Gitea: X-Gitea-Signature, GitHub: X-Hub-Signature-256) made with WEBHOOK_SECRET.
//
// On a valid delivery it touches WAKE_FILE; agent-loop sleeps until that file changes or POLL_SECONDS pass.
'use strict';
const http = require('node:http');
const crypto = require('node:crypto');
const fs = require('node:fs');

const port = Number(process.env.WEBHOOK_PORT || 8080);
const secret = process.env.WEBHOOK_SECRET || '';
const wakeFile = process.env.WAKE_FILE || '/tmp/agent-wake';
const me = process.env.FORGE_USER || '';
const log = (m) => console.log(`${new Date().toISOString().slice(11, 19)} [${me}] webhook: ${m}`);

if (!secret) { log('WEBHOOK_SECRET not set, listener disabled'); process.exit(0); }

function validSignature(body, header) {
  if (!header) return false;
  const given = header.replace(/^sha256=/, '');
  const want = crypto.createHmac('sha256', secret).update(body).digest('hex');
  return given.length === want.length && crypto.timingSafeEqual(Buffer.from(given), Buffer.from(want));
}

http.createServer((req, res) => {
  if (req.method === 'GET' && req.url === '/healthz') { res.writeHead(200).end('ok'); return; }
  if (req.method !== 'POST') { res.writeHead(405).end(); return; }
  const chunks = [];
  let size = 0;
  req.on('data', (c) => { size += c.length; if (size > 5e6) req.destroy(); else chunks.push(c); });
  req.on('end', () => {
    const body = Buffer.concat(chunks);
    const sig = req.headers['x-gitea-signature'] || req.headers['x-hub-signature-256'];
    if (!validSignature(body, sig)) { log('rejected delivery with bad signature'); res.writeHead(401).end(); return; }
    const event = req.headers['x-gitea-event'] || req.headers['x-github-event'] || 'unknown';
    // one line per delivery: the loop compares the line count, so two wakes in the same second are not merged
    fs.appendFileSync(wakeFile, `${Date.now()} ${event}\n`);
    log(`wake (${event})`);
    res.writeHead(204).end();
  });
}).listen(port, () => log(`listening on :${port}`));
