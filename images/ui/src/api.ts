// Types and calls for the agentdev API (#46, #57). Same origin: nginx proxies /api to the API service.
export interface Agent {
  name: string
  repos: string[] | null
  forge?: string
  phase: 'running' | 'starting' | 'stopped' | 'paused' | 'blocked' | 'invalid'
  replicas: number
  ready: number
  managed?: boolean // an Agent object exists; only managed agents can be changed
  maxConcurrent?: number
  scaleToZero?: boolean
  paused?: boolean
  securityEvents?: number // last 7 days (#70)
}

export type Severity = 'low' | 'medium' | 'high'

// agentdev.security/v1 (docs/security-model.md, "Event schema")
export interface SecurityEvent {
  id: number
  schema: string
  time: string
  agent: string
  job?: string
  source: string
  rule: string
  severity: Severity
  action: string
}

export interface EventFilter {
  agent?: string
  severity?: Severity | ''
  since?: string // RFC 3339 or a duration such as 24h
  cursor?: string
}

export interface NewAgent {
  name: string
  repo: string
  forge: 'gitea' | 'github'
  maxConcurrent: number
  scaleToZero: boolean
}

export type AgentPatch = Partial<Pick<Agent, 'paused' | 'maxConcurrent' | 'scaleToZero'>>

export class Unauthorized extends Error {}

// ApiError carries the message of the API. The API decides authorization (#57): the UI shows its answer.
export class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

async function call(token: string, method: string, path: string, body?: unknown): Promise<Response> {
  const headers: Record<string, string> = { Authorization: `Bearer ${token}` }
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  const r = await fetch(path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) })
  if (r.status === 401) throw new Unauthorized('login expired')
  if (!r.ok) {
    let msg = ''
    try { msg = (await r.json()).error ?? '' } catch { /* not JSON */ }
    if (r.status === 503) msg = 'permission check unavailable' + (msg ? `: ${msg}` : '')
    throw new ApiError(r.status, msg || `API: ${r.status}`)
  }
  return r
}

export async function listAgents(token: string): Promise<Agent[]> {
  return (await (await call(token, 'GET', '/api/agents')).json()).agents
}

export async function listSecurityEvents(token: string, f: EventFilter): Promise<{ events: SecurityEvent[]; next_cursor?: string }> {
  const q = new URLSearchParams()
  for (const [k, v] of Object.entries(f)) if (v) q.set(k, v)
  return (await call(token, 'GET', `/api/security-events?${q}`)).json()
}

export async function patchAgent(token: string, name: string, patch: AgentPatch): Promise<void> {
  await call(token, 'PATCH', `/api/agents/${encodeURIComponent(name)}`, patch)
}

export async function createAgent(token: string, a: NewAgent): Promise<void> {
  await call(token, 'POST', '/api/agents', a)
}

export async function deleteAgent(token: string, name: string): Promise<void> {
  await call(token, 'DELETE', `/api/agents/${encodeURIComponent(name)}`)
}

// Client-side checks: the same patterns as images/api/write.go. The API checks again.
const nameRe = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/
const repoSegRe = /^[A-Za-z0-9_][A-Za-z0-9_.-]*$/

export function nameError(s: string): string {
  if (!nameRe.test(s) || s.length > 40) return 'name: lowercase letters, digits and -, max. 40 characters'
  return ''
}

export function repoError(s: string): string {
  const parts = s.split('/')
  const ok = parts.length === 2 && parts.every((p) =>
    p.length <= 100 && repoSegRe.test(p) && !p.endsWith('.') && !p.includes('..'))
  return ok ? '' : 'repository: owner/name'
}

export function maxConcurrentError(n: number): string {
  return Number.isInteger(n) && n >= 1 && n <= 8 ? '' : 'max. concurrent: 1 to 8'
}
