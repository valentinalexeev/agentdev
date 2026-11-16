// Job drawer (#425): one job of an agent, opened by #work?job=<agent>/<job>&jrepo=<owner/name>. The Run tab is the
// run log: what the agent did while the job ran (assistant text, tool calls and results, the final result), read from
// GET /api/jobs/{agent}/{job}/events and followed live with its SSE stream. Every text in it is model output: redacted
// and clipped by the worker and the sidecar, shown as plain text only (React escapes it; no HTML, no markdown, no
// links). Long outputs are folded.
import { useEffect, useRef, useState } from 'react'
import { listRunEvents, Unauthorized, statusOf, type RunEvent } from './api'
import { openStream } from './stream'
import { AgentTag, Drawer, Empty, Icon } from './ui'

const FOLD_AT = 400 // characters; a longer text is folded to its first line

type Tab = 'run' | 'about'

export function JobDrawer({ agent, job, repo, token, onExpired, onClose }: {
  agent: string; job: string; repo: string; token: string; onExpired: () => void; onClose: () => void
}) {
  const [tab, setTab] = useState<Tab>('run')
  return (
    <Drawer title={<>Job <span className="ad-code">{job}</span> of <AgentTag name={agent} /></>} onClose={onClose}>
      <div role="tablist" aria-label={`Job ${job}`} className="ad-tabs" style={{ padding: 0, background: 'none' }}>
        {(['run', 'about'] as Tab[]).map((t) => (
          <button key={t} id={`job-tab-${t}`} role="tab" type="button" aria-selected={tab === t} aria-controls={`job-panel-${t}`}
            tabIndex={tab === t ? 0 : -1} className={`ad-tab ${tab === t ? 'ad-tab-on' : ''}`} onClick={() => setTab(t)}>
            {t === 'run' ? 'Run' : 'About'}
          </button>
        ))}
      </div>
      <div role="tabpanel" id={`job-panel-${tab}`} aria-labelledby={`job-tab-${tab}`} style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
        {tab === 'run'
          ? <RunLog agent={agent} job={job} repo={repo} token={token} onExpired={onExpired} />
          : <p className="ad-muted" style={{ margin: 0, fontSize: 13 }}>
            {repo ? `${repo} · ` : ''}{job}. The run log shows what the agent wrote and which tools it used while the job ran.
            Outputs are clipped to 2 KB and secrets are replaced by [redacted]. The log is kept for 30 days.
          </p>}
      </div>
    </Drawer>
  )
}

function RunLog({ agent, job, repo, token, onExpired }: { agent: string; job: string; repo: string; token: string; onExpired: () => void }) {
  const [events, setEvents] = useState<RunEvent[]>([])
  const [error, setError] = useState('')
  const [loaded, setLoaded] = useState(false)
  const [live, setLive] = useState(false)
  const [follow, setFollow] = useState(true)
  const last = useRef(0)
  const end = useRef<HTMLDivElement>(null)

  useEffect(() => {
    let alive = true
    let stop: (() => void) | undefined
    const add = (evs: RunEvent[]) => {
      const fresh = evs.filter((e) => e.id > last.current)
      if (fresh.length === 0) return
      last.current = fresh[fresh.length - 1].id
      setEvents((l) => [...l, ...fresh])
    }
    ;(async () => {
      try {
        // the stored log first (paged), then the stream from the last id
        for (;;) {
          const r = await listRunEvents(token, agent, job, repo, last.current)
          if (!alive) return
          add(r.events)
          if (!r.next_after) break
        }
        setLoaded(true)
        stop = openStream(token, {
          onOpen: () => setLive(true),
          onMessage: (m) => { if (m.type === 'run-event') add([m.data as RunEvent]) },
          onDown: () => setLive(false),
          onUnauthorized: onExpired,
        }, () => runStreamPath(agent, job, repo, last.current))
      } catch (e) {
        if (!alive) return
        if (e instanceof Unauthorized) onExpired()
        else setError(statusOf(e) === 404 ? 'not-found' : (e as Error).message)
      }
    })()
    return () => { alive = false; stop?.() }
  }, [agent, job, repo, token, onExpired])

  // auto-scroll while the person is at the end; scrolling up stops it
  useEffect(() => { if (follow) end.current?.scrollIntoView({ block: 'end' }) }, [events, follow])

  if (error === 'not-found') return <Empty title="No access">The job does not exist, or you have no write access to its repository.</Empty>
  if (error) return <p className="ad-error" role="alert">Cannot read the run log: {error}</p>
  if (!loaded) return <p className="ad-muted">Loading the run log…</p>
  return (
    <>
      <div className="ad-filters" style={{ gap: 'var(--space-3)', fontSize: 13 }}>
        <span className="ad-muted" role="status">{live ? 'Live' : 'Not live: showing what is stored'} · {events.length} {events.length === 1 ? 'step' : 'steps'}</span>
        <label className="ad-field-inline" style={{ display: 'flex', gap: 'var(--space-2)', alignItems: 'center' }}>
          <input type="checkbox" checked={follow} onChange={(e) => setFollow(e.target.checked)} /> Follow
        </label>
      </div>
      <div className="ad-untrusted-head ad-label"><Icon name="alert" />Written by the agent · not checked</div>
      {events.length === 0
        ? <Empty title="Nothing yet">The run log starts when the agent runs Claude for this job.</Empty>
        : <ol className="ad-runlog" aria-label={`Run log of ${job}`} style={{ listStyle: 'none', margin: 0, padding: 0, display: 'flex', flexDirection: 'column', gap: 'var(--space-2)' }}>
          {events.map((e) => <li key={e.id}><Step e={e} /></li>)}
        </ol>}
      <div ref={end} />
    </>
  )
}

