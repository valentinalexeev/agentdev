// Projects (#342): one row per repository with agents. Projects that need the person sort first. A row sets the scope
// to that project, so every page shows only its work.
import { useData } from './useData'
import { kTokens } from './format'
import { Dots, Empty, PageHead } from './ui'

export function ProjectsPage() {
  const d = useData()
  const rows = d.projects.map((p) => {
    const ag = d.allAgents.filter((a) => (a.repos ?? []).includes(p))
    return {
      p, ag,
      wait: d.inbox.filter((i) => i.repo === p && i.kind === 'approval').length,
      stopped: ag.filter((a) => a.attn === 'blocked' || a.attn === 'guardrail').length,
      working: ag.filter((a) => a.life === 'running' || a.life === 'starting').length,
      tokens: ag.reduce((t, a) => t + (a.budgetUsed ?? 0), 0),
      events: ag.reduce((t, a) => t + (a.securityEvents ?? 0), 0),
    }
  }).sort((a, b) => (b.stopped - a.stopped) || (b.wait - a.wait) || (b.working - a.working) || a.p.localeCompare(b.p))
  const n = (v: number, cls?: string) => (v ? (cls ? <span className={cls}>{v}</span> : v) : <span className="agd-muted">0</span>)
  return (
    <>
      <PageHead title="Projects" sub="One row per repository with agents. Projects that need you sort first. Click a project to see only its work on every page." />
      <div className="agd-filters" style={{ marginBottom: 'var(--space-3)' }}>
        {d.mine && <button className="agd-chip" type="button" aria-pressed={d.scope === 'mine'} onClick={() => d.setScope('mine')}>My projects <span className="agd-data">{d.mine.length}</span></button>}
        <button className="agd-chip" type="button" aria-pressed={d.scope === ''} onClick={() => d.setScope('')}>All projects <span className="agd-data">{d.allProjects.length}</span></button>
      </div>
      {!d.ready ? <p className="agd-muted">Loading…</p> : rows.length === 0 ? <Empty title="No projects">A project shows here when an agent works on its repository.</Empty> : (
        <div className="agd-table-wrap">
          <table className="agd-table agd-table-2l">
            <thead><tr><th>Project</th><th>Agents</th><th className="agd-num">Waiting for you</th><th className="agd-num">Stopped</th>
              <th className="agd-num agd-hide-phone">Working</th><th className="agd-num agd-hide-phone">Claude units 24 h</th><th className="agd-num agd-hide-phone">Events 7 d</th></tr></thead>
            <tbody>
              {rows.map((r) => (
                <tr key={r.p} aria-selected={d.scope === `p:${r.p}`}>
                  <td><button className="agd-row-btn agd-code" type="button" title={`Show only ${r.p}`} onClick={() => { d.setScope(`p:${r.p}`); window.location.hash = 'today' }}>{r.p}</button>
                    {d.mine?.includes(r.p) && <span className="agd-muted" style={{ fontSize: 12 }}> · yours</span>}</td>
                  <td><Dots agents={r.ag} /> <span className="agd-muted agd-data" style={{ fontSize: 12 }}>{r.ag.length}</span></td>
                  <td className="agd-num">{n(r.wait, 'agd-attn agd-attn-you')}</td>
                  <td className="agd-num">{n(r.stopped, 'agd-attn agd-attn-blocked')}</td>
                  <td className="agd-num agd-hide-phone">{n(r.working)}</td>
                  <td className="agd-num agd-hide-phone">{kTokens(r.tokens)}</td>
                  <td className="agd-num agd-hide-phone">{r.events ? <a className="agd-link" href="#security">{r.events}</a> : <span className="agd-muted">0</span>}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>)}
      <p className="agd-muted" style={{ fontSize: 13 }}>Dots: <Dots agents={[{ name: 'a', life: 'running', attn: 'ok' }]} /> working,{' '}
        <Dots agents={[{ name: 'b', life: 'stopped', attn: 'ok' }]} /> idle, <Dots agents={[{ name: 'c', life: 'running', attn: 'you' }]} /> needs you,{' '}
        <Dots agents={[{ name: 'd', life: 'running', attn: 'blocked' }]} /> stopped, <Dots agents={[{ name: 'e', life: 'paused', attn: 'ok' }]} /> paused.</p>
    </>
  )
}
