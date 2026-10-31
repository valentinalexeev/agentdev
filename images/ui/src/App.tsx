import { useCallback, useEffect, useRef, useState } from 'react'
import type { UserManager, User } from './auth'
import { listAgents, listSecurityEvents, Unauthorized, type Agent, type EventFilter, type SecurityEvent } from './api'

export function App({ um }: { um: UserManager }) {
  const [user, setUser] = useState<User | null | undefined>(undefined)
  const [error, setError] = useState<string>('')
  const handled = useRef(false)

  const signIn = useCallback(() => {
    um.signinRedirect().catch((e) => setError(`cannot reach the sign-in server: ${e.message}`))
  }, [um])
  // ends the authentik session too, then comes back to the UI
  const signOut = useCallback(() => {
    um.signoutRedirect().catch(() => um.removeUser().then(() => setUser(null)))
  }, [um])
  const onExpired = useCallback(() => { um.removeUser().then(() => setUser(null)) }, [um])

  useEffect(() => {
    if (window.location.pathname === '/callback') {
      if (handled.current) return // StrictMode runs effects twice in dev; the code is single-use
      handled.current = true
      um.signinRedirectCallback()
        .then((u) => { window.history.replaceState({}, '', '/'); setUser(u) })
        .catch((e) => setError(`login failed: ${e.message}`))
      return
    }
    um.getUser().then((u) => setUser(u && !u.expired ? u : null))
  }, [um])

  if (error) return <Shell><p className="error">{error}</p><button onClick={() => signIn()}>Try again</button></Shell>
  if (user === undefined) return <Shell><p>Loading…</p></Shell>
  if (!user) {
    return (
      <Shell>
        <p>Sign in with your agentdev account.</p>
        <button onClick={() => signIn()}>Sign in</button>
      </Shell>
    )
  }
  return (
    <Shell user={user} onLogout={signOut}>
      <Pages token={user.access_token} onExpired={onExpired} />
    </Shell>
  )
}

