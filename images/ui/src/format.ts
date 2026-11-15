// Helpers shared by the pages (no components here, so fast refresh works).
import type { Approval } from './api'

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
