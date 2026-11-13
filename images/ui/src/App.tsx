import { useCallback, useEffect, useRef, useState } from 'react'
import type { UserManager, User } from './auth'
import { Approvals } from './Approvals'
import { AgentsPage } from './Agents'
import { DataProvider } from './data'
import { useData, type Conn } from './useData'
import { ago, short } from './format'
import { ProjectsPage } from './Projects'
import { SecurityPage } from './Security'
import { TodayPage } from './Today'
import { UsagePage } from './Usage'
import { Banner, Icon, Mark, type IconName } from './ui'

export function App({ um }: { um: UserManager }) {
  const [user, setUser] = useState<User | null | undefined>(undefined)
  const [error, setError] = useState<string>('')
  const handled = useRef(false)

  const signIn = useCallback(() => {
    um.signinRedirect({ state: window.location.hash }).catch((e) => setError(`cannot reach the sign-in server: ${e.message}`))
  }, [um])
  // ends the authentik session too, then comes back to the UI
  const signOut = useCallback(() => {
    um.signoutRedirect().catch(() => um.removeUser().then(() => setUser(null)))
  }, [um])
  const onExpired = useCallback(() => { um.removeUser().then(() => setUser(null)) }, [um])

  useEffect(() => {
    if (window.location.pathname === '/callback') {
      if (handled.current) return // StrictMode runs effects twice in dev; the code is single-use
      handled.current = true
      um.signinRedirectCallback()
        .then((u) => { const h = typeof u.state === 'string' && u.state.startsWith('#') ? u.state : ''; window.history.replaceState({}, '', `/${h}`); setUser(u) })
        .catch((e) => setError(`login failed: ${e.message}`))
      return
    }
    um.getUser().then((u) => setUser(u && !u.expired ? u : null))
  }, [um])

  if (error) return <SignIn><p className="ad-error" role="alert">{error}</p><button className="ad-btn ad-btn-primary" onClick={() => signIn()}>Try again</button></SignIn>
  if (user === undefined) return <SignIn><p className="ad-muted">Loading…</p></SignIn>
  if (!user) {
    return (
      <SignIn>
        <p>Sign in with your agentdev account.</p>
        <button className="ad-btn ad-btn-primary" onClick={() => signIn()}>Sign in</button>
      </SignIn>
    )
  }
  return <Console user={user} token={user.access_token} onExpired={onExpired} onLogout={signOut} onSignIn={signIn} />
}

function SignIn({ children }: { children: React.ReactNode }) {
  return (
    <main className="ad-main" style={{ maxWidth: 420, margin: '12vh auto' }}>
      <div className="ad-empty ad-form" style={{ alignItems: 'center' }}>
        <span className="ad-brand" style={{ padding: 0 }}><Mark />agentdev</span>
        {children}
      </div>
    </main>
  )
}

