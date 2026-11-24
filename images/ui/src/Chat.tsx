// Chat page (#346, docs/console-ux.md §10, #340 decision 4): interactive sessions as a structured chat. The terminal
// (the same tmux session) is the second view, reached with kubectl for now.
//
// Rules:
// - Everything the session writes (assistant text, tool input and output, permission input, drafts) is model output.
//   React escapes it, nothing in it is parsed as HTML, and nothing in it becomes a link, an image or a button. The only
//   buttons come from structured events (permission, draft) built by the console.
// - Assistant text is shown as a safe Markdown subset (#492, react-markdown with the options of markdown.ts): paragraphs, headings, bold, italic, lists,
//   code, block quotes, tables, rules. Raw HTML stays text; a link or an image shows its text and address as text.
//   A per-message toggle shows the exact text. Tool input and output, permission input and drafts stay plain text.
// - A draft becomes an issue only when a person clicks Create issue. The API files it as that person.
// - The event stream is read with a bearer fetch (stream.ts) and reconnects with backoff; it resumes after the last
//   event id it has, so nothing shows twice.
// - A person with read access sees the transcript and the live session, but has no composer and no decision buttons.
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  answerPermission, createDraftIssue, endSession, listSessions, sendMessage, startSession, Unauthorized,
  type ChatEvent, type ChatSession,
} from './api'
import { ago, kTokens, short } from './format'
import { Markdown } from './Markdown.tsx'
import { openStream } from './stream'
import { useData } from './useData'
import { AgentTag, Empty, Icon, PersonTag, Untrusted } from './ui'

const LIST_MS = 15000
const go = (hash: string) => window.location.assign(hash)

const str = (v: unknown): string => (typeof v === 'string' ? v : v === undefined || v === null ? '' : JSON.stringify(v, null, 2))
const live = (s: ChatSession) => !s.endedAt
const canWrite = (s: ChatSession) => s.access === 'write' || s.access === 'admin'

function SessLamp({ s, now }: { s: ChatSession; now: number }) {
  if (!live(s)) return <span className="agd-lamp agd-lamp-stopped"><span className="agd-lamp-dot" /><span className="agd-label">ended</span></span>
  const ends = s.state?.idleEndsAt ? Date.parse(s.state.idleEndsAt) : 0
  if (ends) return <span className="agd-lamp agd-lamp-paused"><span className="agd-lamp-dot" /><span className="agd-label">nobody here, ends in {Math.max(0, Math.round((ends - now) / 60e3))} min</span></span>
  return <span className="agd-lamp agd-lamp-running"><span className="agd-lamp-dot" /><span className="agd-label">live</span></span>
}

