// Security page (#70): events of the guardrails, with filters. The filters live in the URL hash only for the agent.
import { useCallback, useEffect, useState } from 'react'
import { listSecurityEvents, Unauthorized, type EventFilter, type SecurityEvent } from './api'
import { AgentTag, Drawer, Empty, PageHead, SevTag, Untrusted } from './ui'
import { useData } from './useData'

const SINCE: [string, string][] = [['24h', '24 h'], ['168h', '7 days'], ['720h', '30 days'], ['', 'all']]
const SEV: [EventFilter['severity'], string][] = [['', 'all'], ['high', 'high'], ['medium', 'medium'], ['low', 'low']]

export function SecurityPage({ token, onExpired, initialAgent }: { token: string; onExpired: () => void; initialAgent: string }) {
  const [filter, setFilter] = useState<EventFilter>({ agent: initialAgent, severity: '', since: '168h' })
  // the loaded page belongs to one filter; a result for an older filter is shown as loading
  const [result, setResult] = useState<{ for: EventFilter; events: SecurityEvent[]; next?: string } | null>(null)
  const [error, setError] = useState('')
  const [open, setOpen] = useState<SecurityEvent | null>(null)
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
                  <td className="agd-num" style={{ textAlign: 'left' }}>
                    <button className="agd-row-btn" type="button" aria-haspopup="dialog" title="Show the event details"
                      onClick={() => setOpen(e)}>{new Date(e.time).toLocaleString()}</button></td>
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
      {open && <EventDrawer e={open} onClose={() => setOpen(null)} />}
      {next && <p><button className="agd-btn" type="button" onClick={more}>Load more</button></p>}
    </>
  )
}

// The refused host of an egress event: "CONNECT host:443" or "GET http://host/path" (operator egressHostOf).
function egressHost(action: string): string {
  const t = action.trim().split(/\s+/)[1] ?? ''
  if (t.includes('://')) { try { return new URL(t).hostname.toLowerCase() } catch { return '' } }
  return t.split(':')[0].toLowerCase()
}

// Event drawer: every field of one event. The action is the guardrail's record of what was refused; parts of it can
// come from the agent (a command line, a URL), so it is shown as untrusted plain text.
function EventDrawer({ e, onClose }: { e: SecurityEvent; onClose: () => void }) {
  const host = e.source.startsWith('egress') ? egressHost(e.action) : ''
  const jobLink = e.job && /^(issue|pr)-[0-9]{1,10}$/.test(e.job) ? `#work?job=${encodeURIComponent(e.agent)}/${e.job}` : ''
  return (
    <Drawer title={<>Security event <span className="agd-code">#{e.id}</span></>} onClose={onClose}>
      <dl className="agd-dl">
        <dt>Time</dt><dd>{new Date(e.time).toLocaleString()} <span className="agd-muted agd-code">{e.time}</span></dd>
        <dt>Agent</dt><dd><AgentTag name={e.agent} /></dd>
        <dt>Severity</dt><dd><SevTag severity={e.severity} />{e.severity === 'high' && <span className="agd-muted"> pauses the agent</span>}</dd>
        <dt>Source</dt><dd className="agd-code">{e.source}</dd>
        <dt>Rule</dt><dd className="agd-code">{e.rule}</dd>
        {host && <><dt>Host</dt><dd className="agd-code">{host}</dd></>}
        <dt>Job</dt><dd>{jobLink ? <a className="agd-link agd-code" href={jobLink}>{e.job}</a> : <span className="agd-muted">{e.job || 'none (outside a job)'}</span>}</dd>
        <dt>Schema</dt><dd className="agd-code">{e.schema}</dd>
      </dl>
      <Untrusted text={e.action} note="What the guardrail refused, as recorded." />
    </Drawer>
  )
}
