// Console data (#342, #339): one poll for the facts every page needs (agents, pending approvals, usage, recent
// security events), the derived two-axis agent state, the client-built inbox, the project scope and the connection
// state. A failed poll keeps the last good data; the page marks its age instead of showing an error.
import { useCallback, useEffect, useMemo, useState } from 'react'
import {
  getMe, getUsage, listAgents, listApprovals, listSecurityEvents, onCallResult, Unauthorized,
  type Agent, type Approval, type CallResult, type SecurityEvent, type Usage,
} from './api'
import { openStream } from './stream'
import { ADMIN_GROUP, agentView, buildInbox, DOWN_MS, POLL_LIVE_MS, POLL_MS, STALE_MS, type Scope } from './model'
import { Ctx, type Conn, type Data } from './useData'

interface Snapshot { agents: Agent[]; pending: Approval[]; usage: Usage | null; events: SecurityEvent[]; at: number }


function readScope(): Scope {
  try { return localStorage.getItem('ad-scope') ?? 'mine' } catch { return 'mine' }
}

export function DataProvider({ token, onExpired, children }: { token: string; onExpired: () => void; children: React.ReactNode }) {
  const [snap, setSnap] = useState<Snapshot>({ agents: [], pending: [], usage: null, events: [], at: 0 })
  const [scope, setScopeState] = useState<Scope>(readScope)
  const [last, setLast] = useState<{ r: CallResult; since: number }>({ r: 'ok', since: 0 })
  const [now, setNow] = useState(() => Date.now())
  const [tick, setTick] = useState(0)
  const [mine, setMine] = useState<string[] | null>(null)
  const [admin, setAdmin] = useState(false)
  useEffect(() => {
    getMe(token).then((m) => { setMine(Array.isArray(m.repos) ? m.repos : null); setAdmin((m.groups ?? []).includes(ADMIN_GROUP)) })
      .catch(() => setMine(null))
  }, [token])

  useEffect(() => onCallResult((r) => setLast((p) => (p.r === r ? p : { r, since: Date.now() }))), [])
  useEffect(() => {
    const on = () => setLast({ r: navigator.onLine ? 'ok' : 'offline', since: Date.now() })
    window.addEventListener('online', on); window.addEventListener('offline', on)
    return () => { window.removeEventListener('online', on); window.removeEventListener('offline', on) }
  }, [])

  // Live stream (#345): an event from the stream reads the snapshot again at once (debounced), so a new approval shows
  // in about a second. While the stream is open, the poll is only a safety net (POLL_LIVE_MS); while it is down, the
  // console polls every POLL_MS as before, and the stream reconnects with backoff.
  const [live, setLive] = useState(false)
  useEffect(() => {
    let alive = true
    let poll: ReturnType<typeof setInterval> | undefined
    let soon: ReturnType<typeof setTimeout> | undefined
    const load = async () => {
      try {
        const [agents, pending, usage, ev] = await Promise.all([
          listAgents(token), listApprovals(token, 'pending'),
          getUsage(token).catch((e) => { if (e instanceof Unauthorized) throw e; return null }),
          listSecurityEvents(token, { since: '168h' }).catch((e) => { if (e instanceof Unauthorized) throw e; return { events: [] } }),
        ])
        if (alive) setSnap({ agents, pending, usage, events: ev.events, at: Date.now() })
      } catch (e) {
        if (e instanceof Unauthorized) onExpired()
        // anything else: keep the last good data; the connection state says why
      }
    }
    // change the poll only when the stream opens or closes: a failed retry must not restart the 15 s timer
    let period = 0
    const every = (ms: number) => { if (ms === period) return; period = ms; clearInterval(poll); poll = setInterval(load, ms) }
    load()
    every(POLL_MS)
    const stop = openStream(token, {
      onOpen: () => { if (!alive) return; setLive(true); every(POLL_LIVE_MS); load() },
      onMessage: (m) => {
        if (!alive) return
        // a heartbeat proves that nothing changed: the data on the page is current
        if (m.type === 'heartbeat') { setSnap((p) => (p.at ? { ...p, at: Date.now() } : p)); return }
        clearTimeout(soon); soon = setTimeout(load, 250)
      },
      onDown: () => { if (!alive) return; if (period !== POLL_MS) load(); setLive(false); every(POLL_MS) },
      onUnauthorized: () => { if (alive) onExpired() },
    })
    const c = setInterval(() => setNow(Date.now()), 10000)
    return () => { alive = false; stop(); clearTimeout(soon); clearInterval(poll); clearInterval(c) }
  }, [token, onExpired, tick])

  const setScope = useCallback((s: Scope) => {
    setScopeState(s)
    try { localStorage.setItem('ad-scope', s) } catch { /* storage blocked */ }
  }, [])
  const reload = useCallback(() => setTick((n) => n + 1), [])

  const data = useMemo<Data>(() => {
    const all = snap.agents.map((a) => agentView(a, snap.pending, snap.usage?.pauses, snap.at || now))
    const allProjects = [...new Set(snap.agents.flatMap((a) => a.repos ?? []))].sort()
    const known = (scope.startsWith('p:') && allProjects.includes(scope.slice(2))) || (scope === 'mine' && mine !== null)
    const eff = known ? scope : ''
    const inScopeRepo = (repo: string) => !eff || (eff === 'mine' ? mine!.includes(repo) : repo === eff.slice(2))
    const agents = all.filter((a) => !eff || (a.repos ?? []).some(inScopeRepo))
    const names = new Set(agents.map((a) => a.name))
    const inScopeAgent = (n: string) => !eff || names.has(n)
    const pending = snap.pending.filter((p) => inScopeRepo(p.repo))
    const fullInbox = buildInbox(all, snap.pending)
    const inbox = fullInbox.filter((i) => inScopeRepo(i.repo))
    let conn: Conn = 'live'
    if (last.r === 'unauthorized') conn = 'signedout'
    else if (last.r === 'offline') conn = 'offline'
    else if (last.r === 'down' || (last.r === 'starting' && now - last.since > DOWN_MS)) conn = 'down'
    else if (last.r === 'starting') conn = 'starting'
    else if (snap.at && now - snap.at > STALE_MS) conn = 'stale'
    return {
      ready: snap.at > 0, at: snap.at, agents, allAgents: all, pending, inbox, hiddenInbox: fullInbox.length - inbox.length,
      usage: snap.usage, events: snap.events.filter((e) => inScopeAgent(e.agent)),
      projects: allProjects.filter(inScopeRepo), allProjects, scope: eff, setScope, inScopeRepo, inScopeAgent, mine, admin,
      conn, live, failedSince: last.since, reload, now,
    }
  }, [snap, scope, mine, admin, last, live, now, setScope, reload])

  return <Ctx.Provider value={data}>{children}</Ctx.Provider>
}
