// Today (#342): one inbox of what waits for the person, built in the browser from the current API calls. Sort: the
// first to expire, then the oldest. Above 10 items the list groups itself by type or by project, 4 items per group.
import { useState } from 'react'
import { ApprovalDrawer } from './Approvals'
import { ATTN_LABEL, ATTN_RANK, GROUPS, KIND_GROUP, type InboxItem, type ItemKind } from './model'
import { useData } from './useData'
import { ago, CAP, GROUP_AT, left, short } from './format'
import { AgentTag, Attention, Empty, Icon, Lamp, PageHead, type IconName } from './ui'


const KIND: Record<ItemKind, [IconName, string, boolean]> = { // icon, reason chip, alert
  approval: ['approval', 'approval', false], guardrail: ['security', 'guardrail', true],
  blocked: ['blocked', 'blocked', true], limit: ['pause', 'limit', false],
}

function Item({ it, now, onOpen }: { it: InboxItem; now: number; onOpen: (it: InboxItem) => void }) {
  const [icon, reason, alert] = KIND[it.kind]
  return (
    <li className={`ad-item ${alert ? 'ad-item-alert' : ''}`}>
      <span className="ad-item-kind"><Icon name={icon} /></span>
      <div className="ad-item-body">
        <span className="ad-item-title">{it.title}</span>
        <span className="ad-item-meta"><span className="ad-reason">{reason}</span><AgentTag name={it.agent} /><span className="ad-code">{short(it.repo)} {it.where}</span></span>
      </div>
      <div className="ad-item-side">
        {it.due ? <span className={`ad-item-due ${it.due - now < 30 * 60e3 ? 'ad-item-due-soon' : ''}`}>{left(it.due, now)}</span>
          : it.since ? <span className="ad-item-due">{ago(it.since, now)}</span> : null}
        {it.kind === 'approval'
          ? <button className="ad-btn ad-btn-primary ad-btn-sm" type="button" onClick={() => onOpen(it)}>Review</button>
          : it.kind === 'limit'
            ? <a className="ad-btn ad-btn-sm" href="#usage">Usage</a>
            : <a className="ad-btn ad-btn-sm" href={`#security?agent=${encodeURIComponent(it.agent)}`}>Events</a>}
      </div>
    </li>
  )
}

function InboxList({ items, by, now, onOpen }: { items: InboxItem[]; by: 'type' | 'project'; now: number; onOpen: (it: InboxItem) => void }) {
  const [open, setOpen] = useState<Set<string>>(new Set())
  if (items.length <= GROUP_AT) return <ul className="ad-inbox">{items.map((it) => <Item key={it.key} it={it} now={now} onOpen={onOpen} />)}</ul>
  const groups = new Map<string, InboxItem[]>()
  for (const it of items) {
    const g = by === 'type' ? KIND_GROUP[it.kind] : it.repo
    if (!groups.has(g)) groups.set(g, [])
    groups.get(g)!.push(it)
  }
  const order = by === 'type' ? GROUPS.filter((g) => groups.has(g)) : [...groups.keys()].sort((a, b) => groups.get(b)!.length - groups.get(a)!.length)
  return (
    <ul className="ad-inbox">
      {order.map((g) => {
        const list = groups.get(g)!, isOpen = open.has(g), shown = isOpen ? list : list.slice(0, CAP)
        const tone = by === 'type' ? (g === 'Stopped agents' ? 'ad-group-head-alert' : g === 'Decide' ? 'ad-group-head-you' : '') : ''
        const first = list.find((i) => i.due)
        const note = first?.due ? ` · first expires in ${left(first.due, now).replace(' left', '')}` : ''
        return [
          <li key={`h${g}`} className={`ad-group-head ${tone}`}><span className="ad-label">{by === 'type' ? g : short(g)}</span>
            <span className="ad-muted" style={{ fontSize: 13 }}>{note}</span><span className="ad-data">{list.length}</span></li>,
          ...shown.map((it) => <Item key={it.key} it={it} now={now} onOpen={onOpen} />),
          list.length > CAP && (
            <li key={`m${g}`}><button className="ad-showmore" type="button" aria-expanded={isOpen}
              onClick={() => setOpen((s) => { const n = new Set(s); if (n.has(g)) n.delete(g); else n.add(g); return n })}>
              {isOpen ? 'Show fewer' : `Show ${list.length - CAP} more`}</button></li>
          ),
        ]
      })}
    </ul>
  )
}