// sid: the session to open (#chat?s=<id>); agent and context: a new session from Discuss (#chat?agent=<name>&context=...)
export function ChatPage({ token, onExpired, sid, agent: wantAgent = '', context: wantContext = '' }: {
  token: string; onExpired: () => void; sid: string; agent?: string; context?: string
}) {
  const d = useData()
  const [list, setList] = useState<ChatSession[] | null>(null)
  const [error, setError] = useState('')
  const [listOpen, setListOpen] = useState(!sid)
  const [creating, setCreating] = useState(!!wantAgent)
  const [reload, setReload] = useState(0)
  const fail = useCallback((e: Error) => (e instanceof Unauthorized ? onExpired() : setError(e.message)), [onExpired])

  useEffect(() => {
    let alive = true
    const load = () => listSessions(token).then((l) => { if (alive) { setList(l); setError('') } }).catch(fail)
    load()
    const t = setInterval(load, LIST_MS)
    return () => { alive = false; clearInterval(t) }
  }, [token, fail, reload])

  const shown = (list ?? []).filter((s) => d.inScopeRepo(s.repo))
    .sort((a, b) => Number(live(b)) - Number(live(a)) || b.startedAt.localeCompare(a.startedAt))
  const sel = shown.find((s) => s.id === sid) ?? (list ?? []).find((s) => s.id === sid)
  const open = (id: string) => { setCreating(false); setListOpen(false); go(`#chat?s=${encodeURIComponent(id)}`) }

  return (
    <div className={`agd-chat ${listOpen ? 'agd-chat-list-open' : ''}`}>
      <div className="agd-chat-list">
        <div className="agd-chat-list-head">
          <button className="agd-btn agd-btn-primary" type="button" onClick={() => { setCreating(true); setListOpen(false) }}><Icon name="plus" />New session</button>
        </div>
        {error && <p className="agd-error" role="alert" style={{ padding: '0 12px' }}>{error}</p>}
        <ul aria-label="Sessions">
          {!list && !error && <li className="agd-muted" style={{ padding: 12, fontSize: 13 }}>Loading…</li>}
          {list && shown.length === 0 && <li className="agd-muted" style={{ padding: 12, fontSize: 13 }}>No sessions in the projects shown.</li>}
          {shown.map((s) => (
            <li key={s.id}>
              <button className={`agd-sess ${live(s) ? '' : 'agd-sess-ended'}`} type="button" aria-current={s.id === sid} onClick={() => open(s.id)}>
                <span className="agd-sess-title">{s.agent} · {short(s.repo)}</span>
                <span className="agd-sess-meta"><SessLamp s={s} now={d.now} /></span>
                <span className="agd-sess-meta"><span>{s.startedBy}</span><span>{ago(Date.parse(s.startedAt), d.now)}</span></span>
              </button>
            </li>
          ))}
        </ul>
      </div>
      {creating
        ? <NewSession token={token} onExpired={onExpired} agent={wantAgent} context={wantContext} onList={() => setListOpen(true)}
          onStarted={(s) => { setList((l) => [s, ...(l ?? [])]); open(s.id) }} onCancel={() => { setCreating(false); if (!sid) setListOpen(true) }} />
        : sel
          ? <Thread key={sel.id} token={token} onExpired={onExpired} initial={sel} onList={() => setListOpen(true)} onEnded={() => setReload((n) => n + 1)} />
          : <section className="agd-thread" aria-label="No session selected">
            <div className="agd-thread-body">
              <Empty title={sid ? 'No such session' : 'No session selected'}>
                {sid ? 'The session does not exist, is older than the retention time, or you have no access to its repository.' : 'Select a session in the list, or start a new session.'}
              </Empty>
            </div>
          </section>}
    </div>
  )
}

function NewSession({ token, onExpired, agent, context, onStarted, onCancel, onList }: {
  token: string; onExpired: () => void; agent: string; context: string
  onStarted: (s: ChatSession) => void; onCancel: () => void; onList: () => void
}) {
  const d = useData()
  const agents = d.agents.filter((a) => (a.repos ?? []).length > 0).sort((a, b) => a.name.localeCompare(b.name))
  const [name, setName] = useState(agent || agents[0]?.name || '')
  const [ctx, setCtx] = useState(context)
  const [busy, setBusy] = useState(false)
  const [notice, setNotice] = useState('')
  const pick = agents.find((a) => a.name === name)
  const start = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!name) return
    setBusy(true); setNotice('')
    try { onStarted(await startSession(token, name, ctx.trim())) } catch (err) {
      if (err instanceof Unauthorized) onExpired()
      else setNotice((err as Error).message)
    } finally { setBusy(false) }
  }
  return (
    <section className="agd-thread" aria-labelledby="new-session-title">
      <div className="agd-thread-head">
        <button className="agd-iconbtn agd-chat-back" type="button" aria-label="All sessions" onClick={onList}><Icon name="more" /></button>
        <h1 className="agd-thread-title" id="new-session-title">New session</h1>
      </div>
      <div className="agd-thread-body">
        <form className="agd-form" onSubmit={start}>
          <label className="agd-field"><span className="agd-label">Agent</span>
            <select className="agd-select" value={name} onChange={(e) => setName(e.target.value)} required>
              {agents.length === 0 && <option value="">No agents in the projects shown</option>}
              {agents.map((a) => <option key={a.name} value={a.name}>{a.name} · {(a.repos ?? [])[0]}</option>)}
            </select>
          </label>
          {pick && <p className="agd-muted" style={{ fontSize: 13, margin: 0 }}>
            The session gets a new clone of <span className="agd-code">{(pick.repos ?? [])[0]}</span> and uses the Claude budget of {pick.name}.
            It can read and run code in a sandbox. It cannot push or merge. You need write access to the repository.
          </p>}
          <label className="agd-field"><span className="agd-label">Context (optional)</span>
            <textarea className="agd-input" rows={4} value={ctx} maxLength={8192} onChange={(e) => setCtx(e.target.value)}
              placeholder="An issue, a PR or a question to start with. It goes into the session as fenced, untrusted text." />
          </label>
          {notice && <p className="agd-error" role="alert">{notice}</p>}
          <div className="agd-row-actions">
            <button className="agd-btn" type="button" onClick={onCancel}>Cancel</button>
            <button className="agd-btn agd-btn-primary" type="submit" disabled={busy || !name}>{busy ? 'Starting…' : 'Start session'}</button>
          </div>
        </form>
      </div>
    </section>
  )
}

