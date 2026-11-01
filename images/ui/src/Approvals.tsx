// Approvals page (#67): pending requests and their history, read-only. The decision is a forge comment by a person
// with write access to the repository; deciding in the UI is #80.
import { useCallback, useEffect, useState } from 'react'
import { listApprovals, Unauthorized, type Approval } from './api'
import { REFRESH_MS } from './pending'

function jobLink(a: Approval): string {
  const [kind, num] = a.job.split('-')
  return `${a.repo}#${num}${kind === 'pr' ? ' (PR)' : ''}`
}

export function Approvals({ token, onExpired }: { token: string; onExpired: () => void }) {
  const [tab, setTab] = useState<'pending' | 'decided'>('pending')
  const [list, setList] = useState<{ tab: string; items: Approval[] } | null>(null)
  const [error, setError] = useState('')
  const fail = useCallback((e: Error) => (e instanceof Unauthorized ? onExpired() : setError(e.message)), [onExpired])

  useEffect(() => {
    let alive = true
    const load = () => listApprovals(token, tab)
      .then((items) => { if (alive) { setList({ tab, items }); setError('') } })
      .catch(fail)
    load()
    const t = setInterval(load, REFRESH_MS)
    return () => { alive = false; clearInterval(t) }
  }, [token, tab, fail])

  const items = list && list.tab === tab ? list.items : null
  return (
    <>
      <h1>Approvals</h1>
      <p className="muted">
        Requests from agents for a tool or a protected change. A person with write access to the repository decides
        with a comment on the issue or PR: <code>/approve ID</code>, <code>/approve-job ID</code> or <code>/deny ID</code>.
        One request is open until it is approved, denied or void (no longer needed).
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
            <th>{tab === 'pending' ? 'Agent waits until' : 'Outcome'}</th>
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
                    : <>{a.state}{a.scope ? ` (${a.scope})` : ''}{a.decidedBy ? ` by ${a.decidedBy}` : ''}{a.reason ? `: ${a.reason}` : ''}</>}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </>
  )
}
