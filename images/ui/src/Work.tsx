// Work page (#345, docs/console-ux.md §7 and §9): the jobs of the agents by phase. For one project a board with the
// columns plan, build, review and done (review is the person's turn). For several projects with more than 20 jobs a
// phase matrix: one row per project, one column per phase, counts as links; under it "Your turn", the pull requests to
// review, oldest first. A list view shows every job in one table. The jobs come from GET /api/jobs with the scope
// of the top bar (scope=mine or repo=) and filters by agent and repository in the URL (#work?agent=x&repo=o/n).
// Job titles are written on the forge by people or agents: plain text only.
import { useCallback, useEffect, useMemo, useState } from 'react'
import { listJobs, Unauthorized, type Job, type JobPhase } from './api'
import { ago, outcomeText, runLogHash, short } from './format'
import { AgentTag, Empty, PageHead } from './ui'
import { useData } from './useData'

const COLS: [JobPhase, string, string][] = [['queued', 'Plan', 'agent'], ['build', 'Build', 'agent'], ['review', 'Review', 'your turn'], ['done', 'Done', 'finished']]
const STEP: Record<JobPhase, string> = { queued: 'plan', build: 'build', review: 'review', done: 'done' }
const MATRIX_AT = 20
const DONE_DAYS = 7
const REFRESH_MS = 60000

function setHash(agent: string, repo: string) {
  const q = new URLSearchParams()
  if (agent) q.set('agent', agent)
  if (repo) q.set('repo', repo)
  const s = q.toString()
  window.location.hash = s ? `work?${s}` : 'work'
}

function Card({ j, blocked, now }: { j: Job; blocked: string; now: number }) {
  const ci = COLS.findIndex(([p]) => p === j.phase)
  const out = outcomeText(j)
  const failed = j.phase === 'done' && j.outcome !== 'merged'
  return (
    <article className={`ad-card ${j.phase === 'review' ? 'ad-card-review' : ''} ${blocked ? 'ad-card-blocked' : ''}`}>
      <div className="ad-card-top">
        <span className="ad-data">{short(j.repo)}{j.issue ? ` #${j.issue}` : ''}{j.pr ? ` · PR #${j.pr}` : ''}</span><AgentTag name={j.agent} />
      </div>
      <h3 className="ad-card-title">{j.title || '(no title)'}</h3>
      <ol className="ad-track" aria-label={`Phase: ${blocked ? 'blocked in ' : ''}${STEP[j.phase]}`}>
        {COLS.map(([p], i) => {
          const cls = blocked && i === ci ? 'ad-step-blocked' : i < ci ? 'ad-step-past' : i === ci ? 'ad-step-now' : 'ad-step-next'
          return <li key={p} className={`ad-step ad-step-${STEP[p]} ${cls}`}><span className="ad-label">{blocked && i === ci ? 'blocked' : STEP[p]}</span></li>
        })}
      </ol>
      {blocked && <p className="ad-card-note">{blocked}</p>}
      {out && <p className="ad-card-note" style={failed ? { color: 'var(--danger)' } : undefined}>Outcome: {out}</p>}
      <p className="ad-card-note">
        {j.url && <a className="ad-link" href={j.url} target="_blank" rel="noreferrer noopener">Issue</a>}
        {j.url && j.prUrl && ' · '}
        {j.prUrl && <a className="ad-link" href={j.prUrl} target="_blank" rel="noreferrer noopener">Pull request</a>}
        {' · '}<a className="ad-link" href={runLogHash(j)}>Run log</a>
        {' · '}updated {ago(Date.parse(j.updated), now)}
      </p>
    </article>
  )
}

