// Claude cost page (#196, ADR 0002 U5): units (and estimated USD when prices are configured) per agent, the budget of
// each agent (agentdev administrators can change it; claude-proxy enforces it), and the cost of each job of the last
// 7 days. The job id comes from the agent container, so this view is a report, not an audit.
import { useCallback, useEffect, useState } from 'react'
import {
  listAgents, getCosts, setAgentBudget, unitsError, formatUnits, formatUSD, cacheShare, Unauthorized,
  type Agent, type JobCost,
} from './api'
import { Modal } from './App'

function jobLabel(j: JobCost): string {
  const m = /^(issue|pr)-([0-9]+)$/.exec(j.job)
  return m ? `${m[1] === 'pr' ? 'PR' : 'Issue'} #${m[2]}` : j.job
}

export function Costs({ token, onExpired }: { token: string; onExpired: () => void }) {
  const [agents, setAgents] = useState<Agent[] | null>(null)
  const [jobs, setJobs] = useState<{ available: boolean; jobs: JobCost[] } | null>(null)
  const [error, setError] = useState('')
  const [agentFilter, setAgentFilter] = useState('')
  const [edit, setEdit] = useState<Agent | null>(null)
  const [value, setValue] = useState('')
  const [busy, setBusy] = useState(false)
  const [notice, setNotice] = useState('')
  const [reload, setReload] = useState(0)
  const fail = useCallback((e: Error) => (e instanceof Unauthorized ? onExpired() : setError(e.message)), [onExpired])

  useEffect(() => {
    let alive = true
    const load = () => {
      listAgents(token).then((a) => { if (alive) setAgents(a) }).catch(fail)
      getCosts(token).then((c) => { if (alive) { setJobs(c); setError('') } }).catch(fail)
    }
    load()
    const t = setInterval(load, 30000)
    return () => { alive = false; clearInterval(t) }
  }, [token, fail, reload])

  const save = async (units: number | null) => {
    if (!edit) return
    setBusy(true); setNotice('')
    try {
      await setAgentBudget(token, edit.name, units)
      setEdit(null); setReload((n) => n + 1)
    } catch (e) {
      if (e instanceof Unauthorized) onExpired()
      else setNotice((e as Error).message)
    } finally { setBusy(false) }
  }

  const shown = (jobs?.jobs ?? []).filter((j) => !agentFilter || j.agent === agentFilter)
  const anyUSD = (jobs?.jobs ?? []).some((j) => j.usd !== undefined) || (agents ?? []).some((a) => a.budgetUsd !== undefined)
  const err = unitsError(value)
  return (
    <>
      <h1>Claude cost</h1>
      <p className="muted">
        Cost units are input-token equivalents of the reference model: a cache read counts 0.1, output 5. The Claude
        proxy counts them and stops an agent at its budget for 24 hours.
        {anyUSD ? ' USD is an estimate from the configured prices, not the bill.' : ''}
      </p>
      {error && <p className="error">{error}</p>}
      <h2>Agents</h2>
      {!agents ? <p>Loading…</p> : (
        <table>
          <thead><tr><th>Agent</th><th>Units (24 h)</th><th>Budget</th>{anyUSD && <th>USD (24 h)</th>}<th></th></tr></thead>
          <tbody>
            {agents.map((a) => (
              <tr key={a.name}>
                <td>{a.name}</td>
                <td className={a.budgetLimit && (a.budgetUsed ?? 0) >= a.budgetLimit ? 'error' : undefined}>
                  {a.budgetUsed === undefined ? <span className="muted">-</span> : formatUnits(a.budgetUsed)}
                </td>
                <td>{a.budgetLimit === undefined ? <span className="muted">-</span> : a.budgetLimit === 0 ? 'no limit' : formatUnits(a.budgetLimit)}</td>
                {anyUSD && <td>{formatUSD(a.budgetUsd)}</td>}
                <td><button className="link" onClick={() => { setEdit(a); setValue(a.budgetLimit ? String(a.budgetLimit) : ''); setNotice('') }}>Change budget</button></td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <h2>Cost per job (7 days)</h2>
      {!jobs ? <p>Loading…</p> : !jobs.available ? <p className="muted">The Claude proxy does not answer; no job costs.</p> : (
        <>
          <label>Agent{' '}
            <select value={agentFilter} onChange={(e) => setAgentFilter(e.target.value)}>
              <option value="">all</option>
              {[...new Set(jobs.jobs.map((j) => j.agent))].sort().map((a) => <option key={a} value={a}>{a}</option>)}
            </select>
          </label>
          {shown.length === 0 ? <p className="muted">No jobs yet.</p> : (
            <table>
              <thead><tr><th>Job</th><th>Agent</th><th>Units</th>{anyUSD && <th>USD</th>}<th>Requests</th><th>Cache reads</th><th>Last</th></tr></thead>
              <tbody>
                {shown.map((j) => {
                  const share = cacheShare(j.tokens)
                  return (
                    <tr key={`${j.agent}/${j.job}`}>
                      <td>{jobLabel(j)}{j.stopped && <span className="badge" title="stopped by the job budget">stopped</span>}</td>
                      <td>{j.agent}</td>
                      <td>{formatUnits(j.units)}</td>
                      {anyUSD && <td>{formatUSD(j.usd)}</td>}
                      <td>{j.requests}</td>
                      <td>{share === undefined ? '-' : `${Math.round(share * 100)} %`}</td>
                      <td>{new Date(j.last).toLocaleString()}</td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          )}
        </>
      )}
      {edit && (
        <Modal title={`Budget of ${edit.name}`} onClose={() => setEdit(null)}>
          <p>Cost units per 24 hours. 0 means no limit. Empty means the default of the instance. Only agentdev
            administrators can change it. The change is recorded as a security event.</p>
          <input aria-label="units" value={value} onChange={(e) => setValue(e.target.value.trim())} inputMode="numeric" />
          {err && <p className="error">{err}</p>}
          {notice && <p className="error">{notice}</p>}
          <p>
            <button disabled={busy || !!err} onClick={() => save(value === '' ? null : Number(value))}>Save</button>{' '}
            <button className="link" onClick={() => setEdit(null)}>Cancel</button>
          </p>
        </Modal>
      )}
    </>
  )
}
