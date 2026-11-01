import { useCallback, useEffect, useRef, useState } from 'react'
import type { UserManager, User } from './auth'
import {
  listAgents, listSecurityEvents, patchAgent, createAgent, deleteAgent, Unauthorized,
  nameError, repoError, maxConcurrentError, type Agent, type NewAgent, type EventFilter, type SecurityEvent,
  getUsage, setGlobalLimits, setProjectLimits, pctError, type Usage, type UsageWindow, type UsagePause,
} from './api'
import { Approvals } from './Approvals'
import { usePendingCount } from './pending'

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
  const pending = usePendingCount(token)
  return (
    <>
      <nav>
        <button className={`tab ${page === 'agents' ? 'on' : ''}`} onClick={() => go('agents')}>Agents</button>
        <button className={`tab ${page === 'security' ? 'on' : ''}`} onClick={() => go('security')}>Security</button>
        <button className={`tab ${page === 'approvals' ? 'on' : ''}`} onClick={() => go('approvals')}>
          Approvals{pending > 0 && <span className="badge" aria-label={`${pending} pending`}>{pending}</span>}
        </button>
        <button className={`tab ${page === 'usage' ? 'on' : ''}`} onClick={() => go('usage')}>Usage</button>
      </nav>
      {page === 'approvals' ? <Approvals token={token} onExpired={onExpired} /> : page === 'usage' ? <UsagePage token={token} onExpired={onExpired} /> : page === 'security'
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

// The buttons are always on: the API decides who can change what (#57) and the UI shows its message.
function Overview({ token, onExpired }: { token: string; onExpired: () => void }) {
  const [agents, setAgents] = useState<Agent[] | null>(null)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [busy, setBusy] = useState(false)
  const [editing, setEditing] = useState<string | null>(null)
  const [deleting, setDeleting] = useState<string | null>(null)
  const [creating, setCreating] = useState(false)
  // usage pauses (#93): shown as a banner and as a state in the agent rows; a failed read only hides them
  const [pauses, setPauses] = useState<{ list: Record<string, UsagePause>; at: number }>({ list: {}, at: 0 })

  const load = useCallback(() => {
    listAgents(token)
      .then((a) => { setAgents(a); setError('') })
      .catch((e) => (e instanceof Unauthorized ? onExpired() : setError(e.message)))
    getUsage(token).then((u) => setPauses({ list: u.pauses ?? {}, at: Date.now() })).catch(() => setPauses({ list: {}, at: 0 }))
  }, [token, onExpired])
  const usagePaused = Object.entries(pauses.list).filter(([, p]) => p.until * 1000 > pauses.at)

  useEffect(() => {
    load()
    const t = setInterval(load, 15000) // a light poll is enough until the API has events
    return () => clearInterval(t)
  }, [load])

  // run one change, then read the list again; returns true on success
  const act = useCallback(async (what: string, f: () => Promise<void>): Promise<boolean> => {
    setBusy(true)
    setNotice('')
    try {
      await f()
      load()
      return true
    } catch (e) {
      if (e instanceof Unauthorized) onExpired()
      else setNotice(`${what}: ${(e as Error).message}`)
      return false
    } finally {
      setBusy(false)
    }
  }, [load, onExpired])

  if (error) return <p className="error">{error}</p>
  if (!agents) return <p>Loading agents…</p>
  return (
    <>
      <div className="title">
        <h1>Agents</h1>
        <button onClick={() => setCreating(true)}>Add agent</button>
      </div>
      {notice && !deleting && !creating && <p className="error" role="alert">{notice}</p>}
      {usagePaused.length > 0 && (
        <p className="banner" role="status">
          Subscription usage limit: {usagePaused.map(([a, p]) =>
            `${a} (${p.reason}, until ${new Date(p.until * 1000).toLocaleString()})`).join(', ')} start no new runs.{' '}
          <a href="#usage">Usage</a>
        </p>
      )}
      {agents.length === 0
        ? <p>No agents yet. Add one, or connect a project with <code>scripts/bootstrap-project.sh</code>.</p>
        : (
          <table>
            <thead>
              <tr><th>Agent</th><th>Phase</th><th>Repositories</th><th>Forge</th><th>Security (7 days)</th><th>Settings</th><th>Actions</th></tr>
            </thead>
            <tbody>
              {agents.map((a) => (
                <tr key={a.name}>
                  <td>{a.name}</td>
                  <td>
                    <span className={`phase ${a.phase}`}>{a.phase}</span>
                    {usagePaused.some(([n]) => n === a.name) && <> <span className="phase usage-paused">usage-paused</span></>}
                  </td>
                  <td>{(a.repos ?? []).join(', ')}</td>
                  <td>{a.forge ?? ''}</td>
                  <td>
                    {a.securityEvents
                      ? <a className="badge" href={`#security?agent=${encodeURIComponent(a.name)}`}>{a.securityEvents}</a>
                      : <span className="muted">0</span>}
                  </td>
                  <td>
                    {a.managed === false ? <span className="muted">no Agent object</span> : editing === a.name
                      ? (
                        <EditForm
                          agent={a} busy={busy}
                          onCancel={() => setEditing(null)}
                          onSave={async (p) => {
                            if (await act(`Change ${a.name}`, () => patchAgent(token, a.name, p))) setEditing(null)
                          }}
                        />
                      )
                      : <>max. {a.maxConcurrent ?? 2}{a.scaleToZero ? ', scale to zero' : ''}</>}
                  </td>
                  <td className="actions">
                    {a.managed !== false && (
                      <>
                        {a.paused
                          ? <button disabled={busy} aria-label={`Resume agent ${a.name}`}
                            onClick={() => act(`Resume ${a.name}`, () => patchAgent(token, a.name, { paused: false }))}>Resume</button>
                          : <button disabled={busy} aria-label={`Pause agent ${a.name}`}
                            onClick={() => act(`Pause ${a.name}`, () => patchAgent(token, a.name, { paused: true }))}>Pause</button>}
                        <button className="secondary" disabled={busy || editing === a.name} aria-label={`Edit settings of agent ${a.name}`}
                          onClick={() => setEditing(a.name)}>Edit</button>
                        <button className="danger" disabled={busy} aria-label={`Delete agent ${a.name}`}
                          onClick={() => setDeleting(a.name)}>Delete</button>
                      </>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      {deleting && (
        <Modal title={`Delete agent ${deleting}?`} onClose={() => setDeleting(null)}>
          <p>The agent <strong>{deleting}</strong> stops. Its volumes stay; an administrator removes them with kubectl.</p>
          <div className="buttons">
            <button className="secondary" onClick={() => setDeleting(null)}>Cancel</button>
            <button className="danger" disabled={busy} onClick={async () => {
              const n = deleting
              // keep the dialog open on an error, so the message shows where the user acted
              if (await act(`Delete ${n}`, () => deleteAgent(token, n))) setDeleting(null)
            }}>Delete {deleting}</button>
          </div>
          {notice && <p className="error" role="alert">{notice}</p>}
        </Modal>
      )}
      {creating && (
        <Modal title="Add an agent" onClose={() => setCreating(false)}>
          <CreateForm busy={busy} onCancel={() => setCreating(false)} onCreate={async (n) => {
            if (await act(`Add ${n.name}`, () => createAgent(token, n))) setCreating(false)
          }} />
          {notice && <p className="error" role="alert">{notice}</p>}
        </Modal>
      )}
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

// Subscription usage (#93): windows per Claude subscription, pauses, and the limits. The API decides who may change a
// limit (global: agentdev admins; project: write access, only lower than global) and the UI shows its answer.
function UsagePage({ token, onExpired }: { token: string; onExpired: () => void }) {
  const [u, setU] = useState<Usage | null>(null)
  const [loadedAt, setLoadedAt] = useState(0) // windows past their reset are shown as -
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const fail = useCallback((e: Error) => (e instanceof Unauthorized ? onExpired() : setError(e.message)), [onExpired])
  const load = useCallback(() => { getUsage(token).then((r) => { setU(r); setLoadedAt(Date.now()); setError('') }).catch(fail) }, [token, fail])
  useEffect(() => { getUsage(token).then((r) => { setU(r); setLoadedAt(Date.now()); setError('') }).catch(fail) }, [token, fail])
  const save = (p: Promise<void>) => { setBusy(true); p.then(load).catch(fail).finally(() => setBusy(false)) }
  const pct = (w?: UsageWindow) => (w && w.resetsAt * 1000 > loadedAt ? `${Math.round(w.utilization * 100)} %` : '–')
  const at = (s?: number) => (s ? new Date(s * 1000).toLocaleString() : '')
  if (!u) return error ? <p className="error">{error}</p> : <p>Loading usage…</p>
  return (
    <>
      <h1>Subscription usage</h1>
      <p className="muted">When a window passes its limit, agents on that subscription start no new runs until the window resets.
        Runs in progress finish.</p>
      {error && <p className="error">{error}</p>}
      <h2>Subscriptions</h2>
      {!u.state || Object.keys(u.state).length === 0 ? <p>No runs reported yet.</p> : (
        <table>
          <thead><tr><th>Claude Secret</th><th>5-hour</th><th>Resets</th><th>Weekly</th><th>Resets</th><th>Tokens (5h)</th><th>Runs</th></tr></thead>
          <tbody>{Object.entries(u.state).map(([k, s]) => (
            <tr key={k}><td>{k}</td><td>{pct(s.fiveHour)}</td><td className="nowrap">{at(s.fiveHour?.resetsAt)}</td>
              <td>{pct(s.weekly)}</td><td className="nowrap">{at(s.weekly?.resetsAt)}</td>
              <td>{(s.inputTokens + s.outputTokens).toLocaleString()}</td><td>{s.runs}</td></tr>))}</tbody>
        </table>)}
      <h2>Paused agents</h2>
      {!u.pauses || Object.keys(u.pauses).length === 0 ? <p>No agent is paused for usage.</p> : (
        <table>
          <thead><tr><th>Agent</th><th>Reason</th><th>Until</th></tr></thead>
          <tbody>{Object.entries(u.pauses).map(([a, p]) => (
            <tr key={a}><td>{a}</td><td>{p.reason}</td><td className="nowrap">{at(p.until)}</td></tr>))}</tbody>
        </table>)}
      <h2>Limits</h2>
      <LimitsRow key={`g${u.global.fiveHour}/${u.global.weekly}`} label="Global (administrators)" busy={busy} five={u.global.fiveHour} week={u.global.weekly}
        onSave={(f, w) => save(setGlobalLimits(token, { fiveHour: f, weekly: w }))} />
      {Object.keys(u.effective).sort().map((repo) => (
        <LimitsRow key={`${repo}${JSON.stringify(u.projects?.[repo] ?? {})}${u.global.fiveHour}/${u.global.weekly}`} label={repo} busy={busy} clearable
          five={u.projects?.[repo]?.fiveHour ?? u.effective[repo].fiveHour}
          week={u.projects?.[repo]?.weekly ?? u.effective[repo].weekly}
          note={`effective ${u.effective[repo].fiveHour} % / ${u.effective[repo].weekly} %`}
          onSave={(f, w) => save(setProjectLimits(token, repo, { fiveHour: f, weekly: w }))}
          onClear={() => save(setProjectLimits(token, repo, { fiveHour: null, weekly: null }))} />
      ))}
    </>
  )
}

function LimitsRow({ label, five, week, note, busy, clearable, onSave, onClear }: {
  label: string; five: number; week: number; note?: string; busy: boolean; clearable?: boolean
  onSave: (fiveHour: number, weekly: number) => void; onClear?: () => void
}) {
  const [f, setF] = useState(five)
  const [w, setW] = useState(week)
  const err = pctError(f) || pctError(w)
  return (
    <div className="filters">
      <strong>{label}</strong>
      <label>5-hour % <input type="number" min={1} max={100} value={f} onChange={(e) => setF(Number(e.target.value))} /></label>
      <label>Weekly % <input type="number" min={1} max={100} value={w} onChange={(e) => setW(Number(e.target.value))} /></label>
      <button disabled={busy || !!err} onClick={() => onSave(f, w)}>Save</button>
      {clearable && <button disabled={busy} onClick={onClear}>Use global</button>}
      {note && <span className="muted">{note}</span>}
      {err && <span className="error">{err}</span>}
    </div>
  )
}

function EditForm({ agent, busy, onSave, onCancel }: {
  agent: Agent; busy: boolean; onSave: (p: { maxConcurrent: number; scaleToZero: boolean }) => void; onCancel: () => void
}) {
  const [max, setMax] = useState(String(agent.maxConcurrent ?? 2))
  const [stz, setStz] = useState(agent.scaleToZero ?? false)
  const err = maxConcurrentError(Number(max))
  return (
    <form className="inline" onSubmit={(e) => { e.preventDefault(); if (!err) onSave({ maxConcurrent: Number(max), scaleToZero: stz }) }}>
      <label>Max. concurrent <input type="number" min={1} max={8} required value={max} aria-label={`Maximum concurrent items of agent ${agent.name}`}
        onChange={(e) => setMax(e.target.value)} /></label>
      <label><input type="checkbox" checked={stz} aria-label={`Scale agent ${agent.name} to zero`}
        onChange={(e) => setStz(e.target.checked)} /> Scale to zero</label>
      {err && <span className="error">{err}</span>}
      <span className="buttons">
        <button type="submit" disabled={busy || !!err}>Save</button>
        <button type="button" className="secondary" onClick={onCancel}>Cancel</button>
      </span>
    </form>
  )
}

function CreateForm({ busy, onCreate, onCancel }: { busy: boolean; onCreate: (a: NewAgent) => void; onCancel: () => void }) {
  const [a, setA] = useState<NewAgent>({ name: '', repo: '', forge: 'gitea', maxConcurrent: 2, scaleToZero: false })
  const [touched, setTouched] = useState(false)
  const errors = [nameError(a.name), repoError(a.repo), maxConcurrentError(a.maxConcurrent)].filter(Boolean)
  return (
    <form className="stack" onSubmit={(e) => { e.preventDefault(); setTouched(true); if (errors.length === 0) onCreate(a) }}>
      <label>Name <input required autoFocus value={a.name} onChange={(e) => setA({ ...a, name: e.target.value })} /></label>
      <label>Repository (owner/name) <input required value={a.repo} onChange={(e) => setA({ ...a, repo: e.target.value })} /></label>
      <label>Forge
        <select value={a.forge} onChange={(e) => setA({ ...a, forge: e.target.value as NewAgent['forge'] })}>
          <option value="gitea">gitea</option>
          <option value="github">github</option>
        </select>
      </label>
      <label>Max. concurrent <input type="number" min={1} max={8} required value={a.maxConcurrent}
        onChange={(e) => setA({ ...a, maxConcurrent: Number(e.target.value) })} /></label>
      <label><input type="checkbox" checked={a.scaleToZero} onChange={(e) => setA({ ...a, scaleToZero: e.target.checked })} /> Scale to zero</label>
      {touched && errors.map((m) => <p key={m} className="error">{m}</p>)}
      <div className="buttons">
        <button type="button" className="secondary" onClick={onCancel}>Cancel</button>
        <button type="submit" disabled={busy}>Add agent</button>
      </div>
    </form>
  )
}

// Modal uses the native <dialog>: focus stays inside, Escape closes it.
function Modal({ title, onClose, children }: { title: string; onClose: () => void; children: React.ReactNode }) {
  const ref = useRef<HTMLDialogElement>(null)
  useEffect(() => {
    const d = ref.current
    if (d && !d.open) d.showModal()
    return () => d?.close()
  }, [])
  return (
    <dialog ref={ref} aria-labelledby="dialog-title" onCancel={(e) => { e.preventDefault(); onClose() }}>
      <h2 id="dialog-title">{title}</h2>
      {children}
    </dialog>
  )
}
