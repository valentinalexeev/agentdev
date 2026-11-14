// Project secrets page (#202): add, rotate and remove credentials that agents use. Write-only: the value is sent once
// and never shown again. A repository admin (or the admin group) manages the secrets of the repository.
import { useCallback, useEffect, useState } from 'react'
import {
  listSecrets, createSecret, rotateSecret, deleteSecret, Unauthorized, repoError, nameError,
  secretNameError, upstreamError, envNameError, type ProjectSecret, type NewSecret,
} from './api'
import { Modal } from './App'

const EMPTY: NewSecret = { name: '', scope: 'project', project: '', mode: 'broker', broker: { upstream: '', scheme: 'bearer' }, value: '' }

export function Secrets({ token, onExpired }: { token: string; onExpired: () => void }) {
  const [list, setList] = useState<ProjectSecret[] | null>(null)
  const [error, setError] = useState('')
  const [adding, setAdding] = useState(false)
  const [rotating, setRotating] = useState<ProjectSecret | null>(null)
  const [removing, setRemoving] = useState<ProjectSecret | null>(null)
  const fail = useCallback((e: Error) => (e instanceof Unauthorized ? onExpired() : setError(e.message)), [onExpired])
  const load = useCallback(() => {
    listSecrets(token).then((l) => { setList(l); setError('') }).catch(fail)
  }, [token, fail])
  useEffect(() => {
    load()
    const t = setInterval(load, 15000)
    return () => clearInterval(t)
  }, [load])

  return (
    <>
      <h1>Project secrets</h1>
      <p className="muted">
        Credentials that the agents of a project, or one agent, use. In mode <strong>broker</strong> the agent does
        not get the value: the credential broker adds it to requests for the upstream. You cannot read a value
        after you save it. You can only replace it or remove the secret.
      </p>
      <div className="buttons"><button onClick={() => setAdding(true)}>Add a secret</button></div>
      {error && <p className="error">{error}</p>}
      {!list && !error && <p>Loading…</p>}
      {list && list.length === 0 && <p>No project secrets that you manage.</p>}
      {list && list.length > 0 && (
        <table>
          <thead><tr><th>Name</th><th>Scope</th><th>Mode</th><th>State</th><th></th></tr></thead>
          <tbody>
            {list.map((s) => (
              <tr key={s.name}>
                <td><code>{s.name}</code>{s.description && <div className="muted">{s.description}</div>}</td>
                <td>{s.scope === 'project' ? `project ${s.project}` : `agent ${s.agent}`}</td>
                <td>
                  {s.mode === 'broker'
                    ? <>broker: <code>{s.upstream}</code> ({s.header} {s.scheme})</>
                    : <span className="warning">env: <code>{s.envName}</code>, the agent can read it</span>}
                </td>
                <td title={s.message}>{s.phase}{s.syncedAt ? `, ${new Date(s.syncedAt).toLocaleString()}` : ''}</td>
                <td className="actions">
                  <button className="secondary" aria-label={`Replace the value of ${s.name}`} onClick={() => setRotating(s)}>Replace value</button>
                  <button className="danger" aria-label={`Remove ${s.name}`} onClick={() => setRemoving(s)}>Remove</button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {adding && <AddSecret token={token} onExpired={onExpired} onClose={() => setAdding(false)} onDone={() => { setAdding(false); load() }} />}
      {rotating && <Rotate token={token} s={rotating} onExpired={onExpired} onClose={() => setRotating(null)} onDone={() => { setRotating(null); load() }} />}
      {removing && <Remove token={token} s={removing} onExpired={onExpired} onClose={() => setRemoving(null)} onDone={() => { setRemoving(null); load() }} />}
    </>
  )
}

function useAction(onExpired: () => void, onDone: () => void) {
  const [busy, setBusy] = useState(false)
  const [notice, setNotice] = useState('')
  const run = async (f: () => Promise<void>) => {
    setBusy(true); setNotice('')
    try { await f(); onDone() } catch (e) {
      if (e instanceof Unauthorized) onExpired()
      else setNotice((e as Error).message)
    } finally { setBusy(false) }
  }
  return { busy, notice, run }
}

function AddSecret({ token, onExpired, onClose, onDone }: { token: string; onExpired: () => void; onClose: () => void; onDone: () => void }) {
  const [s, setS] = useState<NewSecret>(EMPTY)
  const { busy, notice, run } = useAction(onExpired, onDone)
  const errs = [
    secretNameError(s.name),
    s.scope === 'project' ? (s.project ? repoError(s.project) : 'repository: owner/name') : nameError(s.agent ?? ''),
    s.mode === 'broker' ? upstreamError(s.broker?.upstream ?? '') : envNameError(s.env?.name ?? ''),
    s.mode === 'env' && !s.env?.riskAccepted ? 'mode env: accept the risk' : '',
    s.value.trim() === '' ? 'value: required' : '',
  ].filter(Boolean)
  const set = (p: Partial<NewSecret>) => setS({ ...s, ...p })
  const save = () => {
    const body: NewSecret = { ...s }
    if (s.scope === 'project') delete body.agent; else delete body.project
    if (s.mode === 'broker') delete body.env; else delete body.broker
    if (!body.description) delete body.description
    run(() => createSecret(token, body))
  }
  return (
    <Modal title="Add a project secret" onClose={onClose}>
      <form onSubmit={(e) => { e.preventDefault(); if (!errs.length) save() }}>
        <label>Name <input value={s.name} onChange={(e) => set({ name: e.target.value })} autoComplete="off" /></label>
        <label>Scope{' '}
          <select value={s.scope} onChange={(e) => set({ scope: e.target.value as NewSecret['scope'] })}>
            <option value="project">all agents of a repository</option>
            <option value="agent">one agent</option>
          </select>
        </label>
        {s.scope === 'project'
          ? <>
              <label>Repository <input value={s.project ?? ''} placeholder="owner/name" onChange={(e) => set({ project: e.target.value })} /></label>
              <label>Forge{' '}
                <select value={s.forge ?? 'gitea'} onChange={(e) => set({ forge: e.target.value as NewSecret['forge'] })}>
                  <option value="gitea">Gitea</option><option value="github">GitHub</option>
                </select>
              </label>
            </>
          : <label>Agent <input value={s.agent ?? ''} onChange={(e) => set({ agent: e.target.value })} /></label>}
        <label>Mode{' '}
          <select value={s.mode} onChange={(e) => set(e.target.value === 'env'
            ? { mode: 'env', env: { name: '', riskAccepted: false } }
            : { mode: 'broker', broker: { upstream: '', scheme: 'bearer' } })}>
            <option value="broker">broker: the agent does not get the value</option>
            <option value="env">env: the agent can read the value</option>
          </select>
        </label>
        {s.mode === 'broker'
          ? <>
              <label>Upstream <input value={s.broker?.upstream ?? ''} placeholder="https://npm.pkg.github.com" onChange={(e) => set({ broker: { ...s.broker!, upstream: e.target.value } })} /></label>
              <label>Header <input value={s.broker?.header ?? ''} placeholder="Authorization" onChange={(e) => set({ broker: { ...s.broker!, header: e.target.value || undefined } })} /></label>
              <label>Form{' '}
                <select value={s.broker?.scheme ?? 'bearer'} onChange={(e) => set({ broker: { ...s.broker!, scheme: e.target.value } })}>
                  <option value="bearer">Bearer value</option><option value="basic">Basic value (user:password in base64)</option><option value="raw">value as is</option>
                </select>
              </label>
            </>
          : <>
              <label>Variable <input value={s.env?.name ?? ''} placeholder="DB_PASSWORD" onChange={(e) => set({ env: { ...s.env!, name: e.target.value } })} /></label>
              <p className="warning">
                In mode env the model can read the value. It can write the value into a comment, a commit or a request.
                Use env only if the value has no other path, for example the password of a test database.
              </p>
              <label><input type="checkbox" checked={s.env?.riskAccepted ?? false} onChange={(e) => set({ env: { ...s.env!, riskAccepted: e.target.checked } })} /> I accept this risk</label>
            </>}
        <label>Description <input value={s.description ?? ''} maxLength={200} onChange={(e) => set({ description: e.target.value })} /></label>
        <label>Value <input type="password" value={s.value} autoComplete="new-password" onChange={(e) => set({ value: e.target.value })} /></label>
        {errs.length > 0 && <p className="muted">{errs[0]}</p>}
        <div className="buttons">
          <button type="button" className="secondary" onClick={onClose}>Cancel</button>
          <button type="submit" disabled={busy || errs.length > 0}>Save</button>
        </div>
        {notice && <p className="error" role="alert">{notice}</p>}
      </form>
    </Modal>
  )
}

function Rotate({ token, s, onExpired, onClose, onDone }: { token: string; s: ProjectSecret; onExpired: () => void; onClose: () => void; onDone: () => void }) {
  const [value, setValue] = useState('')
  const { busy, notice, run } = useAction(onExpired, onDone)
  return (
    <Modal title={`Replace the value of ${s.name}`} onClose={onClose}>
      <form onSubmit={(e) => { e.preventDefault(); if (value.trim()) run(() => rotateSecret(token, s.name, value)) }}>
        <p>The agents use the new value within one minute. The old value is gone.</p>
        <label>New value <input type="password" value={value} autoComplete="new-password" onChange={(e) => setValue(e.target.value)} /></label>
        <div className="buttons">
          <button type="button" className="secondary" onClick={onClose}>Cancel</button>
          <button type="submit" disabled={busy || !value.trim()}>Replace</button>
        </div>
        {notice && <p className="error" role="alert">{notice}</p>}
      </form>
    </Modal>
  )
}

function Remove({ token, s, onExpired, onClose, onDone }: { token: string; s: ProjectSecret; onExpired: () => void; onClose: () => void; onDone: () => void }) {
  const { busy, notice, run } = useAction(onExpired, onDone)
  return (
    <Modal title={`Remove ${s.name}?`} onClose={onClose}>
      <p>The agents can no longer use this secret. The value is deleted. You cannot undo this.</p>
      <div className="buttons">
        <button className="secondary" onClick={onClose}>Cancel</button>
        <button className="danger" disabled={busy} onClick={() => run(() => deleteSecret(token, s.name))}>Remove</button>
      </div>
      {notice && <p className="error" role="alert">{notice}</p>}
    </Modal>
  )
}
