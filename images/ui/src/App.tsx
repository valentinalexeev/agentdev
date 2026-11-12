import { useCallback, useEffect, useRef, useState } from 'react'
import type { UserManager, User } from './auth'
import { Approvals } from './Approvals'
import { AgentsPage } from './Agents'
import { SecurityPage } from './Security'
import { UsagePage } from './Usage'
import { usePendingCount } from './pending'
import { Icon, Mark, type IconName } from './ui'

export function App({ um }: { um: UserManager }) {
  const [user, setUser] = useState<User | null | undefined>(undefined)
  const [error, setError] = useState<string>('')
  const handled = useRef(false)

  const signIn = useCallback(() => {
    um.signinRedirect().catch((e) => setError(`cannot reach the sign-in server: ${e.message}`))
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
        .then((u) => { window.history.replaceState({}, '', '/'); setUser(u) })
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
  return <Console user={user} token={user.access_token} onExpired={onExpired} onLogout={signOut} />
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
    return [p || 'agents', new URLSearchParams(q ?? '')] as const
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
  ['agents', 'Agents', 'agents'], ['approvals', 'Approvals', 'approval'], ['security', 'Security', 'security'], ['usage', 'Usage', 'usage'],
]

function Console({ user, token, onExpired, onLogout }: { user: User; token: string; onExpired: () => void; onLogout: () => void }) {
  const [page, params] = usePage()
  const pending = usePendingCount(token)
  const [dark, toggleTheme] = useTheme()
  const name = String(user.profile.preferred_username ?? user.profile.email ?? '')
  const title = NAV.find(([id]) => id === page)?.[1] ?? 'Agents'
  return (
    <div className="ad-shell">
      <a className="ad-btn ad-skip" href="#main" onClick={(e) => { e.preventDefault(); document.getElementById('main')?.focus() }}>Skip to content</a>
      <aside className="ad-rail" aria-label="Main">
        <a className="ad-brand" href="#agents" aria-label="agentdev, Agents"><Mark />agentdev</a>
        <nav className="ad-rail-group">
          {NAV.map(([id, label, icon]) => (
            <a key={id} className="ad-nav" href={`#${id}`} aria-current={page === id ? 'page' : undefined}>
              <Icon name={icon} /><span>{label}</span>
              {id === 'approvals' && pending > 0 && <span className="ad-count" aria-label={`${pending} waiting for you`}>{pending}</span>}
            </a>
          ))}
        </nav>
      </aside>
      <header className="ad-topbar">
        <div className="ad-crumbs"><span>agentdev</span><span aria-hidden="true">/</span><strong>{title}</strong></div>
        <span style={{ marginLeft: 'auto' }} />
        <button className="ad-iconbtn" type="button" onClick={toggleTheme} aria-label={dark ? 'Use the light theme' : 'Use the dark theme'}>
          <Icon name={dark ? 'sun' : 'moon'} />
        </button>
        <span className="ad-user"><span className="ad-avatar" aria-hidden="true">{name.slice(0, 2)}</span><span>{name}</span></span>
        <button className="ad-btn ad-btn-quiet" type="button" onClick={onLogout}>Sign out</button>
      </header>
      <main className="ad-main" id="main" tabIndex={-1}>
        {page === 'approvals' ? <Approvals token={token} onExpired={onExpired} />
          : page === 'usage' ? <UsagePage token={token} onExpired={onExpired} />
            : page === 'security' ? <SecurityPage key={params.get('agent') ?? ''} token={token} onExpired={onExpired} initialAgent={params.get('agent') ?? ''} />
              : <AgentsPage token={token} onExpired={onExpired} />}
      </main>
    </div>
  )
}
