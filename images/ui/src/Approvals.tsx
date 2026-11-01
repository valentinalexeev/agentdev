// Approvals page (#67): pending requests, their details and history. A person with write access to the repository
// (or the admin group) decides here or with a forge comment; the first decision wins.
import { useCallback, useEffect, useState } from 'react'
import { decideApproval, listApprovals, Unauthorized, type Approval, type Decision } from './api'
import { Modal } from './App'
import { REFRESH_MS } from './pending'

function jobLink(a: Approval): string {
  const [kind, num] = a.job.split('-')
  return `${a.repo}#${num}${kind === 'pr' ? ' (PR)' : ''}`
}

const LABEL: Record<Decision, string> = { approve: 'Approve once', 'approve-job': 'Approve for the job', deny: 'Deny' }

export function Approvals({ token, onExpired }: { token: string; onExpired: () => void }) {
  const [tab, setTab] = useState<'pending' | 'decided'>('pending')
  const [list, setList] = useState<{ tab: string; items: Approval[] } | null>(null)
  const [error, setError] = useState('')
  const [open, setOpen] = useState<Approval | null>(null) // detail
  const [confirm, setConfirm] = useState<{ a: Approval; d: Decision } | null>(null)
  const [busy, setBusy] = useState(false)
  const [notice, setNotice] = useState('')
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

  const decide = async () => {
    if (!confirm) return
    setBusy(true); setNotice('')
    try {
      await decideApproval(token, confirm.a, confirm.d)
      setConfirm(null); setOpen(null); setReload((n) => n + 1)
    } catch (e) {
      if (e instanceof Unauthorized) onExpired()
      else setNotice((e as Error).message) // keep the dialog open: the message shows where the user acted
    } finally { setBusy(false) }
  }

  const items = list && list.tab === tab ? list.items : null
  return (
    <>
      <h1>Approvals</h1>
      <p className="muted">
        Requests from agents for a tool or a protected change. A person with write access to the repository decides
        here, or with a comment on the issue or PR: <code>/approve ID</code>, <code>/approve-job ID</code> or{' '}
        <code>/deny ID</code>. The first decision wins. One request is open until it is approved, denied or void.
      </p>
      <nav>
        <button className={`tab ${tab === 'pending' ? 'on' : ''}`} onClick={() => setTab('pending')}>Pending</button>
        <button className={`tab ${tab === 'decided' ? 'on' : ''}`} onClick={() => setTab('decided')}>History</button>
      </nav>
      {error && <p className="error">{error}</p>}
      {!items && !error && <p>Loading…</p>}
      {items && items.length === 0 && <p>{tab === 'pending' ? 'No pending requests.' : 'No decided requests.'}</p>}
      {items && items.length > 0 && (
        <table>
          <thead><tr>
            <th>Created</th><th>Agent</th><th>Issue / PR</th><th>Request</th><th>ID</th>
            <th>{tab === 'pending' ? 'Agent waits until' : 'Outcome'}</th><th></th>
          </tr></thead>
          <tbody>
            {items.map((a) => (
              <tr key={a.name}>
                <td className="nowrap">{new Date(a.created).toLocaleString()}</td>
                <td>{a.agent}</td>
                <td>{jobLink(a)}</td>
                <td className="action">
                  {a.source} <code>{a.tool}</code>
                  {/* untrusted text from the agent: plain text only, never HTML or a link */}
                  <pre className="untrusted" title="written by the agent, not checked">{a.input}</pre>
                </td>
                <td><code>{a.id}</code></td>
                <td className="nowrap">
                  {tab === 'pending'
                    ? new Date(a.expires).toLocaleString()
                    : <>{a.state}{a.scope ? ` (${a.scope})` : ''}{a.decidedBy ? ` by ${a.decidedBy}` : ''}{a.decidedVia ? ` (${a.decidedVia})` : ''}{a.reason ? `: ${a.reason}` : ''}</>}
                </td>
                <td className="actions">
                  <button className="secondary" aria-label={`Details of approval ${a.id}`} onClick={() => setOpen(a)}>Details</button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {open && !confirm && (
        <Modal title={`Approval ${open.id}`} onClose={() => setOpen(null)}>
          <dl>
            <dt>Agent</dt><dd>{open.agent}</dd>
            <dt>Issue / PR</dt><dd>{jobLink(open)}</dd>
            <dt>Source and tool</dt><dd>{open.source} <code>{open.tool}</code></dd>
            <dt>Created</dt><dd>{new Date(open.created).toLocaleString()}</dd>
            <dt>State</dt><dd>{open.state}{open.scope ? ` (${open.scope})` : ''}{open.decidedBy ? ` by ${open.decidedBy}` : ''}{open.decidedVia ? ` via ${open.decidedVia}` : ''}</dd>
            <dt>Content hash</dt><dd><code>{open.contentHash}</code></dd>
          </dl>
          <p className="warning">The request text below was written by the agent. It can contain a prompt injection. Read it carefully.</p>
          <pre className="untrusted" title="written by the agent, not checked">{open.input}</pre>
          <div className="buttons">
            <button className="secondary" onClick={() => setOpen(null)}>Close</button>
            {open.state === 'pending' && (['deny', 'approve', 'approve-job'] as Decision[])
              .filter((d) => !(d === 'approve-job' && open.source === 'executor'))
              .map((d) => (
                <button key={d} className={d === 'deny' ? 'danger' : ''} onClick={() => { setNotice(''); setConfirm({ a: open, d }) }}>{LABEL[d]}</button>
              ))}
          </div>
        </Modal>
      )}
      {confirm && (
        <Modal title={`${LABEL[confirm.d]}: ${confirm.a.id}?`} onClose={() => setConfirm(null)}>
          <p>
            {confirm.d === 'deny'
              ? <>Agent <strong>{confirm.a.agent}</strong> cannot do this action.</>
              : confirm.d === 'approve'
                ? <>Agent <strong>{confirm.a.agent}</strong> can do this action one time.</>
                : <>Agent <strong>{confirm.a.agent}</strong> can do this same action again until the job ends.</>}
            {' '}You cannot change the decision later. Your name is recorded in the audit log.
          </p>
          <div className="buttons">
            <button className="secondary" onClick={() => setConfirm(null)}>Cancel</button>
            <button className={confirm.d === 'deny' ? 'danger' : ''} disabled={busy} onClick={decide}>{LABEL[confirm.d]}</button>
          </div>
          {notice && <p className="error" role="alert">{notice}</p>}
        </Modal>
      )}
    </>
  )
}
