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
  budgetUsed?: number // Claude tokens in the last 24 h, counted by the credential proxy (#66)
  budgetLimit?: number // tokens per 24 h; 0 = no limit
}

// "12.3k / 100k" or "12.3k" when there is no limit; "" when the proxy reports nothing (#66)
export function formatBudget(a: Pick<Agent, 'budgetUsed' | 'budgetLimit'>): string {
  if (a.budgetUsed === undefined) return ''
  const k = (n: number) => (n >= 1000 ? `${(n / 1000).toFixed(n >= 100000 ? 0 : 1)}k` : `${n}`)
  return a.budgetLimit ? `${k(a.budgetUsed)} / ${k(a.budgetLimit)}` : k(a.budgetUsed)
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

type AgentPatch = Partial<Pick<Agent, 'paused' | 'maxConcurrent' | 'scaleToZero'>>

export class Unauthorized extends Error {}

// ApiError carries the message of the API. The API decides authorization (#57): the UI shows its answer.
class ApiError extends Error {
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

// Approval requests (#67): decided by a forge comment (/approve <id>) or here by a person with write access.
export type ApprovalState = 'pending' | 'allowed' | 'denied' | 'void'

export interface Approval {
  name: string
  id: string
  agent: string
  repo: string
  forge: string
  job: string
  source: 'claude' | 'push' | 'executor'
  tool: string
  input: string // written by the agent: untrusted, shown as text only
  untrusted: boolean
  created: string
  expires: string
  state: ApprovalState
  scope?: 'once' | 'job'
  decidedBy?: string
  decidedVia?: string
  decidedAt?: string
  reason?: string // why it became void
  contentHash: string
}

export type Decision = 'approve' | 'approve-job' | 'deny'

// The content hash binds the decision to what the person saw: a changed request is refused (409).
export async function decideApproval(token: string, a: Approval, decision: Decision): Promise<Approval> {
  return (await call(token, 'POST', `/api/approvals/${encodeURIComponent(a.name)}/decision`,
    { decision, contentHash: a.contentHash })).json()
}

export async function listApprovals(token: string, state: 'pending' | 'decided' | 'all'): Promise<Approval[]> {
  return (await (await call(token, 'GET', `/api/approvals?state=${state}`)).json()).approvals
}

// Subscription usage limits (#93). Thresholds in percent; 0 or absent in a project: the global value applies.
export interface Thresholds { fiveHour?: number; weekly?: number }
export interface UsageWindow { utilization: number; resetsAt: number }
export interface SubscriptionState {
  fiveHour: UsageWindow; weekly: UsageWindow; inputTokens: number; outputTokens: number; runs: number; updated: string
}
export interface UsagePause { until: number; reason: string }
export interface Usage {
  global: Required<Thresholds>
  projects?: Record<string, Thresholds>
  effective: Record<string, Required<Thresholds>>
  state?: Record<string, SubscriptionState> // by Claude Secret
  pauses?: Record<string, UsagePause> // by agent
}

export async function getUsage(token: string): Promise<Usage> {
  return (await call(token, 'GET', '/api/usage')).json()
}

export async function setGlobalLimits(token: string, t: Thresholds): Promise<void> {
  await call(token, 'PUT', '/api/usage/global', t)
}

// null clears the project's value for that window
export async function setProjectLimits(token: string, repo: string, t: { fiveHour: number | null; weekly: number | null }): Promise<void> {
  const [o, r] = repo.split('/')
  await call(token, 'PUT', `/api/usage/projects/${encodeURIComponent(o)}/${encodeURIComponent(r)}`, t)
}

export function pctError(n: number): string {
  return Number.isInteger(n) && n >= 1 && n <= 100 ? '' : 'limit: 1 to 100 %'
}

// Project secrets (#202): write-only. The API never returns a value; the UI only sends one.
export interface ProjectSecret {
  name: string
  scope: 'project' | 'agent'
  project?: string
  agent?: string
  forge: string
  mode: 'broker' | 'env'
  upstream?: string
  header?: string
  scheme?: 'bearer' | 'basic' | 'raw'
  envName?: string
  description?: string
  phase: 'pending' | 'ready' | 'error'
  message?: string
  syncedAt?: string
}

export interface NewSecret {
  name: string
  scope: 'project' | 'agent'
  project?: string
  agent?: string
  forge?: 'gitea' | 'github'
  mode: 'broker' | 'env'
  broker?: { upstream: string; header?: string; scheme?: string }
  env?: { name: string; riskAccepted: boolean }
  description?: string
  value: string
}

export async function listSecrets(token: string): Promise<ProjectSecret[]> {
  return (await (await call(token, 'GET', '/api/secrets')).json()).secrets
}

export async function createSecret(token: string, s: NewSecret): Promise<void> {
  await call(token, 'POST', '/api/secrets', s)
}

export async function rotateSecret(token: string, name: string, value: string): Promise<void> {
  await call(token, 'PUT', `/api/secrets/${encodeURIComponent(name)}/value`, { value })
}

export async function deleteSecret(token: string, name: string): Promise<void> {
  await call(token, 'DELETE', `/api/secrets/${encodeURIComponent(name)}`)
}

// same patterns as images/api/secrets.go; the API checks again
const secretNameRe = /^[a-z0-9]([-a-z0-9]{0,48}[a-z0-9])?$/
const upstreamRe = /^https:\/\/[a-z0-9]([-a-z0-9.]*[a-z0-9])?(:[0-9]{1,5})?(\/[A-Za-z0-9._~/-]*)?$/
const envNameRe = /^[A-Z][A-Z0-9_]{0,63}$/
const envReservedRe = /^(AGENTDEV|ANTHROPIC|CLAUDE|FORGE|GIT|GH|GITHUB|SSH|KUBE|KUBERNETES)_/
const envReserved = ['PATH', 'HOME', 'HTTP_PROXY', 'HTTPS_PROXY', 'NO_PROXY', 'LD_PRELOAD', 'LD_LIBRARY_PATH', 'NODE_OPTIONS', 'BASH_ENV', 'ENV']

export function secretNameError(s: string): string {
  return secretNameRe.test(s) ? '' : 'name: lowercase letters, digits and -, max. 50 characters'
}

export function upstreamError(s: string): string {
  return upstreamRe.test(s) && s.length <= 300 ? '' : 'upstream: an https URL such as https://npm.pkg.github.com'
}

export function envNameError(s: string): string {
  if (!envNameRe.test(s)) return 'variable: upper-case letters, digits and _'
  if (envReservedRe.test(s) || envReserved.includes(s)) return `variable: ${s} is used by agentdev`
  return ''
}
