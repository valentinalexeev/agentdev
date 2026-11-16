// Agent drawer (#344, docs/console-ux.md §7): overview with a "waiting on" panel, recent jobs as a timeline, usage,
// settings and the harness guide. Opened by #agents/<name>, so a link to one agent can be shared. Everything the
// API returns here comes from trusted sources (Agent object, pods, approvals, usage pauses, operator guide), except
// job titles and approval tools, which people or agents wrote on the forge: all of it is rendered as plain text.
import { useCallback, useEffect, useState } from 'react'
import {
  getAgentDetail, patchAgent, statusOf, Unauthorized, formatBudget, maxConcurrentError, type AgentDetail, type Job, type WaitingItem,
} from './api'
import { ATTN_LABEL } from './model'
import { useData } from './useData'
import { ago, left, outcomeText } from './format'
import { AgentTag, Attention, Drawer, Empty, Icon, Lamp } from './ui'

type Tab = 'overview' | 'jobs' | 'usage' | 'settings' | 'guide'
const TABS: [Tab, string][] = [['overview', 'Overview'], ['jobs', 'Jobs'], ['usage', 'Usage'], ['settings', 'Settings'], ['guide', 'Guide']]

export function AgentDrawer({ name, token, onExpired, onClose }: { name: string; token: string; onExpired: () => void; onClose: () => void }) {
  const d = useData()
  const [detail, setDetail] = useState<AgentDetail | null>(null)
  const [error, setError] = useState('')
  const [tab, setTab] = useState<Tab>('overview')
  const [tick, setTick] = useState(0)
  const view = d.allAgents.find((a) => a.name === name)

  useEffect(() => {
    let alive = true
    getAgentDetail(token, name)
      .then((r) => { if (alive) { setDetail(r); setError('') } })
      .catch((e) => {
        if (!alive) return
        if (e instanceof Unauthorized) onExpired()
        else setError(statusOf(e) === 404 ? 'not-found' : (e as Error).message)
      })
    return () => { alive = false }
    // d.at: read the detail again after every update of the console data
  }, [token, name, onExpired, tick, d.at])

  const reload = useCallback(() => { setTick((n) => n + 1); d.reload() }, [d])

  // arrow keys move between the tabs (WAI-ARIA tabs pattern)
  const onTabKey = (e: React.KeyboardEvent) => {
    const i = TABS.findIndex(([t]) => t === tab)
    const n = e.key === 'ArrowRight' ? (i + 1) % TABS.length : e.key === 'ArrowLeft' ? (i + TABS.length - 1) % TABS.length : -1
    if (n < 0) return
    e.preventDefault()
    setTab(TABS[n][0])
    document.getElementById(`agent-tab-${TABS[n][0]}`)?.focus()
  }

  return (
    <Drawer title={<>Agent <AgentTag name={name} /></>} onClose={onClose}>
      {error === 'not-found'
        ? <Empty title="Agent not found">The agent does not exist, or you have no write access to one of its repositories.</Empty>
        : error && !detail
          ? <p className="ad-error" role="alert">Cannot read the agent: {error}</p>
          : !detail ? <p className="ad-muted">Loading…</p> : (
            <>
              <div className="ad-filters" style={{ gap: 'var(--space-3)' }}>
                {view ? <Lamp phase={view.life} /> : <Lamp phase={detail.phase} />}
                {view && <Attention attn={view.attn} label={ATTN_LABEL[view.attn]} />}
                <span className="ad-code">{(detail.repos ?? []).join(', ')}</span>
              </div>
              {error && <p className="ad-error" role="alert">Cannot update: {error}. The data below can be old.</p>}
              <div role="tablist" aria-label={`Details of ${name}`} className="ad-tabs" style={{ padding: 0, background: 'none', overflowX: 'auto' }} onKeyDown={onTabKey}>
                {TABS.map(([t, label]) => (
                  <button key={t} id={`agent-tab-${t}`} role="tab" type="button" aria-selected={tab === t} aria-controls={`agent-panel-${t}`}
                    tabIndex={tab === t ? 0 : -1} className={`ad-tab ${tab === t ? 'ad-tab-on' : ''}`} onClick={() => setTab(t)}>
                    {label}{t === 'overview' && detail.waitingOn.length > 0 && <span className="ad-count">{detail.waitingOn.length}</span>}
                  </button>
                ))}
              </div>
              <div role="tabpanel" id={`agent-panel-${tab}`} aria-labelledby={`agent-tab-${tab}`} style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
                {tab === 'overview' && <Overview a={detail} now={d.now} />}
                {tab === 'jobs' && <Jobs jobs={detail.recentJobs} now={d.now} name={name} />}
                {tab === 'usage' && <UsageTab a={detail} pause={d.usage?.pauses?.[name]} now={d.now} />}
                {tab === 'settings' && <Settings a={detail} token={token} onExpired={onExpired} onDone={reload} />}
                {tab === 'guide' && (detail.guide
                  ? <><p className="ad-muted" style={{ margin: 0, fontSize: 13 }}>The operator writes this guide from the Agent object and the harness. Copy it into an issue or a review to tell the agent how to work.</p>
                    <pre className="ad-code ad-guide">{detail.guide}</pre></>
                  : <Empty title="No guide yet">The operator writes the guide when it reconciles the agent.</Empty>)}
              </div>
            </>)}
    </Drawer>
  )
}

