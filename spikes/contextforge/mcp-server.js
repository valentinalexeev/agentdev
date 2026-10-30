// Minimal MCP server (streamable HTTP, JSON responses, no SDK) for the ContextForge spike (#14).
// TOOLSET picks which tools it exposes, so each "project" gets distinguishable tools:
//   alpha -> alpha_echo, alpha_secret_hint      beta -> beta_echo, beta_time
'use strict';
const http = require('node:http');
const set = process.env.TOOLSET || 'alpha';
const tools = {
  alpha: [
    { name: 'alpha_echo', description: 'Project alpha: echo text back', inputSchema: { type: 'object', properties: { text: { type: 'string' } }, required: ['text'] } },
    { name: 'alpha_secret_hint', description: 'Project alpha only: returns a marker that must never be visible to project beta', inputSchema: { type: 'object', properties: {} } },
  ],
  beta: [
    { name: 'beta_echo', description: 'Project beta: echo text back', inputSchema: { type: 'object', properties: { text: { type: 'string' } }, required: ['text'] } },
    { name: 'beta_time', description: 'Project beta: current UTC time', inputSchema: { type: 'object', properties: {} } },
  ],
}[set];

function handle(msg) {
  const { id, method, params } = msg;
  const ok = (result) => ({ jsonrpc: '2.0', id, result });
  switch (method) {
    case 'initialize':
      return ok({ protocolVersion: params?.protocolVersion || '2025-06-18', capabilities: { tools: {} }, serverInfo: { name: `spike-${set}`, version: '0.1.0' } });
    case 'notifications/initialized': return null;
    case 'ping': return ok({});
    case 'tools/list': return ok({ tools });
    case 'tools/call': {
      const n = params?.name, a = params?.arguments || {};
      const text = n.endsWith('_echo') ? `${set}: ${a.text}` : n === 'alpha_secret_hint' ? 'ALPHA-ONLY-MARKER-7f3a' : n === 'beta_time' ? new Date().toISOString() : null;
      if (text === null) return { jsonrpc: '2.0', id, error: { code: -32602, message: `unknown tool ${n}` } };
      return ok({ content: [{ type: 'text', text }] });
    }
    default: return id === undefined ? null : { jsonrpc: '2.0', id, error: { code: -32601, message: `no method ${method}` } };
  }
}

http.createServer((req, res) => {
  if (req.method === 'GET' && req.url === '/healthz') return res.writeHead(200).end('ok');
  if (req.method !== 'POST') return res.writeHead(405).end();
  let b = ''; req.on('data', (c) => (b += c));
  req.on('end', () => {
    let msg; try { msg = JSON.parse(b); } catch { return res.writeHead(400).end(); }
    const out = Array.isArray(msg) ? msg.map(handle).filter(Boolean) : handle(msg);
    if (out === null || (Array.isArray(out) && !out.length)) return res.writeHead(202).end();
    res.writeHead(200, { 'content-type': 'application/json', 'mcp-session-id': 'spike' }).end(JSON.stringify(out));
  });
}).listen(8080, () => console.log(`spike MCP ${set} on :8080`));