type StreamState = 'connecting' | 'live' | 'retry'

function Thread({ token, onExpired, initial, onList, onEnded }: {
  token: string; onExpired: () => void; initial: ChatSession; onList: () => void; onEnded: () => void
}) {
  const d = useData()
  const [s, setS] = useState(initial)
  const [events, setEvents] = useState<ChatEvent[]>([])
  const [stream, setStream] = useState<StreamState>('connecting')
  const [retryAt, setRetryAt] = useState(0)
  const [notice, setNotice] = useState('')
  const [text, setText] = useState('')
  const [busy, setBusy] = useState(false)
  const [confirmEnd, setConfirmEnd] = useState(false)
  const last = useRef(0)
  const body = useRef<HTMLDivElement>(null)
  const stick = useRef(true)

  useEffect(() => openStream(token, {
    onOpen: () => setStream('live'),
    onDown: (ms) => { setStream('retry'); setRetryAt(Date.now() + ms) },
    onUnauthorized: onExpired,
    onMessage: (m) => {
      if (m.type === 'chat-event') {
        const e = m.data as ChatEvent
        if (!e || typeof e.id !== 'number' || e.id <= last.current) return
        last.current = e.id
        setEvents((l) => [...l, e])
      } else if (m.type === 'session') {
        setS(m.data as ChatSession)
      }
    },
  }, () => `/api/sessions/${encodeURIComponent(initial.id)}/events?after=${last.current}`), [token, onExpired, initial.id])

  // keep the newest event in view, unless the person scrolled up to read
  useEffect(() => { const b = body.current; if (b && stick.current) b.scrollTop = b.scrollHeight }, [events])
  const onScroll = () => { const b = body.current; if (b) stick.current = b.scrollHeight - b.scrollTop - b.clientHeight < 80 }

  const act = async (f: () => Promise<unknown>): Promise<boolean> => {
    setBusy(true); setNotice('')
    try { await f(); return true } catch (e) {
      if (e instanceof Unauthorized) onExpired()
      else setNotice((e as Error).message)
      return false
    } finally { setBusy(false) }
  }
  const send = async () => {
    const t = text.trim()
    if (!t || busy) return
    if (await act(() => sendMessage(token, s.id, t))) setText('')
  }

  const view = useMemo(() => buildView(events), [events])
  const writer = canWrite(s) && live(s)
  const tokens = s.state?.tokens

  return (
    <section className="agd-thread" aria-labelledby="thread-title">
      <div className="agd-thread-head">
        <button className="agd-iconbtn agd-chat-back" type="button" aria-label="All sessions" onClick={onList}><Icon name="more" /></button>
        <h1 className="agd-thread-title" id="thread-title">{s.agent} · {short(s.repo)}</h1>
        <div className="agd-thread-facts">
          <SessLamp s={s} now={d.now} />
          <span className="agd-code">{s.repo}</span>
          <span>Claude usage counts for <AgentTag name={s.agent} />{tokens ? ` · ${kTokens(tokens.input + tokens.output)} tokens` : ''}</span>
          {live(s) && <span className="agd-chat-here" aria-live="polite">Here: {s.viewers.length ? s.viewers.map((v) => <PersonTag key={v} name={v} />) : 'nobody'}</span>}
          {stream === 'retry' && live(s) && <span className="agd-conn agd-conn-stale" role="status"><span className="agd-conn-dot" />Reconnecting{retryAt > d.now ? ` in ${Math.ceil((retryAt - d.now) / 1000)} s` : ''}</span>}
        </div>
        <div className="agd-thread-actions">
          <details className="agd-tool">
            <summary><Icon name="down" /><span>Terminal</span></summary>
            <p style={{ margin: '0 12px 12px', fontSize: 13 }}>The same session as a terminal (needs <span className="agd-code">kubectl</span>):</p>
            <pre className="agd-code" style={{ margin: '0 12px 12px', whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }}>{`kubectl -n coding-agents get pods -l session=${s.id}\nkubectl -n coding-agents exec -it <pod> -c chat -- tmux attach -t chat`}</pre>
          </details>
          {writer && (confirmEnd
            ? <><button className="agd-btn" type="button" onClick={() => setConfirmEnd(false)}>Back</button>
              <button className="agd-btn agd-btn-danger" type="button" disabled={busy}
                onClick={async () => { if (await act(() => endSession(token, s.id))) { setConfirmEnd(false); onEnded() } }}>End for everyone</button></>
            : <button className="agd-btn" type="button" onClick={() => setConfirmEnd(true)}>End</button>)}
        </div>
      </div>
      <div className="agd-thread-body" ref={body} onScroll={onScroll} role="log" aria-label="Transcript" aria-live="polite">
        {view.items.length === 0 && <p className="agd-muted">{stream === 'connecting' ? 'Loading the transcript…' : 'No events yet.'}</p>}
        {view.items.map((it) => <Item key={it.key} it={it} s={s} token={token} writer={writer} canCreate={canWrite(s)} act={act} busy={busy} />)}
        {live(s) && view.waitingForClaude && <div className="agd-msg"><span className="agd-working"><span className="agd-lamp-dot" style={{ width: 10, height: 10, border: '1.5px solid', borderRadius: 999, boxSizing: 'border-box' }} />{view.ready ? 'Working' : 'Cloning the repository and starting Claude. This takes about 30 seconds.'}</span></div>}
      </div>
      {notice && <p className="agd-error" role="alert" style={{ margin: '0 var(--space-5)' }}>{notice}</p>}
      {!live(s)
        ? <div className="agd-composer"><p className="agd-composer-note" style={{ marginTop: 0, fontSize: 14 }}>
          This session ended{s.endedReason ? ` (${s.endedReason})` : ''}. The transcript is read-only and stays for 30 days.
          {canWrite(s) ? ' You can still create an issue from a draft.' : ''}</p></div>
        : !canWrite(s)
          ? <div className="agd-composer"><p className="agd-composer-note" style={{ marginTop: 0, fontSize: 14 }}>
            You have read access to <span className="agd-code">{s.repo}</span>. You can follow this session, but you cannot send
            messages, answer permission requests or create issues.</p></div>
          : <div className="agd-composer">
            <div className="agd-composer-box">
              <textarea rows={2} value={text} maxLength={65536} aria-label="Message to the session"
                placeholder={`Ask about the code of ${short(s.repo)}, or ask for an issue draft`}
                onChange={(e) => setText(e.target.value)}
                onKeyDown={(e) => { if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) { e.preventDefault(); send() } }} />
              <div className="agd-composer-bar">
                <button className="agd-btn agd-btn-primary" type="button" disabled={busy || !text.trim()} onClick={send}>Send</button>
              </div>
            </div>
            <p className="agd-composer-note agd-hide-phone"><span className="agd-kbd">Enter</span> send · <span className="agd-kbd">Shift Enter</span> new line · The session reads and runs code in a sandbox. It cannot push or merge.</p>
          </div>}
    </section>
  )
}

