// Approvals page (#67): pending requests, their details and history. A person with write access to the repository
// (or the admin group) decides here or with a forge comment; the first decision wins.
import { useCallback, useEffect, useState } from 'react'
import { decideApproval, listApprovals, Unauthorized, type Approval, type Decision } from './api'
import { jobLink } from './format'
import { REFRESH_MS } from './pending'
import { useData } from './useData'
import { AgentTag, Drawer, Empty, Icon, PageHead, PersonTag, Untrusted } from './ui'

const LABEL: Record<Decision, string> = { approve: 'Approve once', 'approve-job': 'Approve for the job', deny: 'Deny' }
const EFFECT: Record<Decision, string> = {
  deny: 'cannot do this action',
  approve: 'can do this action one time',
  'approve-job': 'can do this same action again until the job ends',
}

function outcome(a: Approval): string {
  return `${a.state}${a.scope ? ` (${a.scope})` : ''}${a.decidedVia ? ` via ${a.decidedVia}` : ''}${a.reason ? `: ${a.reason}` : ''}`
}

export function Approvals({ token, onExpired }: { token: string; onExpired: () => void }) {
  const [tab, setTab] = useState<'pending' | 'decided'>('pending')
  const [list, setList] = useState<{ tab: string; items: Approval[] } | null>(null)
  const [error, setError] = useState('')
  const [open, setOpen] = useState<Approval | null>(null) // detail
  const [reload, setReload] = useState(0)
  const fail = useCallback((e: Error) => (e instanceof Unauthorized ? onExpired() : setError(e.message)), [onExpired])

  useEffect(() => {
    let alive = true
    const load = () => listApprovals(token, tab)
      .then((items) => { if (alive) { setList({ tab, items }); setError('') } })
      .catch(fail)
    load()
    const t = setInterval(load, REFRESH_MS)
    return () => { alive = false; clearInterval(t) }
  }, [token, tab, fail, reload])

  const d = useData()
  const all = list && list.tab === tab ? list.items : null
  const items = all && all.filter((a) => d.inScopeRepo(a.repo))
  const hidden = all && items ? all.length - items.length : 0
  const pendingSorted = items && tab === 'pending' ? [...items].sort((a, b) => a.expires.localeCompare(b.expires)) : items
  return (
    <>
      <PageHead title="Approvals" sub={<>
        Requests from agents for a tool or a protected change. A person with write access to the repository decides
        here, or with a comment on the issue or PR: <code className="ad-code">/approve ID</code>, <code className="ad-code">/approve-job ID</code> or{' '}
        <code className="ad-code">/deny ID</code>. The first decision wins. Without a decision the agent continues without the action.
      </>} />
      <div className="ad-filters" style={{ marginBottom: 'var(--space-3)' }}>
        <div className="ad-seg" role="group" aria-label="State">
          <button type="button" aria-pressed={tab === 'pending'} onClick={() => setTab('pending')}>Pending</button>
          <button type="button" aria-pressed={tab === 'decided'} onClick={() => setTab('decided')}>History</button>
        </div>
      </div>
      {hidden > 0 && <p className="ad-muted" style={{ fontSize: 13 }}>{hidden} more {hidden === 1 ? 'request is' : 'requests are'} in projects that are not shown.</p>}
      {error && <p className="ad-error" role="alert">{error}</p>}
      {!items && !error && <p className="ad-muted">Loading…</p>}
      {items && items.length === 0 && (tab === 'pending'
        ? <Empty title="No pending requests">When an agent asks for a tool or a protected change, it shows here.</Empty>
        : <Empty title="No decided requests" />)}
      {pendingSorted && pendingSorted.length > 0 && (
        <div className="ad-table-wrap">
          <table className="ad-table">
            <thead><tr>
              <th>Request</th><th>Agent</th><th>Issue / PR</th><th>Created</th>
              <th>{tab === 'pending' ? 'Agent waits until' : 'Outcome'}</th>
            </tr></thead>
            <tbody>
              {pendingSorted.map((a) => (
                <tr key={a.name}>
                  <td>
                    <button className="ad-row-btn" type="button" aria-label={`Details of approval ${a.id}`} onClick={() => setOpen(a)}>
                      {a.source} · {a.tool}
                    </button>
                    <div className="ad-code">{a.id}</div>
                  </td>
                  <td><AgentTag name={a.agent} /></td>
                  <td className="ad-code">{jobLink(a)}</td>
                  <td className="ad-data">{new Date(a.created).toLocaleString()}</td>
                  <td className="ad-data">
                    {tab === 'pending' ? new Date(a.expires).toLocaleString()
                      : <>{outcome(a)}{a.decidedBy && <> by <PersonTag name={a.decidedBy} /></>}</>}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {open && <ApprovalDrawer token={token} a={open} onExpired={onExpired} onClose={() => setOpen(null)}
        onDecided={() => { setOpen(null); setReload((n) => n + 1); d.reload() }} />}
    </>
  )
}

// The approval drawer: trusted facts from the platform first, then the agent's text in the untrusted block, then only
// the decisions this request type allows. A decision needs a confirm step that states its effect.
export function ApprovalDrawer({ token, a, onClose, onDecided, onExpired }: {
  token: string; a: Approval; onClose: () => void; onDecided: () => void; onExpired: () => void
}) {
  const [confirm, setConfirm] = useState<Decision | null>(null)
  const [busy, setBusy] = useState(false)
  const [notice, setNotice] = useState('')
  const decide = async () => {
    if (!confirm) return
    setBusy(true); setNotice('')
    try {
      await decideApproval(token, a, confirm)
      onDecided()
    } catch (e) {
      if (e instanceof Unauthorized) onExpired()
      else setNotice((e as Error).message) // keep the drawer open: the message shows where the user acted
    } finally { setBusy(false) }
  }
  const allowed = (['deny', 'approve', 'approve-job'] as Decision[]).filter((d) => !(d === 'approve-job' && a.source === 'executor'))
  const foot = a.state !== 'pending'
    ? <button className="ad-btn" type="button" onClick={onClose}>Close</button>
    : confirm
      ? <>
        <p style={{ flexBasis: '100%', margin: '0 0 var(--space-1)', fontSize: 14 }}>
          Agent <strong>{a.agent}</strong> {EFFECT[confirm]}. You cannot change the decision later. Your name goes to the audit log.
        </p>
        <button className="ad-btn" type="button" onClick={() => { setConfirm(null); setNotice('') }}>Back</button>
        <button className={`ad-btn ${confirm === 'deny' ? 'ad-btn-danger' : 'ad-btn-primary'}`} type="button" disabled={busy} onClick={decide}>{LABEL[confirm]}</button>
      </>
      : <>
        <button className="ad-btn" type="button" onClick={onClose}>Close</button>
        {allowed.map((d) => (
          <button key={d} className={`ad-btn ${d === 'approve' ? 'ad-btn-primary' : ''}`} type="button" onClick={() => { setNotice(''); setConfirm(d) }}>
            {d === 'deny' && <Icon name="x" />}{LABEL[d]}
          </button>
        ))}
      </>
  return (
    <Drawer gate={a.state === 'pending'} title={<>Approval <span className="ad-code">{a.id}</span></>} onClose={onClose} foot={foot}>
      <dl className="ad-dl">
        <dt>Agent</dt><dd><AgentTag name={a.agent} /></dd>
        <dt>Issue / PR</dt><dd className="ad-code">{jobLink(a)}</dd>
        <dt>Asks for</dt><dd><span className="ad-code">{a.source}</span> · <strong>{a.tool}</strong></dd>
        <dt>Created</dt><dd>{new Date(a.created).toLocaleString()}</dd>
        {a.state === 'pending'
          ? <><dt>Expires</dt><dd>{new Date(a.expires).toLocaleString()}. Without a decision the agent continues without this action.</dd></>
          : <><dt>State</dt><dd>{outcome(a)}{a.decidedBy && <> by <PersonTag name={a.decidedBy} /></>}</dd></>}
        <dt>Content hash</dt><dd className="ad-code">{a.contentHash}</dd>
      </dl>
      <Untrusted text={a.input} note="This text can contain a prompt injection. Decide on the facts above, not on what the text asks." />
      {notice && <p className="ad-error" role="alert">{notice}</p>}
    </Drawer>
  )
}