export function TodayPage({ token, onExpired }: { token: string; onExpired: () => void }) {
  const d = useData()
  const [opened, setOpened] = useState<InboxItem | null>(null)
  const [by, setBy] = useState<'type' | 'project'>('type')
  const now = d.now
  const attn = d.agents.filter((a) => a.attn !== 'ok').sort((x, y) => ATTN_RANK[x.attn] - ATTN_RANK[y.attn])
  const stopped = attn.filter((a) => a.attn === 'guardrail' || a.attn === 'blocked')
  const working = d.agents.filter((a) => a.life === 'running' || a.life === 'starting')
  const windows = Object.entries(d.usage?.state ?? {}).flatMap(([k, s]) => [[k, '5-hour', s.fiveHour, d.usage!.global.fiveHour], [k, 'weekly', s.weekly, d.usage!.global.weekly]] as const)
    .filter(([, , w]) => w && w.resetsAt * 1000 > now)
  const low = windows.map(([k, n, w, lim]) => ({ k, n, left: Math.max(0, lim - Math.round(w.utilization * 100)) })).sort((a, b) => a.left - b.left)[0]
  const firstDue = d.inbox.find((i) => i.due)
  if (!d.ready) return <><PageHead title="Today" /><p className="ad-muted">Loading…</p></>
  return (
    <>
      <PageHead title="Today" sub={<>
        What waits for you, the first to expire on top.
        {d.hiddenInbox > 0 && <> {d.hiddenInbox} more {d.hiddenInbox === 1 ? 'item waits' : 'items wait'} in projects that are not shown:{' '}
          <button className="ad-btn ad-btn-quiet" type="button" style={{ height: 'auto', padding: 0 }} onClick={() => d.setScope('')}>show all projects</button>.</>}
      </>} />
      <div className="ad-kpis" style={{ marginBottom: 'var(--space-5)' }}>
        <a className="ad-kpi ad-kpi-you" href="#today"><span className="ad-kpi-label ad-label">Waiting for you</span>
          <span className="ad-kpi-value">{d.inbox.length}</span>
          <span className="ad-kpi-foot">{d.pending.length} approvals{firstDue?.due ? `, first expires in ${left(firstDue.due, now).replace(' left', '')}` : ''}</span></a>
        <a className={`ad-kpi ${stopped.length ? 'ad-kpi-alert' : ''}`} href="#agents"><span className="ad-kpi-label ad-label">Stopped agents</span>
          <span className="ad-kpi-value">{stopped.length}</span><span className="ad-kpi-foot">guardrail or blocked</span></a>
        <a className="ad-kpi" href="#agents"><span className="ad-kpi-label ad-label">Agents working</span>
          <span className="ad-kpi-value">{working.length}<small>/ {d.agents.length}</small></span><span className="ad-kpi-foot">the others are idle or paused</span></a>
        <a className="ad-kpi" href="#usage"><span className="ad-kpi-label ad-label">Headroom</span>
          <span className="ad-kpi-value">{low ? low.left : '–'}<small>%</small></span>
          <span className="ad-kpi-foot">{low ? `${low.k}, ${low.n} window` : 'no usage reported'}</span></a>
      </div>
      <section className="ad-section">
        <div className="ad-section-head"><h2 className="ad-section-title">Inbox</h2>
          {d.inbox.length > GROUP_AT && (
            <div className="ad-seg" role="group" aria-label="Group by">
              <button type="button" aria-pressed={by === 'type'} onClick={() => setBy('type')}>By type</button>
              <button type="button" aria-pressed={by === 'project'} onClick={() => setBy('project')}>By project</button>
            </div>)}
        </div>
        {d.inbox.length === 0
          ? <Empty title="Nothing waits for you">Approval requests and stopped agents show here.</Empty>
          : <InboxList items={d.inbox} by={by} now={now} onOpen={setOpened} />}
        <p className="ad-muted" style={{ fontSize: 13, margin: 0 }}>Questions from agents and pull requests to review are on the forge.</p>
      </section>
      <section className="ad-section">
        <div className="ad-section-head"><h2 className="ad-section-title">Fleet</h2><a className="ad-link" href="#agents">All agents</a></div>
        {attn.length === 0 ? <p className="ad-muted" style={{ margin: 0 }}>All {d.agents.length} agents are ok.</p> : (
          <div className="ad-table-wrap"><table className="ad-table"><tbody>
            {attn.slice(0, 6).map((a) => (
              <tr key={a.name}><td><AgentTag name={a.name} /></td><td className="ad-code ad-hide-phone">{short(a.repo)}</td>
                <td><Lamp phase={a.life} /></td><td><Attention attn={a.attn} label={ATTN_LABEL[a.attn]} /></td>
                <td className="ad-muted ad-hide-phone" style={{ fontSize: 13 }}>{a.why}</td></tr>
            ))}
          </tbody></table></div>)}
        <p className="ad-muted" style={{ fontSize: 13, margin: 0 }}>
          {attn.length > 6 && <><a className="ad-link" href="#agents">{attn.length - 6} more</a> · </>}{d.agents.length - attn.length} other agents are ok.</p>
      </section>
      {opened?.approval && <ApprovalDrawer token={token} a={opened.approval} onExpired={onExpired} onClose={() => setOpened(null)}
        onDecided={() => { setOpened(null); d.reload() }} />}
    </>
  )
}
