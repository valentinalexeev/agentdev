// The console model (#342): two-axis agent state and the client-built inbox. Pure functions, no React.
import type { Agent, Approval, Usage } from './api'

export const POLL_MS = 15000
export const STALE_MS = 60000 // no good update for this long: "updated N min ago"
export const DOWN_MS = 5 * 60000 // still failing after this long: "API unavailable"

export type Life = 'running' | 'starting' | 'stopped' | 'paused' | 'usage-paused' | 'invalid'
export type Attn = 'ok' | 'you' | 'blocked' | 'guardrail' | 'usage'
export const ATTN_LABEL: Record<Attn, string> = { ok: 'ok', you: 'needs you', blocked: 'blocked', guardrail: 'guardrail', usage: 'usage limit' }
export const ATTN_RANK: Record<Attn, number> = { guardrail: 0, blocked: 1, you: 2, usage: 3, ok: 4 }

export interface AgentView extends Agent { life: Life; attn: Attn; why: string; repo: string }

export type ItemKind = 'approval' | 'guardrail' | 'blocked' | 'limit'
export interface InboxItem {
  key: string; kind: ItemKind; title: string; agent: string; repo: string; where: string
  due?: number // expiry, ms
  since?: number // age, ms
  approval?: Approval
}
export const KIND_GROUP: Record<ItemKind, string> = { approval: 'Decide', guardrail: 'Stopped agents', blocked: 'Stopped agents', limit: 'Usage limit' }
export const GROUPS = ['Decide', 'Stopped agents', 'Usage limit']

// Lifecycle from the pods and flags, attention from what waits. The API's one `phase` mixes both; split it here.
export function agentView(a: Agent, pending: Approval[], pauses: Usage['pauses'], at: number): AgentView {
  const usage = pauses?.[a.name] && pauses[a.name].until * 1000 > at
  const life: Life = a.phase === 'invalid' ? 'invalid' : usage ? 'usage-paused' : a.paused || a.phase === 'paused' ? 'paused'
    : a.replicas === 0 ? 'stopped' : a.ready >= a.replicas ? 'running' : 'starting'
  const mine = pending.filter((p) => p.agent === a.name)
  let attn: Attn = 'ok', why = ''
  if ((a.paused || a.phase === 'paused') && (a.securityEvents ?? 0) > 0) {
    attn = 'guardrail'; why = `paused, with ${a.securityEvents} security ${a.securityEvents === 1 ? 'event' : 'events'} in 7 days`
  } else if (a.phase === 'blocked' || a.phase === 'invalid') {
    attn = 'blocked'; why = a.phase === 'invalid' ? 'the Agent object is not valid' : 'the operator marked the agent blocked'
  } else if (mine.length) {
    attn = 'you'; why = `${mine.length} ${mine.length === 1 ? 'approval waits' : 'approvals wait'}`
  } else if (usage) {
    attn = 'usage'; why = `${pauses![a.name].reason}, until ${new Date(pauses![a.name].until * 1000).toLocaleString()}`
  }
  return { ...a, life, attn, why, repo: (a.repos ?? [])[0] ?? '' }
}

// The inbox: what waits for a person. Sort: the first to expire, then the oldest.
export function buildInbox(agents: AgentView[], pending: Approval[]): InboxItem[] {
  const items: InboxItem[] = pending.map((p) => ({
    key: `a:${p.name}`, kind: 'approval', title: `${p.agent} asks for ${p.source} · ${p.tool}`, agent: p.agent, repo: p.repo,
    where: p.job, due: Date.parse(p.expires), since: Date.parse(p.created), approval: p,
  }))
  for (const a of agents) {
    if (a.attn === 'guardrail') items.push({ key: `g:${a.name}`, kind: 'guardrail', title: `${a.name} is ${a.why}`, agent: a.name, repo: a.repo, where: 'agent' })
    if (a.attn === 'blocked') items.push({ key: `b:${a.name}`, kind: 'blocked', title: `${a.name} is blocked: ${a.why}`, agent: a.name, repo: a.repo, where: 'agent' })
    if (a.life === 'usage-paused') items.push({ key: `l:${a.name}`, kind: 'limit', title: `${a.name} starts no new runs: ${a.why}`, agent: a.name, repo: a.repo, where: 'usage' })
  }
  return items.sort((x, y) => ((x.due ?? Infinity) - (y.due ?? Infinity)) || ((x.since ?? Infinity) - (y.since ?? Infinity)) || x.key.localeCompare(y.key))
}

// Project scope: '' = all projects, 'mine' = repositories where the person has write access, 'p:owner/name' = one.
// "My projects" needs the list from the API (open decision on #342): it is offered when /api/me returns `repos`.
export type Scope = string

