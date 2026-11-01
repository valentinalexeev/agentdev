// Pending approvals count for the tab badge (#67).
import { useEffect, useState } from 'react'
import { listApprovals } from './api'

// how often the Pending tab and the badge refresh
export const REFRESH_MS = 15000

export function usePendingCount(token: string): number {
  const [n, setN] = useState(0)
  useEffect(() => {
    let alive = true
    const load = () => listApprovals(token, 'pending').then((l) => alive && setN(l.length)).catch(() => {})
    load()
    const t = setInterval(load, REFRESH_MS)
    return () => { alive = false; clearInterval(t) }
  }, [token])
  return n
}