function Board({ jobs, blockedOf, now }: { jobs: Job[]; blockedOf: (j: Job) => string; now: number }) {
  return (
    // focusable, so the arrow keys scroll the columns on a phone
    <div className="ad-board" tabIndex={0} role="region" aria-label="Board: plan, build, review, done">
      {COLS.map(([p, label, who]) => {
        const l = jobs.filter((j) => j.phase === p)
        return (
          <section key={p} className={`ad-col ad-col-${STEP[p]}`} aria-label={`${label}, ${l.length} ${l.length === 1 ? 'job' : 'jobs'}`}>
            <div className="ad-col-head"><span className="ad-label">{label} · {who}</span><span className="ad-data">{l.length}</span></div>
            {l.length === 0 ? <p className="ad-muted" style={{ fontSize: 13, margin: 0 }}>Nothing here.</p>
              : l.map((j) => <Card key={j.id} j={j} blocked={blockedOf(j)} now={now} />)}
          </section>
        )
      })}
    </div>
  )
}

function Matrix({ jobs, blockedOf, now }: { jobs: Job[]; blockedOf: (j: Job) => string; now: number }) {
  const [all, setAll] = useState(false)
  const rows = [...new Set(jobs.map((j) => j.repo))].map((p) => {
    const jb = jobs.filter((j) => j.repo === p)
    const c = Object.fromEntries(COLS.map(([ph]) => [ph, jb.filter((j) => j.phase === ph).length])) as Record<JobPhase, number>
    return { p, c, blocked: jb.filter((j) => j.phase === 'build' && blockedOf(j)).length }
  }).sort((a, b) => (b.blocked - a.blocked) || (b.c.review - a.c.review) || a.p.localeCompare(b.p))
  const tot = (ph: JobPhase) => jobs.filter((j) => j.phase === ph).length
  const review = jobs.filter((j) => j.phase === 'review').sort((a, b) => Date.parse(a.updated) - Date.parse(b.updated))
  const cell = (r: typeof rows[number], ph: JobPhase, label: string) => {
    const n = r.c[ph]
    const cls = !n ? 'ad-cell-zero' : ph === 'review' ? 'ad-cell-you' : ph === 'build' && r.blocked ? 'ad-cell-alert' : ph === 'done' ? '' : 'ad-cell-agent'
    return (
      <td key={ph} className={`ad-cell ${cls}`}>
        {n ? <button type="button" onClick={() => setHash('', r.p)} aria-label={`${n} in ${label}${ph === 'build' && r.blocked ? `, ${r.blocked} blocked` : ''} in ${r.p}, open the board`}>
          {n}{ph === 'build' && r.blocked ? <span className="ad-hide-phone"> · {r.blocked} blocked</span> : ''}</button> : '·'}
      </td>
    )
  }
  return (
    <>
      <div className="ad-table-wrap">
        <table className="ad-table ad-matrix">
          <thead><tr><th>Project</th>{COLS.map(([ph, label]) => <th key={ph} className="ad-cell">{ph === 'done' ? `Done ${DONE_DAYS} d` : label}<span className="ad-hide-phone"> · {tot(ph)}</span></th>)}</tr></thead>
          <tbody>
            {rows.map((r) => (
              <tr key={r.p}>
                <td><button className="ad-row-btn" type="button" title={r.p} onClick={() => setHash('', r.p)}>{short(r.p)}</button></td>
                {COLS.map(([ph, label]) => cell(r, ph, label))}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <section className="ad-section" style={{ marginTop: 'var(--space-5)' }}>
        <div className="ad-section-head"><h2 className="ad-section-title">Your turn: {review.length} pull {review.length === 1 ? 'request' : 'requests'}, oldest first</h2></div>
        {review.length === 0 ? <p className="ad-muted" style={{ margin: 0 }}>No pull request waits for a review.</p> : (
          <div className="ad-table-wrap">
            <table className="ad-table ad-table-2l">
              <thead><tr><th>Pull request</th><th>Agent</th><th className="ad-hide-phone">Project</th><th className="ad-num">Waiting</th></tr></thead>
              <tbody>
                {review.slice(0, all ? undefined : 8).map((j) => (
                  <tr key={j.id}>
                    <td>{j.prUrl ? <a className="ad-link" href={j.prUrl} target="_blank" rel="noreferrer noopener">PR #{j.pr}</a> : `PR #${j.pr ?? '?'}`} {j.title}</td>
                    <td><AgentTag name={j.agent} /></td>
                    <td className="ad-hide-phone ad-code">{short(j.repo)}</td>
                    <td className="ad-num">{ago(Date.parse(j.updated), now).replace(' ago', '')}</td>
                  </tr>
                ))}
              </tbody>
            </table>
            {review.length > 8 && <button className="ad-showmore" type="button" aria-expanded={all} style={{ paddingLeft: 'var(--space-3)' }}
              onClick={() => setAll(!all)}>{all ? 'Show fewer' : `Show ${review.length - 8} more`}</button>}
          </div>
        )}
      </section>
    </>
  )
}

function List({ jobs, blockedOf, now }: { jobs: Job[]; blockedOf: (j: Job) => string; now: number }) {
  return (
    <div className="ad-table-wrap">
      <table className="ad-table ad-table-2l">
        <thead><tr><th>Job</th><th>Agent</th><th>Phase</th><th className="ad-hide-phone">Project</th><th className="ad-num ad-hide-phone">Updated</th></tr></thead>
        <tbody>
          {jobs.map((j) => (
            <tr key={j.id}>
              <td>{j.url ? <a className="ad-link" href={j.url} target="_blank" rel="noreferrer noopener">#{j.issue ?? j.pr}</a> : `#${j.issue ?? j.pr}`} {j.title}
                {j.prUrl && <> · <a className="ad-link" href={j.prUrl} target="_blank" rel="noreferrer noopener">PR #{j.pr}</a></>}
                {' · '}<a className="ad-link" href={runLogHash(j)}>Run log</a></td>
              <td><AgentTag name={j.agent} /></td>
              <td>{blockedOf(j) ? <span className="ad-error">blocked</span> : STEP[j.phase]}{outcomeText(j) && <div className="ad-muted" style={{ fontSize: 13 }}>{outcomeText(j)}</div>}</td>
              <td className="ad-hide-phone ad-code">{short(j.repo)}</td>
              <td className="ad-num ad-hide-phone">{ago(Date.parse(j.updated), now).replace(' ago', '')}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

export function WorkPage({ token, onExpired, agent, repo }: { token: string; onExpired: () => void; agent: string; repo: string }) {
  const d = useData()
  const [res, setRes] = useState<{ key: string; jobs: Job[]; unavailable: string[]; at: number } | null>(null)
  const [error, setError] = useState('')
  const [view, setView] = useState<'auto' | 'list'>('auto')
  const [tick, setTick] = useState(0)
  // the scope of the top bar goes to the API: one project = repo=, My projects = scope=mine
  const q = useMemo(() => {
    const r = repo || (d.scope.startsWith('p:') ? d.scope.slice(2) : '')
    return { repo: r, scope: !r && d.scope === 'mine' ? 'mine' : '', agent }
  }, [repo, d.scope, agent])
  const key = JSON.stringify(q)
  // the agents changed (a new job, a finished job): read the jobs again, at most once per 10 s
  const agentsKey = d.agents.map((a) => `${a.name}:${a.phase}:${a.ready}`).join(',')

  const load = useCallback(async () => {
    try {
      const r = await listJobs(token, q)
      setRes({ key, jobs: r.jobs, unavailable: r.unavailable ?? [], at: Date.now() })
      setError('')
    } catch (e) {
      if (e instanceof Unauthorized) onExpired()
      else setError((e as Error).message)
    }
  }, [token, q, key, onExpired])
  useEffect(() => {
    const t0 = setTimeout(load, 0)
    const t = setInterval(load, REFRESH_MS)
    return () => { clearTimeout(t0); clearInterval(t) }
  }, [load, tick])
  useEffect(() => {
    if (!agentsKey) return
    const t = setTimeout(load, 10000)
    return () => clearTimeout(t)
  }, [agentsKey, load])

  const stopped = useMemo(() => new Map(d.allAgents.filter((a) => a.attn === 'blocked' || a.attn === 'guardrail').map((a) => [a.name, a.why])), [d.allAgents])
  const blockedOf = useCallback((j: Job) => (j.phase === 'build' || j.phase === 'queued') && stopped.has(j.agent) ? `${j.agent} is ${stopped.get(j.agent)}` : '', [stopped])

  const fresh = res && res.key === key ? res : null
  const now = d.now
  const jobs = (fresh?.jobs ?? []).filter((j) => d.inScopeRepo(j.repo)
    && (j.phase !== 'done' || now - Date.parse(j.closed || j.updated) < DONE_DAYS * 86400e3))
  const projects = new Set(jobs.map((j) => j.repo))
  const multi = !repo && projects.size > 1 && jobs.length > MATRIX_AT
  const matrix = view === 'auto' && multi
  const agentsHere = [...new Set((fresh?.jobs ?? []).map((j) => j.agent))].sort()
  const reposHere = [...new Set([...d.projects, ...(fresh?.jobs ?? []).map((j) => j.repo)])].sort()
  const n = (p: JobPhase) => jobs.filter((j) => j.phase === p).length

  return (
    <>
      <PageHead title="Work" sub={fresh
        ? `${jobs.length} ${jobs.length === 1 ? 'job' : 'jobs'} in ${projects.size} ${projects.size === 1 ? 'project' : 'projects'}: ${n('queued')} plan, ${n('build')} build, ${n('review')} review, ${n('done')} done in ${DONE_DAYS} days. The review column is your turn.`
        : 'Every issue an agent works on, by phase. The review column is your turn.'}
        actions={<button className="ad-btn" type="button" onClick={() => setTick((t) => t + 1)}>Refresh</button>} />
      <div className="ad-filters" style={{ marginBottom: 'var(--space-3)' }}>
        <label className="ad-field ad-field-inline"><span>Agent</span>
          <select className="ad-input" value={agent} onChange={(e) => setHash(e.target.value, repo)}>
            <option value="">all</option>
            {[...new Set([...agentsHere, ...(agent ? [agent] : [])])].map((a) => <option key={a} value={a}>{a}</option>)}
          </select></label>
        <label className="ad-field ad-field-inline"><span>Project</span>
          <select className="ad-input" value={repo} onChange={(e) => setHash(agent, e.target.value)}>
            <option value="">all in scope</option>
            {[...new Set([...reposHere, ...(repo ? [repo] : [])])].map((r) => <option key={r} value={r}>{r}</option>)}
          </select></label>
        <div className="ad-seg" role="group" aria-label="View">
          <button type="button" aria-pressed={view === 'auto'} onClick={() => setView('auto')}>{multi ? 'Matrix' : 'Board'}</button>
          <button type="button" aria-pressed={view === 'list'} onClick={() => setView('list')}>List</button>
        </div>
        {(agent || repo) && <button className="ad-btn ad-btn-quiet" type="button" onClick={() => setHash('', '')}>Clear filters</button>}
      </div>
      {error && <p className="ad-error" role="alert">Cannot read the jobs: {error}{fresh ? '. The data below can be old.' : ''}</p>}
      {fresh && fresh.unavailable.length > 0 && (
        <p className="ad-muted" role="status" style={{ fontSize: 13 }}>The forge did not answer for {fresh.unavailable.join(', ')}. Jobs of {fresh.unavailable.length === 1 ? 'this project' : 'these projects'} can be missing.</p>
      )}
      {!fresh ? (!error && <p className="ad-muted">Loading jobs…</p>)
        : jobs.length === 0 ? <Empty title="No jobs">Assign an issue to an agent on the forge. The job shows here in its phase.</Empty>
          : view === 'list' ? <List jobs={jobs} blockedOf={blockedOf} now={now} />
            : matrix ? <Matrix jobs={jobs} blockedOf={blockedOf} now={now} />
              : <Board jobs={jobs} blockedOf={blockedOf} now={now} />}
    </>
  )
}