// ---- the transcript as display items ----

type ViewItem =
  | { key: string; kind: 'note'; text: string; time: string }
  | { key: string; kind: 'user'; who: string; text: string }
  | { key: string; kind: 'assistant'; text: string }
  | { key: string; kind: 'tool'; name: string; input: string; output?: string; error?: boolean }
  | { key: string; kind: 'permission'; id: string; tool: string; input: string; answer?: { behavior: string; by: string; reason: string } }
  | { key: string; kind: 'draft'; id: string; title: string; what: string; acceptance: string; decisions: string; issue?: { number: number; url: string; by: string } }

function buildView(events: ChatEvent[]): { items: ViewItem[]; waitingForClaude: boolean; ready: boolean } {
  const items: ViewItem[] = []
  const tools = new Map<string, Extract<ViewItem, { kind: 'tool' }>>()
  const perms = new Map<string, Extract<ViewItem, { kind: 'permission' }>>()
  const drafts = new Map<string, Extract<ViewItem, { kind: 'draft' }>>()
  let viewers: string[] = []
  let waiting = true
  let ready = false
  for (const e of events) {
    const k = `e${e.id}`
    const dt = e.data ?? {}
    switch (e.type) {
      case 'start': items.push({ key: k, kind: 'note', time: e.time, text: `${e.author ?? 'someone'} started the session` }); break
      case 'ready': ready = true; waiting = false; break
      case 'user': items.push({ key: k, kind: 'user', who: e.author ?? '', text: str(dt.text) }); waiting = true; break
      case 'assistant': items.push({ key: k, kind: 'assistant', text: str(dt.text) }); break
      case 'tool': {
        const t = { key: k, kind: 'tool' as const, name: str(dt.name), input: str(dt.input) }
        tools.set(str(dt.id), t); items.push(t); break
      }
      case 'tool-result': {
        const t = tools.get(str(dt.id))
        if (t) { t.output = str(dt.output); t.error = !!dt.error } else items.push({ key: k, kind: 'tool', name: 'output', input: '', output: str(dt.output), error: !!dt.error })
        break
      }
      case 'turn': waiting = false; break
      case 'permission': {
        const p = { key: k, kind: 'permission' as const, id: str(dt.id), tool: str(dt.tool), input: str(dt.input) }
        perms.set(p.id, p); items.push(p); break
      }
      case 'permission-answer': {
        const p = perms.get(str(dt.id))
        if (p) p.answer = { behavior: str(dt.behavior), by: e.author ?? '', reason: str(dt.reason) }
        break
      }
      case 'draft': {
        const dr = { key: k, kind: 'draft' as const, id: str(dt.id), title: str(dt.title), what: str(dt.what), acceptance: str(dt.acceptance), decisions: str(dt.decisions) }
        drafts.set(dr.id, dr); items.push(dr); break
      }
      case 'issue-created': {
        const dr = drafts.get(str(dt.draft))
        if (dr) dr.issue = { number: Number(dt.number), url: str(dt.url), by: e.author ?? '' }
        items.push({ key: k, kind: 'note', time: e.time, text: `${e.author ?? 'someone'} created issue #${Number(dt.number)}` })
        break
      }
      case 'presence': {
        const now = Array.isArray(dt.viewers) ? dt.viewers.map(str) : []
        const joined = now.filter((v) => !viewers.includes(v)), left = viewers.filter((v) => !now.includes(v))
        if (joined.length) items.push({ key: `${k}j`, kind: 'note', time: e.time, text: `${joined.join(', ')} joined` })
        if (left.length) items.push({ key: `${k}l`, kind: 'note', time: e.time, text: `${left.join(', ')} left` })
        viewers = now
        break
      }
      case 'end': items.push({ key: k, kind: 'note', time: e.time, text: `The session ended: ${str(dt.reason) || 'ended'}` }); waiting = false; break
      default:
    }
  }
  // a pending permission request means Claude waits for a person, not that it works
  if ([...perms.values()].some((p) => !p.answer)) waiting = false
  return { items, waitingForClaude: waiting, ready }
}

