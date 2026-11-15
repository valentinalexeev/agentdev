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
  budgetUsed?: number // Claude cost units in the last 24 h, counted by the credential proxy (#66, #196)
  budgetLimit?: number // units per 24 h; 0 = no limit
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

// Connection state (#339): every call reports its result, so the console can show live, starting, unavailable or
// signed out without a separate health check. 'starting': the ingress answers 502, 503 or 404 for the API while the
// platform scales up from zero. 'offline': the browser has no network.
export type CallResult = 'ok' | 'starting' | 'down' | 'offline' | 'unauthorized'
const listeners = new Set<(r: CallResult) => void>()
export function onCallResult(f: (r: CallResult) => void): () => void {
  listeners.add(f)
  return () => { listeners.delete(f) }
}
const report = (r: CallResult) => listeners.forEach((f) => f(r))

async function call(token: string, method: string, path: string, body?: unknown): Promise<Response> {
  const headers: Record<string, string> = { Authorization: `Bearer ${token}` }
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  let r: Response
  try {
    r = await fetch(path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) })
  } catch (e) {
    report(navigator.onLine === false ? 'offline' : 'down')
    throw e
  }
  if (r.status === 401) { report('unauthorized'); throw new Unauthorized('login expired') }
  // a JSON error from the API itself is an answer; an HTML 502/503/404 is the ingress while the API is away
  const fromApi = (r.headers.get('content-type') ?? '').includes('json')
  if (!fromApi && [404, 502, 503, 504].includes(r.status)) report('starting')
  else if (r.status >= 500 && !fromApi) report('down')
  else report('ok')
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

// The caller (#57). `repos` (repositories with write access) is the proposal on #342 for "My projects"; an API
// without it leaves the scope at all projects or one project.
interface Me { preferred_username?: string; email?: string; groups?: string[]; repos?: string[] }

export async function getMe(token: string): Promise<Me> {
  return (await call(token, 'GET', '/api/me')).json()
}

// Notifications (#347): Web Push subscriptions of this user and the settings per type and project.
export type NotifyType = 'approvals' | 'stopped' | 'usage'
export interface NotifyMute { project: string; type: NotifyType | '*' }
export interface NotifySettings {
  channels: string[] // on for this instance; empty: notifications are off
  types: Record<NotifyType, boolean>
  mutes: NotifyMute[]
  projects: string[] // projects the user may mute
  subscriptions: string[] // endpoints of the user's devices
  snoozeMinutes: number
  vapidPublicKey?: string
}

export async function getNotifySettings(token: string): Promise<NotifySettings> {
  return (await call(token, 'GET', '/api/notify/settings')).json()
}

export async function putNotifySettings(token: string, s: { types: Partial<Record<NotifyType, boolean>>; mutes: NotifyMute[] }): Promise<NotifySettings> {
  return (await call(token, 'PUT', '/api/notify/settings', s)).json()
}

export async function addPushSubscription(token: string, sub: PushSubscriptionJSON): Promise<void> {
  await call(token, 'POST', '/api/push/subscriptions', { endpoint: sub.endpoint, keys: sub.keys })
}

export async function removePushSubscription(token: string, endpoint: string): Promise<void> {
  await call(token, 'DELETE', `/api/push/subscriptions?endpoint=${encodeURIComponent(endpoint)}`)
}

// Jobs (#345): one issue or pull request of an agent, from the forge and the approvals, never from agent output.
// `title` is written by people or the agent on the forge: untrusted, render as text.
export type JobPhase = 'queued' | 'build' | 'review' | 'done'
export interface Job {
  id: string
  agent: string
  repo: string
  forge: string
  issue?: number
  pr?: number
  title: string
  url?: string
  prUrl?: string
  phase: JobPhase
  outcome?: 'merged' | 'closed' | string
  created: string
  updated: string
  closed?: string
}

// Agent detail (#344): the drawer. 404 when the person may not see the agent.
export interface PodView { name: string; phase: string; ready: boolean; restarts: number; started?: string; reason?: string }
export interface WaitingItem {
  kind: 'approval' | 'usage-pause' | 'paused' | 'blocked' | 'invalid' | string
  ref?: string; job?: string; repo?: string; reason?: string; since?: string; until?: string
}
export interface AgentDetail extends Agent {
  pods: PodView[]
  waitingOn: WaitingItem[]
  recentJobs: Job[]
  guide?: string // the harness guide, written by the operator
}

export async function getAgentDetail(token: string, name: string): Promise<AgentDetail> {
  return (await call(token, 'GET', `/api/agents/${encodeURIComponent(name)}`)).json()
}

// Platform health (#344), agentdev-admins only: 403 for everyone else.
export type Health = 'ok' | 'degraded' | 'down' | 'absent' | 'unknown'
export interface PlatformComponent { name: string; status: Health; detail: string; command: string }
export interface Platform { version: string; checked: string; components: PlatformComponent[] }

export async function getPlatform(token: string): Promise<Platform> {
  return (await call(token, 'GET', '/api/platform')).json()
}

// Audit trail of the write API (#91), agentdev-admins only.
export type AuditResult = 'ok' | 'denied' | 'invalid' | 'error' | 'unauthenticated'
export interface AuditState { repo: string; forge: string; claudeSecret?: string; maxConcurrent: number; scaleToZero: boolean; paused: boolean }
export interface AuditEntry {
  time: string
  who: { sub: string; username: string; forgeUser: string; groups?: string[] }
  action: 'patch' | 'create' | 'delete' | string
  agent: string
  result: AuditResult
  reason?: string
  request?: unknown
  before: AuditState | null
  after: AuditState | null
  grant?: string
  keepState?: boolean
  remote?: string
}
export interface AuditFilter { agent?: string; result?: AuditResult | ''; since?: string; cursor?: string }

export async function listAudit(token: string, f: AuditFilter): Promise<{ entries: { id: number; entry: AuditEntry }[]; next_cursor?: string }> {
  const q = new URLSearchParams()
  for (const [k, v] of Object.entries(f)) if (v) q.set(k, v)
  return (await call(token, 'GET', `/api/audit?${q}`)).json()
}

// The HTTP status of a failed call (403: not allowed, 404: not found or not visible), 0 when it has none.
export function statusOf(e: unknown): number {
  return e instanceof ApiError ? e.status : 0
}

// GET /api/jobs with the scope filters. Reads every page (500 per page, at most 10 pages).
export async function listJobs(token: string, f: { agent?: string; repo?: string; scope?: string; phase?: JobPhase }): Promise<{ jobs: Job[]; unavailable?: string[] }> {
  const jobs: Job[] = []
  let unavailable: string[] | undefined
  let cursor = ''
  for (let i = 0; i < 10; i++) {
    const q = new URLSearchParams({ limit: '500' })
    for (const [k, v] of Object.entries(f)) if (v) q.set(k, v)
    if (cursor) q.set('cursor', cursor)
    const r = await (await call(token, 'GET', `/api/jobs?${q}`)).json()
    jobs.push(...(r.jobs ?? []))
    unavailable = r.unavailable ?? unavailable
    if (!r.next_cursor) break
    cursor = r.next_cursor
  }
  return { jobs, unavailable }
}
