// Agents page (#46, #57): the fleet table. The buttons are always on: the API decides who can change what and the
// UI shows its answer.
import { useCallback, useEffect, useState } from 'react'
import {
  listAgents, patchAgent, createAgent, deleteAgent, getUsage, Unauthorized, nameError, repoError, maxConcurrentError,
  formatBudget, type Agent, type NewAgent, type UsagePause,
} from './api'
import { AgentTag, Banner, Drawer, Empty, Icon, Lamp, PageHead } from './ui'

export function AgentsPage({ token, onExpired }: { token: string; onExpired: () => void }) {
  const [agents, setAgents] = useState<Agent[] | null>(null)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [busy, setBusy] = useState(false)
  const [editing, setEditing] = useState<Agent | null>(null)
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

  const head = (
    <PageHead title="Agents" sub="An idle agent has zero pods and wakes on new work. This is normal."
      actions={<button className="ad-btn ad-btn-primary" type="button" onClick={() => { setNotice(''); setCreating(true) }}><Icon name="plus" />Add agent</button>} />
  )
  if (error) return <>{head}<p className="ad-error" role="alert">{error}</p></>
  if (!agents) return <>{head}<p className="ad-muted">Loading agents…</p></>
  const modal = deleting || creating || editing
  return (
    <>
      {head}
      {notice && !modal && <p className="ad-error" role="alert">{notice}</p>}
      {usagePaused.length > 0 && (
        <div style={{ marginBottom: 'var(--space-4)' }}>
          <Banner kind="paused" icon="pause" action={<a className="ad-btn" href="#usage">Usage</a>}>
            Subscription usage limit: {usagePaused.map(([a, p]) =>
              `${a} (${p.reason}, until ${new Date(p.until * 1000).toLocaleString()})`).join(', ')} start no new runs.
          </Banner>
        </div>
      )}
      {agents.length === 0
        ? <Empty title="No agents yet">Add one, or connect a project with <code className="ad-code">scripts/bootstrap-project.sh</code>.</Empty>
        : (
          <div className="ad-table-wrap">
            <table className="ad-table">
              <thead>
                <tr><th>Agent</th><th>Phase</th><th>Repositories</th><th>Forge</th><th className="ad-num">Events 7 d</th>
                  <th className="ad-num">Tokens 24 h</th><th>Settings</th><th><span className="ad-sr">Actions</span></th></tr>
              </thead>
              <tbody>
                {agents.map((a) => (
                  <tr key={a.name}>
                    <td><AgentTag name={a.name} /></td>
                    <td className="ad-nowrap">
                      {usagePaused.some(([n]) => n === a.name) ? <Lamp phase="usage-paused" /> : <Lamp phase={a.phase} />}
                    </td>
                    <td className="ad-code ad-nowrap">{(a.repos ?? []).join(', ')}</td>
                    <td className="ad-code">{a.forge ?? ''}</td>
                    <td className="ad-num">
                      {a.securityEvents
                        ? <a className="ad-link" href={`#security?agent=${encodeURIComponent(a.name)}`} aria-label={`${a.securityEvents} security events of ${a.name}`}>{a.securityEvents}</a>
                        : <span className="ad-muted">0</span>}
                    </td>
                    <td className="ad-num" style={a.budgetLimit && (a.budgetUsed ?? 0) >= a.budgetLimit ? { color: 'var(--danger)' } : undefined}>
                      {formatBudget(a) || <span className="ad-muted">–</span>}
                    </td>
                    <td className="ad-muted">
                      {a.managed === false ? 'no Agent object' : <>max. {a.maxConcurrent ?? 2}{a.scaleToZero ? ', scale to zero' : ''}</>}
                    </td>
                    <td>
                      {a.managed !== false && (
                        <div className="ad-row-actions" style={{ flexWrap: 'nowrap' }}>
                          {a.paused
                            ? <button className="ad-btn ad-btn-sm" disabled={busy} aria-label={`Resume agent ${a.name}`}
                              onClick={() => act(`Resume ${a.name}`, () => patchAgent(token, a.name, { paused: false }))}><Icon name="play" />Resume</button>
                            : <button className="ad-btn ad-btn-sm" disabled={busy} aria-label={`Pause agent ${a.name}`}
                              onClick={() => act(`Pause ${a.name}`, () => patchAgent(token, a.name, { paused: true }))}><Icon name="pause" />Pause</button>}
                          <button className="ad-btn ad-btn-sm" disabled={busy} aria-label={`Edit settings of agent ${a.name}`}
                            onClick={() => { setNotice(''); setEditing(a) }}>Edit</button>
                          <button className="ad-btn ad-btn-sm ad-btn-quiet" disabled={busy} aria-label={`Delete agent ${a.name}`}
                            onClick={() => { setNotice(''); setDeleting(a.name) }}>Delete</button>
                        </div>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      {editing && (
        <EditDrawer agent={editing} busy={busy} notice={notice} onClose={() => setEditing(null)}
          onSave={async (p) => { if (await act(`Change ${editing.name}`, () => patchAgent(token, editing.name, p))) setEditing(null) }} />
      )}
      {deleting && (
        <Drawer title={`Delete agent ${deleting}?`} onClose={() => setDeleting(null)}
          foot={<>
            <button className="ad-btn" type="button" onClick={() => setDeleting(null)}>Cancel</button>
            <button className="ad-btn ad-btn-danger" type="button" disabled={busy} onClick={async () => {
              const n = deleting
              // keep the drawer open on an error, so the message shows where the user acted
              if (await act(`Delete ${n}`, () => deleteAgent(token, n))) setDeleting(null)
            }}>Delete {deleting}</button>
          </>}>
          <p style={{ margin: 0 }}>The agent <strong>{deleting}</strong> stops. Its volumes stay; an administrator removes them with kubectl.</p>
          {notice && <p className="ad-error" role="alert">{notice}</p>}
        </Drawer>
      )}
      {creating && (
        <CreateDrawer busy={busy} notice={notice} onClose={() => setCreating(false)}
          onCreate={async (n) => { if (await act(`Add ${n.name}`, () => createAgent(token, n))) setCreating(false) }} />
      )}
    </>
  )
}

function EditDrawer({ agent, busy, notice, onSave, onClose }: {
  agent: Agent; busy: boolean; notice: string; onSave: (p: { maxConcurrent: number; scaleToZero: boolean }) => void; onClose: () => void
}) {
  const [max, setMax] = useState(String(agent.maxConcurrent ?? 2))
  const [stz, setStz] = useState(agent.scaleToZero ?? true)
  const err = maxConcurrentError(Number(max))
  return (
    <Drawer title={<>Settings of <AgentTag name={agent.name} /></>} onClose={onClose}
      foot={<>
        <button className="ad-btn" type="button" onClick={onClose}>Cancel</button>
        <button className="ad-btn ad-btn-primary" type="submit" form="edit-agent" disabled={busy || !!err}>Save</button>
      </>}>
      <form id="edit-agent" className="ad-form" onSubmit={(e) => { e.preventDefault(); if (!err) onSave({ maxConcurrent: Number(max), scaleToZero: stz }) }}>
        <label className="ad-field"><span>Max. concurrent</span>
          <input className="ad-input" type="number" min={1} max={8} required value={max} aria-invalid={!!err}
            aria-label={`Maximum concurrent items of agent ${agent.name}`} onChange={(e) => setMax(e.target.value)} />
          <small>Issues and pull requests the agent works on at the same time, 1 to 8.</small></label>
        <label className="ad-check"><input type="checkbox" checked={stz} aria-label={`Scale agent ${agent.name} to zero`}
          onChange={(e) => setStz(e.target.checked)} /> Scale to zero</label>
        {err && <span className="ad-error">{err}</span>}
        {notice && <p className="ad-error" role="alert">{notice}</p>}
      </form>
    </Drawer>
  )
}

function CreateDrawer({ busy, notice, onCreate, onClose }: { busy: boolean; notice: string; onCreate: (a: NewAgent) => void; onClose: () => void }) {
  const [a, setA] = useState<NewAgent>({ name: '', repo: '', forge: 'gitea', maxConcurrent: 2, scaleToZero: true })
  const [touched, setTouched] = useState(false)
  const errors = [nameError(a.name), repoError(a.repo), maxConcurrentError(a.maxConcurrent)].filter(Boolean)
  return (
    <Drawer title="Add an agent" onClose={onClose}
      foot={<>
        <button type="button" className="ad-btn" onClick={onClose}>Cancel</button>
        <button type="submit" form="add-agent" className="ad-btn ad-btn-primary" disabled={busy}>Add agent</button>
      </>}>
      <form id="add-agent" className="ad-form" onSubmit={(e) => { e.preventDefault(); setTouched(true); if (errors.length === 0) onCreate(a) }}>
        <label className="ad-field"><span>Name</span><input className="ad-input" required autoFocus value={a.name} onChange={(e) => setA({ ...a, name: e.target.value })} /></label>
        <label className="ad-field"><span>Repository (owner/name)</span><input className="ad-input" required value={a.repo} onChange={(e) => setA({ ...a, repo: e.target.value })} /></label>
        <label className="ad-field"><span>Forge</span>
          <select className="ad-input" value={a.forge} onChange={(e) => setA({ ...a, forge: e.target.value as NewAgent['forge'] })}>
            <option value="gitea">gitea</option>
            <option value="github">github</option>
          </select>
        </label>
        <label className="ad-field"><span>Max. concurrent</span><input className="ad-input" type="number" min={1} max={8} required value={a.maxConcurrent}
          onChange={(e) => setA({ ...a, maxConcurrent: Number(e.target.value) })} /></label>
        <label className="ad-check"><input type="checkbox" checked={a.scaleToZero} onChange={(e) => setA({ ...a, scaleToZero: e.target.checked })} /> Scale to zero</label>
        {touched && errors.map((m) => <p key={m} className="ad-error">{m}</p>)}
        {notice && <p className="ad-error" role="alert">{notice}</p>}
      </form>
    </Drawer>
  )
}
