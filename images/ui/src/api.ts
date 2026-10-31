// Types and calls for the agentdev API (#46). Same origin: nginx proxies /api to the API service.
export interface Agent {
  name: string
  repos: string[] | null
  forge?: string
  phase: 'running' | 'starting' | 'stopped' | 'paused' | 'blocked' | 'invalid'
  replicas: number
  ready: number
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

export class Unauthorized extends Error {}

async function get(path: string, token: string) {
  const r = await fetch(path, { headers: { Authorization: `Bearer ${token}` } })
  if (r.status === 401) throw new Unauthorized('login expired')
  if (!r.ok) throw new Error(`API: ${r.status}`)
  return r.json()
}

export async function listAgents(token: string): Promise<Agent[]> {
  return (await get('/api/agents', token)).agents
}

export async function listSecurityEvents(token: string, f: EventFilter): Promise<{ events: SecurityEvent[]; next_cursor?: string }> {
  const q = new URLSearchParams()
  for (const [k, v] of Object.entries(f)) if (v) q.set(k, v)
  return get(`/api/security-events?${q}`, token)
}