function Item({ it, s, token, writer, canCreate, act, busy }: {
  it: ViewItem; s: ChatSession; token: string; writer: boolean; canCreate: boolean; act: (f: () => Promise<unknown>) => Promise<boolean>; busy: boolean
}) {
  switch (it.kind) {
    case 'note': return <p className="agd-muted" style={{ fontSize: 13, margin: 0, textAlign: 'center' }}>{it.text} · {new Date(it.time).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}</p>
    case 'user': return (
      <div className="agd-msg agd-msg-person"><span className="agd-msg-who"><PersonTag name={it.who} /></span>
        <div className="agd-msg-text agd-plain">{it.text}</div></div>
    )
    case 'assistant': return <Assistant text={it.text} agent={s.agent} />
    case 'tool': return (
      <details className="agd-tool">
        <summary><Icon name="down" /><span className="agd-label">{it.name}</span><span className="agd-code">{firstLine(it.input)}</span>
          <span className="agd-muted">{it.output === undefined ? 'running' : it.error ? 'error' : 'done'}</span></summary>
        {it.input && <Untrusted text={it.input} />}
        {it.output !== undefined && <Untrusted text={it.output || '(no output)'} note="Output of the tool." />}
      </details>
    )
    case 'permission': return <Permission it={it} s={s} token={token} writer={writer} act={act} busy={busy} />
    case 'draft': return <Draft it={it} s={s} token={token} canCreate={canCreate} act={act} busy={busy} />
  }
}