const WAIT: Record<string, string> = {
  approval: 'Your decision', 'usage-pause': 'Usage limit', paused: 'Paused by a person', blocked: 'Blocked by the operator', invalid: 'The Agent object is not valid',
}

function Overview({ a, now }: { a: AgentDetail; now: number }) {
  return (
    <>
      <section className="ad-section" aria-labelledby="ag-wait">
        <h3 className="ad-section-title" id="ag-wait">Waiting on</h3>
        {a.waitingOn.length === 0
          ? <p className="ad-muted" style={{ margin: 0 }}>Nothing. The agent works or waits for new issues.</p>
          : <ul className="ad-inbox">{a.waitingOn.map((w, i) => <WaitRow key={`${w.kind}${w.ref ?? i}`} w={w} now={now} />)}</ul>}
      </section>
      <section className="ad-section" aria-labelledby="ag-pods">
        <h3 className="ad-section-title" id="ag-pods">Pods</h3>
        {a.pods.length === 0
          ? <p className="ad-muted" style={{ margin: 0 }}>No pods. The agent is idle (scaled to zero) and wakes on new work. This is normal.</p>
          : <dl className="ad-dl">{a.pods.map((p) => (
            <div key={p.name} style={{ display: 'contents' }}>
              <dt className="ad-code">{p.name}</dt>
              <dd>{p.phase}{p.ready ? ', ready' : ', not ready'}{p.reason ? ` (${p.reason})` : ''}{p.restarts ? `, ${p.restarts} restarts` : ''}
                {p.started && <span className="ad-muted">, started {ago(Date.parse(p.started), now)}</span>}</dd>
            </div>))}</dl>}
      </section>
      <dl className="ad-dl">
        <dt>Forge</dt><dd>{a.forge ?? '–'}</dd>
        <dt>Max. concurrent</dt><dd>{a.maxConcurrent ?? 2}</dd>
        <dt>Scale to zero</dt><dd>{a.scaleToZero ? 'yes' : 'no'}</dd>
        <dt>Events 7 d</dt><dd>{a.securityEvents
          ? <a className="ad-link" href={`#security?agent=${encodeURIComponent(a.name)}`}>{a.securityEvents} security events</a> : '0'}</dd>
      </dl>
    </>
  )
}

function WaitRow({ w, now }: { w: WaitingItem; now: number }) {
  const until = w.until ? Date.parse(w.until) : 0
  return (
    <li className="ad-item" style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 'var(--space-2) var(--space-3)' }}>
      <div style={{ minWidth: 0, flex: '1 1 200px' }}>
        <strong>{WAIT[w.kind] ?? w.kind}</strong>
        {(w.reason || w.job) && <div className="ad-muted" style={{ fontSize: 13, overflowWrap: 'anywhere' }}>{[w.kind === 'paused' ? '' : w.reason, w.job && `job ${w.job}`, w.repo].filter(Boolean).join(' · ')}</div>}
      </div>
      <div className="ad-row-actions" style={{ alignItems: 'center' }}>
        {w.kind === 'approval' && w.ref && <a className="ad-btn ad-btn-sm" href={`#approvals?open=${encodeURIComponent(w.ref)}`}>Decide</a>}
        {w.kind === 'usage-pause' && <a className="ad-btn ad-btn-sm" href="#usage">Usage</a>}
        {until > 0 && <span className="ad-data">{left(until, now)}</span>}
      </div>
    </li>
  )
}

const PHASE_LABEL: Record<string, string> = { queued: 'plan', build: 'build', review: 'review', done: 'done' }