function runStreamPath(agent: string, job: string, repo: string, after: number): string {
  const q = new URLSearchParams()
  if (repo) q.set('repo', repo)
  if (after) q.set('after', String(after))
  return `/api/jobs/${encodeURIComponent(agent)}/${encodeURIComponent(job)}/events/stream?${q}`
}

const time = (s: string) => { const d = new Date(s); return Number.isNaN(d.getTime()) ? '' : d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' }) }
const kb = (n?: number) => n === undefined ? '' : n < 1024 ? `${n} B` : `${(n / 1024).toFixed(1)} KB`

// Text: plain, folded when long. Never HTML: React escapes {text}.
function Folded({ text, label }: { text: string; label: string }) {
  if (text.length <= FOLD_AT && !text.includes('\n')) return <pre className="ad-untrusted">{text}</pre>
  return (
    <details className="ad-tool">
      <summary><Icon name="down" /><span className="ad-code">{text.split('\n')[0].slice(0, 120)}</span><span className="ad-muted">{label}</span></summary>
      <pre className="ad-untrusted">{text}</pre>
    </details>
  )
}

function Step({ e }: { e: RunEvent }) {
  const d = e.data ?? {}
  const t = <span className="ad-muted" style={{ fontSize: 12 }}>{time(e.time)}</span>
  switch (e.kind) {
    case 'text':
      return <div>{t} <span className="ad-label">Agent</span><Folded text={d.text ?? ''} label="show all" /></div>
    case 'tool_use':
      return <div>{t} <span className="ad-label">Tool</span> <span className="ad-code">{d.tool ?? ''}</span><Folded text={d.input ?? ''} label="input" /></div>
    case 'tool_result':
      return (
        <div>{t} <span className="ad-label" style={d.ok === false ? { color: 'var(--danger)' } : undefined}>{d.ok === false ? 'Error' : 'Result'}</span>{' '}
          <span className="ad-muted" style={{ fontSize: 12 }}>{kb(d.size)}</span>
          {d.text ? <Folded text={d.text} label="output" /> : null}
        </div>
      )
    case 'result':
      return <p className="ad-card-note" style={{ margin: 0 }}>{t} Run ended{d.ok === false ? ` with an error${d.subtype ? ` (${d.subtype})` : ''}` : ''}: {d.turns ?? 0} turns, {d.input_tokens ?? 0} input and {d.output_tokens ?? 0} output tokens.</p>
    case 'truncated':
      return <p className="ad-card-note" role="note" style={{ margin: 0 }}>{t} The run log of this job reached its size limit. Later steps are not shown.</p>
    default:
      return null
  }
}