function Assistant({ text, agent }: { text: string; agent: string }) {
  const [plain, setPlain] = useState(false)
  return (
    <div className="agd-msg"><span className="agd-msg-who"><AgentTag name={agent} /><span>session · not checked</span>
      <button className="agd-btn agd-btn-quiet" type="button" aria-pressed={plain} onClick={() => setPlain(!plain)}>{plain ? 'Formatted' : 'Plain text'}</button></span>
      {plain ? <div className="agd-msg-text agd-plain">{text}</div> : <Markdown text={text} />}</div>
  )
}

function firstLine(t: string): string {
  const l = t.split('\n').find((x) => x.trim()) ?? ''
  return l.length > 80 ? `${l.slice(0, 80)}…` : l
}

function Permission({ it, s, token, writer, act, busy }: {
  it: Extract<ViewItem, { kind: 'permission' }>; s: ChatSession; token: string; writer: boolean; act: (f: () => Promise<unknown>) => Promise<boolean>; busy: boolean
}) {
  const a = it.answer
  return (
    <section className={`agd-ask ${a ? 'agd-ask-done' : ''}`} aria-label={`Permission request ${it.id}`}>
      <span className="agd-ask-text">
        {a ? <>{a.behavior === 'allow' ? 'Allowed once' : 'Denied'}{a.by ? <> by <PersonTag name={a.by} /></> : ''}{a.reason ? ` (${a.reason})` : ''}: </> : 'The session asks to use '}
        <code className="agd-code">{it.tool}</code>{a ? '' : ' in its sandbox, one time.'}
      </span>
      {!a && writer && <>
        <button className="agd-btn" type="button" disabled={busy} onClick={() => act(() => answerPermission(token, s.id, it.id, 'deny'))}><Icon name="x" />Deny</button>
        <button className="agd-btn agd-btn-primary" type="button" disabled={busy} onClick={() => act(() => answerPermission(token, s.id, it.id, 'allow'))}>Allow once</button>
      </>}
      {!a && !writer && live(s) && <span className="agd-muted" style={{ fontSize: 13 }}>Waits for a person with write access.</span>}
      {it.input && <div style={{ flexBasis: '100%', minWidth: 0 }}><Untrusted text={it.input} note="The input of the tool. Decide on what it does, not on what the text asks." /></div>}
    </section>
  )
}