// two pages, chosen by the URL hash (#security?agent=x), so a link to one agent's events can be shared
function usePage(): [string, URLSearchParams, (p: string) => void] {
  const read = () => {
    const [p, q] = window.location.hash.replace(/^#/, '').split('?')
    return [p || 'agents', new URLSearchParams(q ?? '')] as const
  }
  const [state, setState] = useState(read)
  useEffect(() => {
    const on = () => setState(read())
    window.addEventListener('hashchange', on)
    return () => window.removeEventListener('hashchange', on)
  }, [])
  return [state[0], state[1], (p) => { window.location.hash = p }]
}

function Pages({ token, onExpired }: { token: string; onExpired: () => void }) {
  const [page, params, go] = usePage()
  return (
    <>
      <nav>
        <button className={`tab ${page === 'agents' ? 'on' : ''}`} onClick={() => go('agents')}>Agents</button>
        <button className={`tab ${page === 'security' ? 'on' : ''}`} onClick={() => go('security')}>Security</button>
      </nav>
      {page === 'security'
        ? <Security key={params.get('agent') ?? ''} token={token} onExpired={onExpired} initialAgent={params.get('agent') ?? ''} />
        : <Overview token={token} onExpired={onExpired} />}
    </>
  )
}

function Shell({ user, onLogout, children }: { user?: User; onLogout?: () => void; children: React.ReactNode }) {
  return (
    <>
      <header>
        <strong>agentdev</strong>
        {user && (
          <span className="who">
            {user.profile.preferred_username ?? user.profile.email}
            <button className="link" onClick={onLogout}>Sign out</button>
          </span>
        )}
      </header>
      <main>{children}</main>
    </>
  )
}

function Overview({ token, onExpired }: { token: string; onExpired: () => void }) {
  const [agents, setAgents] = useState<Agent[] | null>(null)
  const [error, setError] = useState('')

  const load = useCallback(() => {
    listAgents(token)
      .then((a) => { setAgents(a); setError('') })
      .catch((e) => (e instanceof Unauthorized ? onExpired() : setError(e.message)))
  }, [token, onExpired])

  useEffect(() => {
    load()
    const t = setInterval(load, 15000) // read-only overview: a light poll is enough until the API has events
    return () => clearInterval(t)
  }, [load])

  if (error) return <p className="error">{error}</p>
  if (!agents) return <p>Loading agents…</p>
  if (agents.length === 0) return <p>No agents yet. Connect a project with <code>scripts/bootstrap-project.sh</code>.</p>
  return (
    <>
      <h1>Agents</h1>
      <table>
        <thead><tr><th>Agent</th><th>Phase</th><th>Repositories</th><th>Forge</th><th>Security (7 days)</th></tr></thead>
        <tbody>
          {agents.map((a) => (
            <tr key={a.name}>
              <td>{a.name}</td>
              <td><span className={`phase ${a.phase}`}>{a.phase}</span></td>
              <td>{(a.repos ?? []).join(', ')}</td>
              <td>{a.forge ?? ''}</td>
              <td>
                {a.securityEvents
                  ? <a className="badge" href={`#security?agent=${encodeURIComponent(a.name)}`}>{a.securityEvents}</a>
                  : <span className="muted">0</span>}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </>
  )
}

const SINCE: [string, string][] = [['24h', 'last 24 hours'], ['168h', 'last 7 days'], ['720h', 'last 30 days'], ['', 'all']]

function Security({ token, onExpired, initialAgent }: { token: string; onExpired: () => void; initialAgent: string }) {
  const [filter, setFilter] = useState<EventFilter>({ agent: initialAgent, severity: '', since: '168h' })
  // the loaded page belongs to one filter; a result for an older filter is shown as loading
  const [result, setResult] = useState<{ for: EventFilter; events: SecurityEvent[]; next?: string } | null>(null)
  const [error, setError] = useState('')
  const fail = useCallback((e: Error) => (e instanceof Unauthorized ? onExpired() : setError(e.message)), [onExpired])

  useEffect(() => {
    listSecurityEvents(token, filter)
      .then((r) => { setResult({ for: filter, events: r.events, next: r.next_cursor }); setError('') })
      .catch(fail)
  }, [token, filter, fail])

  const events = result && result.for === filter ? result.events : null
  const next = result && result.for === filter ? result.next : undefined
  const more = () => {
    if (!next) return
    listSecurityEvents(token, { ...filter, cursor: next })
      .then((r) => setResult((p) => (p && p.for === filter ? { ...p, events: [...p.events, ...r.events], next: r.next_cursor } : p)))
      .catch(fail)
  }

  return (
    <>
      <h1>Security events</h1>
      <p className="muted">Guardrails that refused an action. High severity, or too many events in one job, pause the agent.</p>
      <div className="filters">
        <label>Agent <input value={filter.agent ?? ''} placeholder="all"
          onChange={(e) => setFilter({ ...filter, agent: e.target.value.trim() })} /></label>
        <label>Severity <select value={filter.severity ?? ''}
          onChange={(e) => setFilter({ ...filter, severity: e.target.value as EventFilter['severity'] })}>
          <option value="">all</option><option value="high">high</option><option value="medium">medium</option><option value="low">low</option>
        </select></label>
        <label>Time <select value={filter.since ?? ''} onChange={(e) => setFilter({ ...filter, since: e.target.value })}>
          {SINCE.map(([v, l]) => <option key={v} value={v}>{l}</option>)}
        </select></label>
      </div>
      {error && <p className="error">{error}</p>}
      {!events && !error && <p>Loading events…</p>}
      {events && events.length === 0 && <p>No security events for this filter.</p>}
      {events && events.length > 0 && (
        <table>
          <thead><tr><th>Time</th><th>Agent</th><th>Severity</th><th>Source / rule</th><th>Job</th><th>Action</th></tr></thead>
          <tbody>
            {events.map((e) => (
              <tr key={e.id}>
                <td className="nowrap">{new Date(e.time).toLocaleString()}</td>
                <td>{e.agent}</td>
                <td><span className={`sev ${e.severity}`}>{e.severity}</span></td>
                <td>{e.source} / <code>{e.rule}</code></td>
                <td>{e.job ?? ''}</td>
                <td className="action">{e.action}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {next && <p><button onClick={more}>Load more</button></p>}
    </>
  )
}
