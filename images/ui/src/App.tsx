import { useCallback, useEffect, useState } from 'react'
import type { UserManager, User } from './auth'
import { listAgents, Unauthorized, type Agent } from './api'

export function App({ um }: { um: UserManager }) {
  const [user, setUser] = useState<User | null | undefined>(undefined)
  const [error, setError] = useState<string>('')

  useEffect(() => {
    if (window.location.pathname === '/callback') {
      um.signinRedirectCallback()
        .then((u) => { window.history.replaceState({}, '', '/'); setUser(u) })
        .catch((e) => setError(`login failed: ${e.message}`))
      return
    }
    um.getUser().then((u) => setUser(u && !u.expired ? u : null))
  }, [um])

  if (error) return <Shell><p className="error">{error}</p><button onClick={() => um.signinRedirect()}>Try again</button></Shell>
  if (user === undefined) return <Shell><p>Loading…</p></Shell>
  if (!user) {
    return (
      <Shell>
        <p>Sign in with your agentdev account.</p>
        <button onClick={() => um.signinRedirect()}>Sign in</button>
      </Shell>
    )
  }
  return (
    <Shell user={user} onLogout={() => um.removeUser().then(() => setUser(null))}>
      <Overview token={user.access_token} onExpired={() => um.removeUser().then(() => setUser(null))} />
    </Shell>
  )
}

function Shell({ user, onLogout, children }: { user?: User; onLogout?: () => void; children: React.ReactNode }) {
  return (
    <>
      <header>
        <strong>agentdev</strong>
        {user && (
          <span className="who">
            {user.profile.preferred_username ?? user.profile.email}
            <button className="link" onClick={onLogout}>Sign out</button>
          </span>
        )}
      </header>
      <main>{children}</main>
    </>
  )
}

function Overview({ token, onExpired }: { token: string; onExpired: () => void }) {
  const [agents, setAgents] = useState<Agent[] | null>(null)
  const [error, setError] = useState('')

  const load = useCallback(() => {
    listAgents(token)
      .then((a) => { setAgents(a); setError('') })
      .catch((e) => (e instanceof Unauthorized ? onExpired() : setError(e.message)))
  }, [token, onExpired])

  useEffect(() => {
    load()
    const t = setInterval(load, 15000) // read-only overview: a light poll is enough until the API has events
    return () => clearInterval(t)
  }, [load])

  if (error) return <p className="error">{error}</p>
  if (!agents) return <p>Loading agents…</p>
  if (agents.length === 0) return <p>No agents yet. Connect a project with <code>scripts/bootstrap-project.sh</code>.</p>
  return (
    <>
      <h1>Agents</h1>
      <table>
        <thead><tr><th>Agent</th><th>Phase</th><th>Repositories</th><th>Forge</th></tr></thead>
        <tbody>
          {agents.map((a) => (
            <tr key={a.name}>
              <td>{a.name}</td>
              <td><span className={`phase ${a.phase}`}>{a.phase}</span></td>
              <td>{(a.repos ?? []).join(', ')}</td>
              <td>{a.forge ?? ''}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </>
  )
}
