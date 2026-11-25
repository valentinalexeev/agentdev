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
    <li className={`agd-item ${alert ? 'agd-item-alert' : ''}`}>
      <span className="agd-item-kind"><Icon name={icon} /></span>
      <div className="agd-item-body">
        <span className="agd-item-title">{it.title}</span>
        <span className="agd-item-meta"><span className="agd-reason">{reason}</span><AgentTag name={it.agent} /><span className="agd-code">{short(it.repo)} {it.where}</span></span>
      </div>
      <div className="agd-item-side">
        {it.due ? <span className={`agd-item-due ${it.due - now < 30 * 60e3 ? 'agd-item-due-soon' : ''}`}>{left(it.due, now)}</span>
          : it.since ? <span className="agd-item-due">{ago(it.since, now)}</span> : null}
        {it.kind === 'approval'
          ? <button className="agd-btn agd-btn-primary agd-btn-sm" type="button" onClick={() => onOpen(it)}>Review</button>
          : it.kind === 'limit'
            ? <a className="agd-btn agd-btn-sm" href="#usage">Usage</a>
            : <a className="agd-btn agd-btn-sm" href={`#security?agent=${encodeURIComponent(it.agent)}`}>Events</a>}
      </div>
    </li>
  )
}

function InboxList({ items, by, now, onOpen }: { items: InboxItem[]; by: 'type' | 'project'; now: number; onOpen: (it: InboxItem) => void }) {
  const [open, setOpen] = useState<Set<string>>(new Set())
  if (items.length <= GROUP_AT) return <ul className="agd-inbox">{items.map((it) => <Item key={it.key} it={it} now={now} onOpen={onOpen} />)}</ul>
  const groups = new Map<string, InboxItem[]>()
  for (const it of items) {
    const g = by === 'type' ? KIND_GROUP[it.kind] : it.repo
    if (!groups.has(g)) groups.set(g, [])
    groups.get(g)!.push(it)
  }
  const order = by === 'type' ? GROUPS.filter((g) => groups.has(g)) : [...groups.keys()].sort((a, b) => groups.get(b)!.length - groups.get(a)!.length)
  return (
    <ul className="agd-inbox">
      {order.map((g) => {
        const list = groups.get(g)!, isOpen = open.has(g), shown = isOpen ? list : list.slice(0, CAP)
        const tone = by === 'type' ? (g === 'Stopped agents' ? 'agd-group-head-alert' : g === 'Decide' ? 'agd-group-head-you' : '') : ''
        const first = list.find((i) => i.due)
        const note = first?.due ? ` · first expires in ${left(first.due, now).replace(' left', '')}` : ''
        return [
          <li key={`h${g}`} className={`agd-group-head ${tone}`}><span className="agd-label">{by === 'type' ? g : short(g)}</span>
            <span className="agd-muted" style={{ fontSize: 13 }}>{note}</span><span className="agd-data">{list.length}</span></li>,
          ...shown.map((it) => <Item key={it.key} it={it} now={now} onOpen={onOpen} />),
          list.length > CAP && (
            <li key={`m${g}`}><button className="agd-showmore" type="button" aria-expanded={isOpen}
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
  if (!d.ready) return <><PageHead title="Today" /><p className="agd-muted">Loading…</p></>
  return (
    <>
      <PageHead title="Today" sub={<>
        What waits for you, the first to expire on top.
        {d.hiddenInbox > 0 && <> {d.hiddenInbox} more {d.hiddenInbox === 1 ? 'item waits' : 'items wait'} in projects that are not shown:{' '}
          <button className="agd-btn agd-btn-quiet" type="button" style={{ height: 'auto', padding: 0 }} onClick={() => d.setScope('')}>show all projects</button>.</>}
      </>} />
      <div className="agd-kpis" style={{ marginBottom: 'var(--space-5)' }}>
        <a className="agd-kpi agd-kpi-you" href="#today"><span className="agd-kpi-label agd-label">Waiting for you</span>
          <span className="agd-kpi-value">{d.inbox.length}</span>
          <span className="agd-kpi-foot">{d.pending.length} approvals{firstDue?.due ? `, first expires in ${left(firstDue.due, now).replace(' left', '')}` : ''}</span></a>
        <a className={`agd-kpi ${stopped.length ? 'agd-kpi-alert' : ''}`} href="#agents"><span className="agd-kpi-label agd-label">Stopped agents</span>
          <span className="agd-kpi-value">{stopped.length}</span><span className="agd-kpi-foot">guardrail or blocked</span></a>
        <a className="agd-kpi" href="#agents"><span className="agd-kpi-label agd-label">Agents working</span>
          <span className="agd-kpi-value">{working.length}<small>/ {d.agents.length}</small></span><span className="agd-kpi-foot">the others are idle or paused</span></a>
        <a className="agd-kpi" href="#usage"><span className="agd-kpi-label agd-label">Headroom</span>
          <span className="agd-kpi-value">{low ? low.left : '–'}<small>%</small></span>
          <span className="agd-kpi-foot">{low ? `${low.k}, ${low.n} window` : 'no usage reported'}</span></a>
      </div>
      <section className="agd-section">
        <div className="agd-section-head"><h2 className="agd-section-title">Inbox</h2>
          {d.inbox.length > GROUP_AT && (
            <div className="agd-seg" role="group" aria-label="Group by">
              <button type="button" aria-pressed={by === 'type'} onClick={() => setBy('type')}>By type</button>
              <button type="button" aria-pressed={by === 'project'} onClick={() => setBy('project')}>By project</button>
            </div>)}
        </div>
        {d.inbox.length === 0
          ? <Empty title="Nothing waits for you">Approval requests and stopped agents show here.</Empty>
          : <InboxList items={d.inbox} by={by} now={now} onOpen={setOpened} />}
        <p className="agd-muted" style={{ fontSize: 13, margin: 0 }}>Questions from agents and pull requests to review are on the forge.</p>
      </section>
      <section className="agd-section">
        <div className="agd-section-head"><h2 className="agd-section-title">Fleet</h2><a className="agd-link" href="#agents">All agents</a></div>
        {attn.length === 0 ? <p className="agd-muted" style={{ margin: 0 }}>All {d.agents.length} agents are ok.</p> : (
          <div className="agd-table-wrap"><table className="agd-table agd-table-2l"><tbody>
            {attn.slice(0, 6).map((a) => (
              <tr key={a.name}><td><AgentTag name={a.name} /></td><td className="agd-code agd-hide-phone">{short(a.repo)}</td>
                <td><Lamp phase={a.life} /></td><td><Attention attn={a.attn} label={ATTN_LABEL[a.attn]} /></td>
                <td className="agd-muted agd-hide-phone" style={{ fontSize: 13 }}>{a.why}</td></tr>
            ))}
          </tbody></table></div>)}
        <p className="agd-muted" style={{ fontSize: 13, margin: 0 }}>
          {attn.length > 6 && <><a className="agd-link" href="#agents">{attn.length - 6} more</a> · </>}{d.agents.length - attn.length} other agents are ok.</p>
      </section>
      {opened?.approval && <ApprovalDrawer token={token} a={opened.approval} onExpired={onExpired} onClose={() => setOpened(null)}
        onDecided={() => { setOpened(null); d.reload() }} />}
    </>
  )
}
