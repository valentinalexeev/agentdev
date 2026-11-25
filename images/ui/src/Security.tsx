// Security page (#70): events of the guardrails, with filters. The filters live in the URL hash only for the agent.
import { useCallback, useEffect, useState } from 'react'
import { listSecurityEvents, Unauthorized, type EventFilter, type SecurityEvent } from './api'
import { AgentTag, Empty, PageHead, SevTag } from './ui'
import { useData } from './useData'

const SINCE: [string, string][] = [['24h', '24 h'], ['168h', '7 days'], ['720h', '30 days'], ['', 'all']]
const SEV: [EventFilter['severity'], string][] = [['', 'all'], ['high', 'high'], ['medium', 'medium'], ['low', 'low']]

export function SecurityPage({ token, onExpired, initialAgent }: { token: string; onExpired: () => void; initialAgent: string }) {
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

  const d = useData()
  const events = result && result.for === filter ? result.events.filter((e) => d.inScopeAgent(e.agent)) : null
  const next = result && result.for === filter ? result.next : undefined
  const more = () => {
    if (!next) return
    listSecurityEvents(token, { ...filter, cursor: next })
      .then((r) => setResult((p) => (p && p.for === filter ? { ...p, events: [...p.events, ...r.events], next: r.next_cursor } : p)))
      .catch(fail)
  }

  return (
    <>
      <PageHead sub="Guardrails that refused an action. High severity, or too many events in one job, pause the agent." />
      <div className="agd-filters" style={{ marginBottom: 'var(--space-3)' }}>
        <label className="agd-field" style={{ flexDirection: 'row', alignItems: 'center', gap: 'var(--space-2)' }}><span>Agent</span>
          <input className="agd-input" style={{ width: 160 }} value={filter.agent ?? ''} placeholder="all"
            onChange={(e) => setFilter({ ...filter, agent: e.target.value.trim() })} /></label>
        <div className="agd-seg" role="group" aria-label="Severity">
          {SEV.map(([v, l]) => <button key={l} type="button" aria-pressed={filter.severity === v} onClick={() => setFilter({ ...filter, severity: v })}>{l}</button>)}
        </div>
        <div className="agd-seg" role="group" aria-label="Time">
          {SINCE.map(([v, l]) => <button key={l} type="button" aria-pressed={filter.since === v} onClick={() => setFilter({ ...filter, since: v })}>{l}</button>)}
        </div>
      </div>
      {error && <p className="agd-error" role="alert">{error}</p>}
      {!events && !error && <p className="agd-muted">Loading events…</p>}
      {events && events.length === 0 && <Empty title="No security events for this filter">When a guardrail refuses an action, it shows here.</Empty>}
      {events && events.length > 0 && (
        <div className="agd-table-wrap">
          <table className="agd-table agd-table-2l">
            <thead><tr><th>Time</th><th>Agent</th><th>Severity</th><th>Source / rule</th><th>Job</th><th>Action</th></tr></thead>
            <tbody>
              {events.map((e) => (
                <tr key={e.id}>
                  <td className="agd-num" style={{ textAlign: 'left' }}>{new Date(e.time).toLocaleString()}</td>
                  <td><AgentTag name={e.agent} /></td>
                  <td><SevTag severity={e.severity} /></td>
                  <td className="agd-code">{e.source} / {e.rule}</td>
                  <td className="agd-code">{e.job ?? ''}</td>
                  <td style={{ overflowWrap: 'anywhere' }}>{e.action}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {next && <p><button className="agd-btn" type="button" onClick={more}>Load more</button></p>}
    </>
  )
}
