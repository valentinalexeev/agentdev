// Types and calls for the agentdev API (#46). Same origin: nginx proxies /api to the API service.
export interface Agent {
  name: string
  repos: string[] | null
  forge?: string
  phase: 'running' | 'starting' | 'stopped' | 'blocked'
  replicas: number
  ready: number
}

export class Unauthorized extends Error {}

export async function listAgents(token: string): Promise<Agent[]> {
  const r = await fetch('/api/agents', { headers: { Authorization: `Bearer ${token}` } })
  if (r.status === 401) throw new Unauthorized('login expired')
  if (!r.ok) throw new Error(`API: ${r.status}`)
  return (await r.json()).agents
}