function Draft({ it, s, token, canCreate, act, busy }: {
  it: Extract<ViewItem, { kind: 'draft' }>; s: ChatSession; token: string; canCreate: boolean; act: (f: () => Promise<unknown>) => Promise<boolean>; busy: boolean
}) {
  const initialBody = `## What\n\n${it.what}\n\n## Acceptance criterion\n\n${it.acceptance}\n${it.decisions.trim() ? `\n## Open decisions\n\n${it.decisions}\n` : ''}`
  const [editing, setEditing] = useState(false)
  const [title, setTitle] = useState(it.title)
  const [bodyText, setBody] = useState(initialBody)
  const [made, setMade] = useState<{ number: number; url: string } | null>(null)
  const issue = it.issue ?? (made ? { ...made, by: '' } : undefined)
  const create = () => act(async () => {
    const edit: { title?: string; body?: string } = {}
    if (editing && title !== it.title) edit.title = title
    if (editing && bodyText !== initialBody) edit.body = bodyText
    setMade(await createDraftIssue(token, s.id, it.id, edit))
    setEditing(false)
  })
  const safeURL = issue && /^https?:\/\//.test(issue.url) ? issue.url : ''
  return (
    <section className={`agd-draft ${issue ? 'agd-draft-done' : ''}`} aria-label={`Issue draft ${it.id}`}>
      <div className="agd-draft-head"><Icon name={issue ? 'approval' : 'alert'} />
        <span className="agd-label">{issue ? `Issue #${issue.number} created${issue.by ? ` by ${issue.by}` : ''}` : 'Issue draft from the session · not checked'}</span></div>
      {editing
        ? <div className="agd-draft-body agd-form">
          <label className="agd-field"><span className="agd-label">Title</span>
            <input className="agd-input" value={title} maxLength={200} onChange={(e) => setTitle(e.target.value.replace(/[\r\n]/g, ' '))} /></label>
          <label className="agd-field"><span className="agd-label">Body</span>
            <textarea className="agd-input" rows={10} value={bodyText} maxLength={60000} onChange={(e) => setBody(e.target.value)} /></label>
        </div>
        : <div className="agd-draft-body">
          <h3 className="agd-draft-title">{it.title}</h3>
          <h4>What</h4><p className="agd-plain">{it.what}</p>
          <h4>Acceptance criterion</h4><p className="agd-plain">{it.acceptance}</p>
          {it.decisions.trim() && <><h4>Open decisions</h4><p className="agd-plain">{it.decisions}</p></>}
        </div>}
      <div className="agd-draft-foot">
        {issue
          ? <>
            <span>To give it to an agent, assign it or add the <span className="agd-code">agent:{s.agent}</span> label.</span>
            {safeURL && <a className="agd-btn" href={safeURL} target="_blank" rel="noopener noreferrer" style={{ marginLeft: 'auto' }}>Open #{issue.number} <Icon name="ext" /></a>}
          </>
          : canCreate
            ? <>
              <button className="agd-btn agd-btn-primary" type="button" disabled={busy || !title.trim()} onClick={create}>Create issue</button>
              <button className="agd-btn" type="button" aria-pressed={editing} onClick={() => setEditing(!editing)}>{editing ? 'Preview' : 'Edit'}</button>
              <span className="agd-muted" style={{ fontSize: 13, marginLeft: 'auto' }}>{s.repo} · you create it, as you</span>
            </>
            : <span className="agd-muted" style={{ fontSize: 13 }}>A person with write access to {s.repo} can create this issue.</span>}
      </div>
    </section>
  )
}