function Jobs({ jobs, now, name }: { jobs: Job[]; now: number; name: string }) {
  if (jobs.length === 0) return <Empty title="No recent jobs">Assign an issue to {name} on the forge. The job shows here.</Empty>
  return (
    <ol className="ad-timeline" aria-label={`Recent jobs of ${name}`}>
      {jobs.map((j) => {
        const failed = j.phase === 'done' && j.outcome !== 'merged'
        return (
          <li key={j.id} className={`ad-tl ${j.phase === 'review' ? 'ad-tl-person' : failed ? 'ad-tl-alert' : 'ad-tl-agent'}`}>
            <span className="ad-tl-time">{ago(Date.parse(j.updated), now).replace(' ago', '')}</span>
            <span className="ad-tl-mark" aria-hidden="true" />
            <div className="ad-tl-text">
              <div>{j.title || '(no title)'}</div>
              <div className="ad-muted">
                {PHASE_LABEL[j.phase] ?? j.phase}{outcomeText(j) && `: ${outcomeText(j)}`}
                {j.issue ? <> · {j.url ? <a className="ad-link" href={j.url} target="_blank" rel="noreferrer noopener">issue #{j.issue}</a> : `issue #${j.issue}`}</> : null}
                {j.pr ? <> · {j.prUrl ? <a className="ad-link" href={j.prUrl} target="_blank" rel="noreferrer noopener">PR #{j.pr}</a> : `PR #${j.pr}`}</> : null}
              </div>
            </div>
          </li>
        )
      })}
    </ol>
  )
}

function UsageTab({ a, pause, now }: { a: AgentDetail; pause?: { until: number; reason: string }; now: number }) {
  const pct = a.budgetLimit && a.budgetUsed !== undefined ? Math.round((a.budgetUsed / a.budgetLimit) * 100) : null
  return (
    <>
      <dl className="ad-dl">
        <dt>Claude units 24 h</dt><dd className="ad-data">{formatBudget(a) || 'not reported'}{pct !== null ? ` (${pct}%)` : ''}</dd>
        <dt>Usage pause</dt><dd>{pause && pause.until * 1000 > now ? `${pause.reason}, until ${new Date(pause.until * 1000).toLocaleString()}` : 'none'}</dd>
      </dl>
      <p className="ad-muted" style={{ margin: 0, fontSize: 13 }}>The credential proxy counts the units. Limits per subscription are on the <a className="ad-link" href="#usage">Usage</a> page.</p>
    </>
  )
}

function Settings({ a, token, onExpired, onDone }: { a: AgentDetail; token: string; onExpired: () => void; onDone: () => void }) {
  const [max, setMax] = useState(String(a.maxConcurrent ?? 2))
  const [stz, setStz] = useState(a.scaleToZero ?? true)
  const [busy, setBusy] = useState(false)
  const [msg, setMsg] = useState<{ ok: boolean; text: string } | null>(null)
  const err = maxConcurrentError(Number(max))
  const run = async (what: string, patch: Parameters<typeof patchAgent>[2]) => {
    setBusy(true); setMsg(null)
    try { await patchAgent(token, a.name, patch); setMsg({ ok: true, text: `${what}: done. Your name is in the audit log.` }); onDone() } catch (e) {
      if (e instanceof Unauthorized) onExpired()
      else setMsg({ ok: false, text: `${what}: ${(e as Error).message}` })
    } finally { setBusy(false) }
  }
  if (a.managed === false) return <p className="ad-muted" style={{ margin: 0 }}>This agent has no Agent object. Change it in the deployment repository.</p>
  return (
    <>
      <div className="ad-row-actions">
        {a.paused
          ? <button className="ad-btn" type="button" disabled={busy} onClick={() => run(`Resume ${a.name}`, { paused: false })}><Icon name="play" />Resume</button>
          : <button className="ad-btn" type="button" disabled={busy} onClick={() => run(`Pause ${a.name}`, { paused: true })}><Icon name="pause" />Pause</button>}
      </div>
      <form className="ad-form" onSubmit={(e) => { e.preventDefault(); if (!err) run(`Change ${a.name}`, { maxConcurrent: Number(max), scaleToZero: stz }) }}>
        <label className="ad-field"><span>Max. concurrent</span>
          <input className="ad-input" type="number" min={1} max={8} required value={max} aria-invalid={!!err} onChange={(e) => setMax(e.target.value)} />
          <small>Issues and pull requests the agent works on at the same time, 1 to 8.</small></label>
        <label className="ad-check"><input type="checkbox" checked={stz} onChange={(e) => setStz(e.target.checked)} /> Scale to zero</label>
        {err && <span className="ad-error">{err}</span>}
        <div><button className="ad-btn ad-btn-primary" type="submit" disabled={busy || !!err}>Save</button></div>
      </form>
      {msg && <p className={msg.ok ? 'ad-muted' : 'ad-error'} role={msg.ok ? 'status' : 'alert'} style={{ margin: 0 }}>{msg.text}</p>}
    </>
  )
}