// Pages are chosen by the URL hash (#security?agent=x), so a link to one agent's events can be shared.
function usePage(): [string, URLSearchParams] {
  const read = () => {
    const [p, q] = window.location.hash.replace(/^#/, '').split('?')
    return [p || 'today', new URLSearchParams(q ?? '')] as const
  }
  const [state, setState] = useState(read)
  useEffect(() => {
    const on = () => setState(read())
    window.addEventListener('hashchange', on)
    return () => window.removeEventListener('hashchange', on)
  }, [])
  return [state[0], state[1]]
}

// Theme: the system setting, or the person's choice (kept in localStorage, applied in main.tsx before the first paint).
function useTheme(): [boolean, () => void] {
  const sys = () => window.matchMedia('(prefers-color-scheme: dark)').matches
  const [dark, setDark] = useState(() => (document.documentElement.dataset.theme ? document.documentElement.dataset.theme === 'dark' : sys()))
  const toggle = () => {
    const next = dark ? 'light' : 'dark'
    document.documentElement.dataset.theme = next
    try { localStorage.setItem('ad-theme', next) } catch { /* storage blocked */ }
    setDark(!dark)
  }
  return [dark, toggle]
}

const NAV: [string, string, IconName][] = [
  ['today', 'Today', 'inbox'], ['projects', 'Projects', 'projects'], ['agents', 'Agents', 'agents'],
  ['approvals', 'Approvals', 'approval'], ['security', 'Security', 'security'], ['usage', 'Usage', 'usage'],
]

function Console({ user, token, onExpired, onLogout, onSignIn }: {
  user: User; token: string; onExpired: () => void; onLogout: () => void; onSignIn: () => void
}) {
  return (
    <DataProvider token={token} onExpired={onExpired}>
      <Shell user={user} token={token} onExpired={onExpired} onLogout={onLogout} onSignIn={onSignIn} />
    </DataProvider>
  )
}

function scopeLabel(d: ReturnType<typeof useData>): string {
  return d.scope === 'mine' ? 'My projects' : d.scope.startsWith('p:') ? short(d.scope.slice(2)) : 'All projects'
}

function Shell({ user, token, onExpired, onLogout, onSignIn }: {
  user: User; token: string; onExpired: () => void; onLogout: () => void; onSignIn: () => void
}) {
  const [page, params] = usePage()
  const d = useData()
  const [dark, toggleTheme] = useTheme()
  const [picking, setPicking] = useState(false)
  const name = String(user.profile.preferred_username ?? user.profile.email ?? '')
  const title = NAV.find(([id]) => id === page)?.[1] ?? 'Today'
  const count = (id: string) => id === 'today' ? (d.inbox.length > 0 && <span className="ad-count" aria-label={`${d.inbox.length} waiting for you`}>{d.inbox.length}</span>)
    : id === 'approvals' ? (d.pending.length > 0 && <span className="ad-count" aria-label={`${d.pending.length} pending`}>{d.pending.length}</span>)
      : id === 'agents' ? <span className="ad-nav-agentcount">{d.agents.filter((a) => a.life === 'running' || a.life === 'starting').length}/{d.agents.length}</span>
        : id === 'projects' ? <span className="ad-nav-agentcount">{d.projects.length}</span> : null
  // keyboard: P opens the scope picker (not inside a field)
  useEffect(() => {
    const on = (e: KeyboardEvent) => {
      const t = e.target as HTMLElement
      if (e.key.toLowerCase() === 'p' && !e.ctrlKey && !e.metaKey && !e.altKey && !t.closest('input, textarea, select, dialog')) { e.preventDefault(); setPicking(true) }
    }
    window.addEventListener('keydown', on)
    return () => window.removeEventListener('keydown', on)
  }, [])
  return (
    <div className="ad-shell">
      <a className="ad-btn ad-skip" href="#main" onClick={(e) => { e.preventDefault(); document.getElementById('main')?.focus() }}>Skip to content</a>
      <aside className="ad-rail" aria-label="Main">
        <a className="ad-brand" href="#today" aria-label="agentdev, Today"><Mark />agentdev</a>
        <nav className="ad-rail-group">
          {NAV.map(([id, label, icon]) => (
            <a key={id} className="ad-nav" href={`#${id}`} aria-current={page === id ? 'page' : undefined}>
              <Icon name={icon} /><span>{label}</span>{count(id)}
            </a>
          ))}
        </nav>
      </aside>
      <header className="ad-topbar">
        <button className="ad-scope" type="button" aria-haspopup="dialog" onClick={() => setPicking(true)}
          aria-label={`Projects shown: ${scopeLabel(d)}, ${d.projects.length}. Change`}>
          <span>{scopeLabel(d)}</span><span className="ad-data">{d.projects.length}</span><Icon name="down" />
        </button>
        <div className="ad-crumbs ad-hide-phone"><strong>{title}</strong></div>
        <span style={{ marginLeft: 'auto' }} />
        <ConnState />
        <button className="ad-iconbtn" type="button" onClick={toggleTheme} aria-label={dark ? 'Use the light theme' : 'Use the dark theme'}>
          <Icon name={dark ? 'sun' : 'moon'} />
        </button>
        <span className="ad-user"><span className="ad-avatar" aria-hidden="true">{name.slice(0, 2)}</span><span>{name}</span></span>
        <button className="ad-btn ad-btn-quiet" type="button" onClick={onLogout}>Sign out</button>
      </header>
      <ConnBanner onSignIn={onSignIn} />
      <main className="ad-main" id="main" tabIndex={-1}>
        {page === 'approvals' ? <Approvals token={token} onExpired={onExpired} />
          : page === 'usage' ? <UsagePage token={token} onExpired={onExpired} />
            : page === 'security' ? <SecurityPage key={params.get('agent') ?? ''} token={token} onExpired={onExpired} initialAgent={params.get('agent') ?? ''} />
              : page === 'agents' ? <AgentsPage token={token} onExpired={onExpired} />
                : page === 'projects' ? <ProjectsPage />
                  : <TodayPage token={token} onExpired={onExpired} />}
      </main>
      {picking && <ScopePicker onClose={() => setPicking(false)} />}
    </div>
  )
}

// The scope picker: My projects (when the API knows them), All projects, or one project, sorted by what waits.
function ScopePicker({ onClose }: { onClose: () => void }) {
  const d = useData()
  const [q, setQ] = useState('')
  const ref = useRef<HTMLDialogElement>(null)
  useEffect(() => { const el = ref.current; if (el && !el.open) el.showModal(); return () => el?.close() }, [])
  const waits = (p: string) => d.allAgents.filter((a) => a.repo === p && a.attn !== 'ok').length
  const opts: [string, string, string][] = [
    ...(d.mine ? [['mine', 'My projects', `${d.mine.length} projects`] as [string, string, string]] : []),
    ['', 'All projects', `${d.allProjects.length} projects`] as [string, string, string],
    ...[...d.allProjects].sort((a, b) => waits(b) - waits(a) || a.localeCompare(b))
      .map((p) => [`p:${p}`, p, waits(p) ? `${waits(p)} need attention` : ''] as [string, string, string]),
  ].filter(([, l]) => l.toLowerCase().includes(q.toLowerCase()))
  const pick = (v: string) => { d.setScope(v); onClose() }
  return (
    <dialog ref={ref} className="ad-palette" aria-label="Projects shown" onCancel={(e) => { e.preventDefault(); onClose() }}
      onClick={(e) => { if (e.target === e.currentTarget) onClose() }}>
      <input autoFocus placeholder="Find a project" value={q} onChange={(e) => setQ(e.target.value)}
        onKeyDown={(e) => { if (e.key === 'Enter' && opts[0]) pick(opts[0][0]) }} aria-label="Find a project" />
      <ul>
        {opts.map(([v, l, note]) => (
          <li key={v || 'all'} aria-selected={d.scope === v}>
            <button type="button" onClick={() => pick(v)}><span className={v.startsWith('p:') ? 'ad-code' : ''}>{l}</span><span className="ad-muted">{note}</span></button>
          </li>
        ))}
      </ul>
    </dialog>
  )
}

const CONN: Record<Conn, [string, string]> = {
  live: ['', 'Live'], stale: ['ad-conn-stale', 'Updated'], starting: ['ad-conn-waking', 'Starting'],
  down: ['ad-conn-down', 'Unavailable'], offline: ['ad-conn-down', 'Offline'], signedout: ['ad-conn-down', 'Signed out'],
}

function ConnState() {
  const d = useData()
  const [cls, label] = CONN[d.conn]
  const text = d.conn === 'stale' && d.at ? `Updated ${ago(d.at, d.now)}` : label
  return <span className={`ad-conn ${cls}`} role="status" title={d.at ? `Last update ${new Date(d.at).toLocaleTimeString()}` : undefined}>
    <span className="ad-conn-dot" /><span className="ad-hide-phone">{text}</span></span>
}

// Not live: one banner with the cause and one action. The page keeps the last good data (#339).
function ConnBanner({ onSignIn }: { onSignIn: () => void }) {
  const d = useData()
  const at = d.at ? new Date(d.at).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) : ''
  const data = at ? ` The data below is from ${at}.` : ''
  if (d.conn === 'starting') return <Banner kind="warn" icon="clock"><strong>agentdev is starting.</strong> The platform scaled to zero and wakes now. This can take 1 to 2 minutes.{data}</Banner>
  if (d.conn === 'down') return <Banner kind="down" icon="alert" action={<button className="ad-btn" type="button" onClick={d.reload}>Try again</button>}>
    <strong>The API does not answer.</strong>{at ? ` Last good update ${at}.` : ''} Actions can fail until it answers.</Banner>
  if (d.conn === 'offline') return <Banner kind="down" icon="alert"><strong>You are offline.</strong> The console updates again when the network is back.{data}</Banner>
  if (d.conn === 'signedout') return <Banner kind="down" icon="alert" action={<button className="ad-btn ad-btn-primary" type="button" onClick={onSignIn}>Sign in</button>}>
    <strong>Your sign-in expired.</strong> Sign in again to come back to this page.</Banner>
  return null
}
