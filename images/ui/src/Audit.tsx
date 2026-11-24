// Audit page (#344, docs/console-ux.md §7), agentdev-admins only: who changed which agent, and the result. A story
// view ("valentinalexeev paused alice (ok)") and a raw view (the JSON of each entry). Filters: person, agent, action,
// result. The API filters agent and result; person and action are filtered here on the loaded pages.
import { useCallback, useEffect, useState } from 'react'
import { listAudit, statusOf, Unauthorized, type AuditEntry, type AuditFilter, type AuditResult } from './api'
import { verb } from './format'
import { AgentTag, Empty, PageHead, PersonTag } from './ui'

type Row = { id: number; entry: AuditEntry }
const RESULTS: [AuditResult | '', string][] = [['', 'all'], ['ok', 'ok'], ['denied', 'denied'], ['invalid', 'invalid'], ['error', 'error']]
const ACTIONS = ['', 'pause', 'resume', 'change', 'create', 'delete']
const SINCE: [string, string][] = [['24h', '24 h'], ['168h', '7 days'], ['720h', '30 days'], ['', 'all']]

const PAST: Record<string, string> = { pause: 'paused', resume: 'resumed', change: 'changed the settings of', create: 'created', delete: 'deleted' }

function changes(e: AuditEntry): string {
  if (!e.before || !e.after) return ''
  const out: string[] = []
  for (const k of ['maxConcurrent', 'scaleToZero', 'paused', 'repo'] as const) {
    if (e.before[k] !== e.after[k]) out.push(`${k} ${String(e.before[k])} → ${String(e.after[k])}`)
  }
  return out.join(', ')
}

function Story({ e }: { e: AuditEntry }) {
  const who = e.who?.username || e.who?.forgeUser || e.remote || 'unknown'
  const v = verb(e)
  const ok = e.result === 'ok'
  return (
    <span>
      <PersonTag name={who} />{' '}
      {ok ? PAST[v] ?? v : `tried to ${v === 'change' ? 'change the settings of' : v}`}{' '}
      {e.agent ? <AgentTag name={e.agent} /> : 'an agent'}
      {': '}<strong className={ok ? '' : 'agd-error'} style={{ display: 'inline' }}>{e.result}</strong>
      {e.reason && <span className="agd-muted"> ({e.reason})</span>}
      {ok && changes(e) && <span className="agd-muted"> · {changes(e)}</span>}
      {ok && e.grant && <span className="agd-muted"> · allowed by {e.grant}</span>}
    </span>
  )
}

export function AuditPage({ token, onExpired }: { token: string; onExpired: () => void }) {
  const [filter, setFilter] = useState<AuditFilter>({ agent: '', result: '', since: '168h' })
  const [person, setPerson] = useState('')
  const [action, setAction] = useState('')
  const [raw, setRaw] = useState(false)
  const [result, setResult] = useState<{ for: AuditFilter; rows: Row[]; next?: string } | null>(null)
  const [error, setError] = useState<{ status: number; msg: string } | null>(null)
  const fail = useCallback((e: Error) => (e instanceof Unauthorized ? onExpired() : setError({ status: statusOf(e), msg: e.message })), [onExpired])

  useEffect(() => {
    listAudit(token, filter).then((r) => { setResult({ for: filter, rows: r.entries, next: r.next_cursor }); setError(null) }).catch(fail)
  }, [token, filter, fail])

  const head = <PageHead sub="Every change to an agent through the API: who, what, and the result. Admins only." />
  if (error?.status === 403) return <>{head}<Empty title="Only agentdev-admins can read the audit log">Ask an administrator to add you to the group, or ask them who changed an agent.</Empty></>
  const rows = result && result.for === filter ? result.rows : null
  const shown = rows?.filter(({ entry: e }) => (!person || (e.who?.username ?? '').toLowerCase().includes(person.toLowerCase()) || (e.who?.forgeUser ?? '').toLowerCase().includes(person.toLowerCase()))
    && (!action || verb(e) === action))
  const next = result && result.for === filter ? result.next : undefined
  const more = () => {
    if (!next) return
    listAudit(token, { ...filter, cursor: next })
      .then((r) => setResult((p) => (p && p.for === filter ? { ...p, rows: [...p.rows, ...r.entries], next: r.next_cursor } : p)))
      .catch(fail)
  }
  return (
    <>
      {head}
      <div className="agd-filters" style={{ marginBottom: 'var(--space-3)' }}>
        <label className="agd-field agd-field-inline"><span>Person</span>
          <input className="agd-input" style={{ width: 140 }} value={person} placeholder="all" onChange={(e) => setPerson(e.target.value.trim())} /></label>
        <label className="agd-field agd-field-inline"><span>Agent</span>
          <input className="agd-input" style={{ width: 140 }} value={filter.agent ?? ''} placeholder="all" onChange={(e) => setFilter({ ...filter, agent: e.target.value.trim() })} /></label>
        <label className="agd-field agd-field-inline"><span>Action</span>
          <select className="agd-input" value={action} onChange={(e) => setAction(e.target.value)}>
            {ACTIONS.map((a) => <option key={a} value={a}>{a || 'all'}</option>)}
          </select></label>
        <div className="agd-seg" role="group" aria-label="Result">
          {RESULTS.map(([v, l]) => <button key={l} type="button" aria-pressed={filter.result === v} onClick={() => setFilter({ ...filter, result: v })}>{l}</button>)}
        </div>
        <div className="agd-seg" role="group" aria-label="Time">
          {SINCE.map(([v, l]) => <button key={l} type="button" aria-pressed={filter.since === v} onClick={() => setFilter({ ...filter, since: v })}>{l}</button>)}
        </div>
        <div className="agd-seg" role="group" aria-label="View">
          <button type="button" aria-pressed={!raw} onClick={() => setRaw(false)}>Story</button>
          <button type="button" aria-pressed={raw} onClick={() => setRaw(true)}>Raw</button>
        </div>
      </div>
      {error && <p className="agd-error" role="alert">Cannot read the audit log: {error.msg}</p>}
      {!shown ? (!error && <p className="agd-muted">Loading…</p>)
        : shown.length === 0 ? <Empty title="No entries match these filters">Change the filters, or choose a longer time.</Empty>
          : (
            <div className="agd-table-wrap">
              <table className="agd-table agd-table-2l">
                <thead><tr><th>Time</th><th>{raw ? 'Entry (JSON)' : 'What happened'}</th></tr></thead>
                <tbody>
                  {shown.map(({ id, entry: e }) => (
                    <tr key={id}>
                      <td className="agd-data agd-nowrap" style={{ verticalAlign: 'top' }}><time dateTime={e.time}>{new Date(e.time).toLocaleString()}</time></td>
                      <td>{raw ? <pre className="agd-code agd-raw">{JSON.stringify(e, null, 2)}</pre> : <Story e={e} />}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
      {next && <div style={{ marginTop: 'var(--space-3)' }}><button className="agd-btn" type="button" onClick={more}>Load older entries</button></div>}
    </>
  )
}
