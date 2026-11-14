// Subscription usage (#93): windows per Claude subscription, pauses, and the limits. The API decides who may change a
// limit (global: agentdev admins; project: write access, only lower than global) and the UI shows its answer.
import { useCallback, useEffect, useState } from 'react'
import { getUsage, setGlobalLimits, setProjectLimits, pctError, Unauthorized, type Usage, type UsageWindow } from './api'
import { AgentTag, Empty, Meter, PageHead } from './ui'
import { useData } from './useData'

export function UsagePage({ token, onExpired }: { token: string; onExpired: () => void }) {
  const d = useData()
  const [u, setU] = useState<Usage | null>(null)
  const [loadedAt, setLoadedAt] = useState(0) // windows past their reset are shown as -
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const fail = useCallback((e: Error) => (e instanceof Unauthorized ? onExpired() : setError(e.message)), [onExpired])
  const load = useCallback(() => { getUsage(token).then((r) => { setU(r); setLoadedAt(Date.now()); setError('') }).catch(fail) }, [token, fail])
  useEffect(() => { getUsage(token).then((r) => { setU(r); setLoadedAt(Date.now()); setError('') }).catch(fail) }, [token, fail])
  const save = (p: Promise<void>) => { setBusy(true); p.then(load).catch(fail).finally(() => setBusy(false)) }
  const pct = (w?: UsageWindow) => (w && w.resetsAt * 1000 > loadedAt ? Math.round(w.utilization * 100) : null)
  const at = (s?: number) => (s ? new Date(s * 1000).toLocaleString() : '')
  const head = <PageHead title="Usage" sub="When a window reaches its limit, the agents on that subscription start no new runs until it resets. Runs in progress finish." />
  if (!u) return <>{head}{error ? <p className="ad-error" role="alert">{error}</p> : <p className="ad-muted">Loading usage…</p>}</>
  const subs = Object.entries(u.state ?? {})
  return (
    <>
      {head}
      {error && <p className="ad-error" role="alert">{error}</p>}
      <section className="ad-section">
        <div className="ad-section-head"><h2 className="ad-section-title">Subscriptions</h2></div>
        {subs.length === 0 ? <Empty title="No runs reported yet">A subscription shows here after the first run of an agent.</Empty> : (
          <div className="ad-meters">
            {subs.map(([k, s]) => (
              <div className="ad-sub" key={k}>
                <h3><span className="ad-code">{k}</span><span className="ad-muted ad-data">{s.runs} runs · {(s.inputTokens + s.outputTokens).toLocaleString()} tokens</span></h3>
                <Meter label="5-hour" pct={pct(s.fiveHour)} limit={u.global.fiveHour} foot={`Resets ${at(s.fiveHour?.resetsAt)} · global limit ${u.global.fiveHour}%`} />
                <Meter label="Weekly" pct={pct(s.weekly)} limit={u.global.weekly} foot={`Resets ${at(s.weekly?.resetsAt)} · global limit ${u.global.weekly}%`} />
              </div>
            ))}
          </div>
        )}
      </section>
      <section className="ad-section">
        <div className="ad-section-head"><h2 className="ad-section-title">Paused agents</h2></div>
        {!u.pauses || Object.keys(u.pauses).length === 0 ? <p className="ad-muted" style={{ margin: 0 }}>No agent is paused for usage.</p> : (
          <div className="ad-table-wrap">
            <table className="ad-table">
              <thead><tr><th>Agent</th><th>Reason</th><th>Until</th></tr></thead>
              <tbody>{Object.entries(u.pauses).filter(([a]) => d.inScopeAgent(a)).map(([a, p]) => (
                <tr key={a}><td><AgentTag name={a} /></td><td>{p.reason}</td><td className="ad-data">{at(p.until)}</td></tr>))}</tbody>
            </table>
          </div>)}
      </section>
      <section className="ad-section">
        <div className="ad-section-head"><h2 className="ad-section-title">Limits</h2><span className="ad-muted">A project limit can only be lower than the global limit.</span></div>
        <div className="ad-table-wrap">
          <LimitsRow key={`g${u.global.fiveHour}/${u.global.weekly}`} label="Global" note="agentdev-admins" busy={busy} five={u.global.fiveHour} week={u.global.weekly}
            onSave={(f, w) => save(setGlobalLimits(token, { fiveHour: f, weekly: w }))} />
          {Object.keys(u.effective).filter(d.inScopeRepo).sort().map((repo) => (
            <LimitsRow key={`${repo}${JSON.stringify(u.projects?.[repo] ?? {})}${u.global.fiveHour}/${u.global.weekly}`} label={repo} busy={busy} clearable
              five={u.projects?.[repo]?.fiveHour ?? u.effective[repo].fiveHour}
              week={u.projects?.[repo]?.weekly ?? u.effective[repo].weekly}
              note={`effective ${u.effective[repo].fiveHour}% / ${u.effective[repo].weekly}%`}
              onSave={(f, w) => save(setProjectLimits(token, repo, { fiveHour: f, weekly: w }))}
              onClear={() => save(setProjectLimits(token, repo, { fiveHour: null, weekly: null }))} />
          ))}
        </div>
      </section>
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
    <div className="ad-limit-row">
      <div><strong className="ad-code">{label}</strong>{note && <div className="ad-muted" style={{ fontSize: 13 }}>{note}</div>}
        {err && <div className="ad-error">{err}</div>}</div>
      <label className="ad-field"><span>5-hour %</span>
        <input className="ad-input" type="number" min={1} max={100} value={f} aria-invalid={!!pctError(f)} onChange={(e) => setF(Number(e.target.value))} /></label>
      <label className="ad-field"><span>Weekly %</span>
        <input className="ad-input" type="number" min={1} max={100} value={w} aria-invalid={!!pctError(w)} onChange={(e) => setW(Number(e.target.value))} /></label>
      <div className="ad-row-actions">
        <button className="ad-btn" type="button" disabled={busy || !!err} onClick={() => onSave(f, w)}>Save</button>
        {clearable && <button className="ad-btn ad-btn-quiet" type="button" disabled={busy} onClick={onClear}>Use global</button>}
      </div>
    </div>
  )
}
