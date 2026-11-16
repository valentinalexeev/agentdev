// Agents page (#46, #57): the fleet table. The buttons are always on: the API decides who can change what and the
// UI shows its answer.
import { useCallback, useState } from 'react'
import {
  patchAgent, createAgent, deleteAgent, Unauthorized, nameError, repoError, maxConcurrentError, formatBudget, type Agent, type NewAgent,
} from './api'
import { ATTN_LABEL, ATTN_RANK, type AgentView } from './model'
import { useData } from './useData'
import { GROUP_AT, short } from './format'
import { AgentTag, Attention, Banner, Dots, Drawer, Empty, Icon, Lamp, PageHead } from './ui'

export function AgentsPage({ token, onExpired }: { token: string; onExpired: () => void }) {
  const d = useData()
  const [notice, setNotice] = useState('')
  const [busy, setBusy] = useState(false)
  const [editing, setEditing] = useState<Agent | null>(null)
  const [deleting, setDeleting] = useState<string | null>(null)
  const [creating, setCreating] = useState(false)
  const [filter, setFilter] = useState<'all' | 'attn' | 'working'>('all')
  const [fold, setFold] = useState<Record<string, boolean>>({})

  // run one change, then read the data again; returns true on success
  const act = useCallback(async (what: string, f: () => Promise<void>): Promise<boolean> => {
    setBusy(true)
    setNotice('')
    try {
      await f()
      d.reload()
      return true
    } catch (e) {
      if (e instanceof Unauthorized) onExpired()
      else setNotice(`${what}: ${(e as Error).message}`)
      return false
    } finally {
      setBusy(false)
    }
  }, [d, onExpired])

  const head = (
    <PageHead title="Agents" sub="Lifecycle and attention are separate. An idle agent has zero pods and wakes on new work. This is normal."
      actions={<button className="agd-btn agd-btn-primary" type="button" onClick={() => { setNotice(''); setCreating(true) }}><Icon name="plus" />Add agent</button>} />
  )
  if (!d.ready) return <>{head}<p className="agd-muted">Loading agents…</p></>
  const usagePaused = d.agents.filter((a) => a.life === 'usage-paused')
  const nAttn = d.agents.filter((a) => a.attn !== 'ok').length
  const nWork = d.agents.filter((a) => a.life === 'running' || a.life === 'starting').length
  const shown = d.agents.filter((a) => filter === 'all' || (filter === 'attn' ? a.attn !== 'ok' : a.life === 'running' || a.life === 'starting'))
    .sort((x, y) => (ATTN_RANK[x.attn] - ATTN_RANK[y.attn]) || x.name.localeCompare(y.name))
  const modal = deleting || creating || editing
  const row = (a: AgentView) => (
    <tr key={a.name}>
      <td><a className="agd-row-link" href={`#agents/${encodeURIComponent(a.name)}`} aria-label={`Open agent ${a.name}`}><AgentTag name={a.name} /></a></td>
      <td className="agd-nowrap"><Lamp phase={a.life} /></td>
      <td><Attention attn={a.attn} label={ATTN_LABEL[a.attn]} />{a.why && <div className="agd-muted" style={{ fontSize: 12, lineHeight: '16px', marginTop: 2 }}>{a.why}</div>}</td>
      <td className="agd-code agd-nowrap agd-hide-phone">{(a.repos ?? []).join(', ')}</td>
      <td className="agd-num agd-hide-phone">
        {a.securityEvents
          ? <a className="agd-link" href={`#security?agent=${encodeURIComponent(a.name)}`} aria-label={`${a.securityEvents} security events of ${a.name}`}>{a.securityEvents}</a>
          : <span className="agd-muted">0</span>}
      </td>
      <td className="agd-num agd-hide-phone" style={a.budgetLimit && (a.budgetUsed ?? 0) >= a.budgetLimit ? { color: 'var(--danger)' } : undefined}>
        {formatBudget(a) || <span className="agd-muted">–</span>}
      </td>
      <td className="agd-muted agd-hide-phone agd-hide-narrow">
        {a.managed === false ? 'no Agent object' : <>max. {a.maxConcurrent ?? 2}{a.scaleToZero ? ', scale to zero' : ''}</>}
      </td>
      <td>
        {a.managed !== false && (
          <div className="agd-row-actions" style={{ flexWrap: 'nowrap' }}>
            {a.paused
              ? <button className="agd-btn agd-btn-sm" disabled={busy} aria-label={`Resume agent ${a.name}`}
                onClick={() => act(`Resume ${a.name}`, () => patchAgent(token, a.name, { paused: false }))}><Icon name="play" />Resume</button>
              : <button className="agd-btn agd-btn-sm" disabled={busy} aria-label={`Pause agent ${a.name}`}
                onClick={() => act(`Pause ${a.name}`, () => patchAgent(token, a.name, { paused: true }))}><Icon name="pause" />Pause</button>}
            <button className="agd-btn agd-btn-sm" disabled={busy} aria-label={`Edit settings of agent ${a.name}`}
              onClick={() => { setNotice(''); setEditing(a) }}>Edit</button>
            <button className="agd-btn agd-btn-sm agd-btn-quiet" disabled={busy} aria-label={`Delete agent ${a.name}`}
              onClick={() => { setNotice(''); setDeleting(a.name) }}>Delete</button>
          </div>
        )}
      </td>
    </tr>
  )
  // above 10 agents: one group per project; groups where every agent is ok are folded, attention is never hidden
  let body: React.ReactNode
  if (shown.length <= GROUP_AT) body = shown.map(row)
  else {
    const byP = new Map<string, AgentView[]>()
    for (const a of shown) { const k = a.repo || '(no repository)'; if (!byP.has(k)) byP.set(k, []); byP.get(k)!.push(a) }
    body = [...byP.entries()]
      .sort(([pa, a], [pb, b]) => (Math.min(...a.map((x) => ATTN_RANK[x.attn])) - Math.min(...b.map((x) => ATTN_RANK[x.attn]))) || pa.localeCompare(pb))
      .map(([p, ag]) => {
        const bad = ag.filter((a) => a.attn !== 'ok').length
        const open = fold[p] ?? bad > 0
        const working = ag.filter((a) => a.life === 'running' || a.life === 'starting').length
        return [
          <tr key={`g${p}`} className="agd-group-row"><td colSpan={8}>
            <button className="agd-group-btn" type="button" aria-expanded={open} onClick={() => setFold({ ...fold, [p]: !open })}>
              <Icon name="down" /><strong className="agd-code">{short(p)}</strong><Dots agents={ag} />
              <span className="agd-muted">{ag.length} {ag.length === 1 ? 'agent' : 'agents'} · {working} working{bad ? ` · ${bad} ${bad === 1 ? 'needs' : 'need'} attention` : ' · all ok'}</span>
            </button></td></tr>,
          ...(open ? ag.map(row) : ag.filter((a) => a.attn !== 'ok').map(row)),
        ]
      })
  }
  return (
    <>
      {head}
      {notice && !modal && <p className="agd-error" role="alert">{notice}</p>}
      {usagePaused.length > 0 && (
        <div style={{ marginBottom: 'var(--space-4)' }}>
          <Banner kind="paused" icon="pause" action={<a className="agd-btn" href="#usage">Usage</a>}>
            Usage limit: {usagePaused.map((a) => `${a.name} (${a.why})`).join(', ')} {usagePaused.length === 1 ? 'starts' : 'start'} no new runs.
          </Banner>
        </div>
      )}
      <div className="agd-filters" style={{ marginBottom: 'var(--space-3)' }}>
        <button className="agd-chip" type="button" aria-pressed={filter === 'all'} onClick={() => setFilter('all')}>All <span className="agd-data">{d.agents.length}</span></button>
        <button className="agd-chip" type="button" aria-pressed={filter === 'attn'} onClick={() => setFilter('attn')}>Needs attention <span className="agd-data">{nAttn}</span></button>
        <button className="agd-chip" type="button" aria-pressed={filter === 'working'} onClick={() => setFilter('working')}>Working <span className="agd-data">{nWork}</span></button>
      </div>
      {shown.length === 0
        ? (d.agents.length === 0
          ? <Empty title="No agents yet">Add one, or connect a project with <code className="agd-code">scripts/bootstrap-project.sh</code>.</Empty>
          : <Empty title="No agent matches this filter" />)
        : (
          <div className="agd-table-wrap">
            <table className="agd-table agd-table-2l">
              <thead>
                <tr><th>Agent</th><th>Lifecycle</th><th>Attention</th><th className="agd-hide-phone">Repositories</th><th className="agd-num agd-hide-phone">Events 7 d</th>
                  <th className="agd-num agd-hide-phone">Claude units 24 h</th><th className="agd-hide-phone agd-hide-narrow">Settings</th><th><span className="agd-sr">Actions</span></th></tr>
              </thead>
              <tbody>{body}</tbody>
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
            <button className="agd-btn" type="button" onClick={() => setDeleting(null)}>Cancel</button>
            <button className="agd-btn agd-btn-danger" type="button" disabled={busy} onClick={async () => {
              const n = deleting
              // keep the drawer open on an error, so the message shows where the user acted
              if (await act(`Delete ${n}`, () => deleteAgent(token, n))) setDeleting(null)
            }}>Delete {deleting}</button>
          </>}>
          <p style={{ margin: 0 }}>The agent <strong>{deleting}</strong> stops. Its volumes stay; an administrator removes them with kubectl.</p>
          {notice && <p className="agd-error" role="alert">{notice}</p>}
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
        <button className="agd-btn" type="button" onClick={onClose}>Cancel</button>
        <button className="agd-btn agd-btn-primary" type="submit" form="edit-agent" disabled={busy || !!err}>Save</button>
      </>}>
      <form id="edit-agent" className="agd-form" onSubmit={(e) => { e.preventDefault(); if (!err) onSave({ maxConcurrent: Number(max), scaleToZero: stz }) }}>
        <label className="agd-field"><span>Max. concurrent</span>
          <input className="agd-input" type="number" min={1} max={8} required value={max} aria-invalid={!!err}
            aria-label={`Maximum concurrent items of agent ${agent.name}`} onChange={(e) => setMax(e.target.value)} />
          <small>Issues and pull requests the agent works on at the same time, 1 to 8.</small></label>
        <label className="agd-check"><input type="checkbox" checked={stz} aria-label={`Scale agent ${agent.name} to zero`}
          onChange={(e) => setStz(e.target.checked)} /> Scale to zero</label>
        {err && <span className="agd-error">{err}</span>}
        {notice && <p className="agd-error" role="alert">{notice}</p>}
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
        <button type="button" className="agd-btn" onClick={onClose}>Cancel</button>
        <button type="submit" form="add-agent" className="agd-btn agd-btn-primary" disabled={busy}>Add agent</button>
      </>}>
      <form id="add-agent" className="agd-form" onSubmit={(e) => { e.preventDefault(); setTouched(true); if (errors.length === 0) onCreate(a) }}>
        <label className="agd-field"><span>Name</span><input className="agd-input" required autoFocus value={a.name} onChange={(e) => setA({ ...a, name: e.target.value })} /></label>
        <label className="agd-field"><span>Repository (owner/name)</span><input className="agd-input" required value={a.repo} onChange={(e) => setA({ ...a, repo: e.target.value })} /></label>
        <label className="agd-field"><span>Forge</span>
          <select className="agd-input" value={a.forge} onChange={(e) => setA({ ...a, forge: e.target.value as NewAgent['forge'] })}>
            <option value="gitea">gitea</option>
            <option value="github">github</option>
          </select>
        </label>
        <label className="agd-field"><span>Max. concurrent</span><input className="agd-input" type="number" min={1} max={8} required value={a.maxConcurrent}
          onChange={(e) => setA({ ...a, maxConcurrent: Number(e.target.value) })} /></label>
        <label className="agd-check"><input type="checkbox" checked={a.scaleToZero} onChange={(e) => setA({ ...a, scaleToZero: e.target.checked })} /> Scale to zero</label>
        {touched && errors.map((m) => <p key={m} className="agd-error">{m}</p>)}
        {notice && <p className="agd-error" role="alert">{notice}</p>}
      </form>
    </Drawer>
  )
}
