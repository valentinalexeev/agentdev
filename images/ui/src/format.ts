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

