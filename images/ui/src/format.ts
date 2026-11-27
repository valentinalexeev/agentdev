// Helpers shared by the pages (no components here, so fast refresh works).
import type { AgentPermission, Approval, AuditEntry, Job } from './api'

// lane hue from the name: x = (x * 31 + charCode) >>> 0, lane x % 6 + 1 (design-system.md, "Agent identity")
export function lane(name: string): number {
  let x = 0
  for (const c of name) x = (x * 31 + c.charCodeAt(0)) >>> 0
  return (x % 6) + 1
}

export function jobLink(a: Approval): string {
  const [kind, num] = a.job.split('-')
  return `${a.repo}#${num}${kind === 'pr' ? ' (PR)' : ''}`
}


// "owner/name" -> "name"
export function short(repo: string): string {
  return repo.split('/')[1] ?? repo
}

export function ago(t: number, now: number): string {
  const m = Math.max(0, Math.round((now - t) / 60e3))
  return m < 60 ? `${m} min ago` : m < 2880 ? `${Math.round(m / 60)} h ago` : `${Math.round(m / 1440)} d ago`
}

export function left(t: number, now: number): string {
  const m = Math.round((t - now) / 60e3)
  if (m <= 0) return 'expired'
  return m < 60 ? `${m} min left` : `${Math.floor(m / 60)} h ${m % 60} min left`
}

export function kTokens(n: number): string {
  return n >= 1e6 ? `${(n / 1e6).toFixed(1)}M` : n >= 1e3 ? `${Math.round(n / 1e3)}k` : `${n}`
}

export const GROUP_AT = 10 // a list groups itself above this many items (docs/console-ux.md, "At scale")
export const CAP = 4 // items per group before "Show N more"

// The outcome of a finished job, in words (merged, closed without merge, closed with no change)
export function outcomeText(j: Job): string {
  if (j.phase !== 'done') return ''
  return j.outcome === 'merged' ? 'merged' : j.outcome === 'closed' ? (j.pr ? 'closed without merge' : 'closed, no change') : (j.outcome || 'done')
}

// The verb of an entry: a patch that only flips `paused` is a pause or a resume.
export function verb(e: AuditEntry): string {
  if (e.action === 'patch') {
    const req = (e.request && typeof e.request === 'object' ? e.request : {}) as Record<string, unknown>
    const keys = Object.keys(req)
    if (keys.length === 1 && keys[0] === 'paused') return req.paused ? 'pause' : 'resume'
    return 'change'
  }
  return e.action
}

// runLogHash (#425): the hash that opens the job drawer on its Run tab. The job name is the last part of the job id
// (<agent>/<owner>/<repo>/issue-N or pr-N).
export function runLogHash(j: { id: string; agent: string; repo: string }): string {
  const name = j.id.split('/').pop() ?? ''
  const q = new URLSearchParams({ job: `${j.agent}/${name}`, jrepo: j.repo })
  return `#work?${q}`
}

// permissionText (#505): one line for a permission mode, e.g. "auto (2 allow, 1 soft deny, 0 environment rules)".
export function permissionText(p?: AgentPermission): string {
  if (!p) return 'unknown'
  if (!p.autoMode) return p.mode === 'auto' ? 'auto (default rules)' : p.mode
  const r = p.autoMode
  return `${p.mode} (${r.allow.length} allow, ${r.softDeny.length} soft deny, ${r.environment.length} environment rules)`
}
