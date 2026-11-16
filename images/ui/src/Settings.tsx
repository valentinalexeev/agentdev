// Settings (#347): notifications for this person. Web Push on this device (opt-in), one switch per type, and mutes
// per project and type. The API keeps the settings; the instance admin turns channels on (notifications.channels).
import { useCallback, useEffect, useState } from 'react'
import {
  addPushSubscription, getNotifySettings, putNotifySettings, removePushSubscription, Unauthorized,
  type NotifyMute, type NotifySettings, type NotifyType,
} from './api'
import { Empty, PageHead } from './ui'

const TYPES: [NotifyType, string, string][] = [
  ['approvals', 'Approvals', 'An agent asks for a tool or a protected change. Deny and Snooze work from the notification.'],
  ['stopped', 'Stopped agents', 'A guardrail paused an agent, or the operator blocked it.'],
  ['usage', 'Usage limits', 'An agent starts no new runs until a usage window resets.'],
]

const pushSupported = () => 'serviceWorker' in navigator && 'PushManager' in window && 'Notification' in window

function keyBytes(b64url: string): Uint8Array<ArrayBuffer> {
  const b = atob(b64url.replace(/-/g, '+').replace(/_/g, '/') + '='.repeat((4 - (b64url.length % 4)) % 4))
  const out = new Uint8Array(new ArrayBuffer(b.length))
  for (let i = 0; i < b.length; i++) out[i] = b.charCodeAt(i)
  return out
}

async function currentSubscription(): Promise<PushSubscription | null> {
  if (!pushSupported()) return null
  const reg = await navigator.serviceWorker.getRegistration('/')
  return reg ? reg.pushManager.getSubscription() : null
}

export function SettingsPage({ token, onExpired }: { token: string; onExpired: () => void }) {
  const [s, setS] = useState<NotifySettings | null>(null)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const [here, setHere] = useState<string | null>(null) // this browser's endpoint, when subscribed
  const fail = useCallback((e: Error) => (e instanceof Unauthorized ? onExpired() : setError(e.message)), [onExpired])

  useEffect(() => {
    getNotifySettings(token).then((r) => { setS(r); setError('') }).catch(fail)
    currentSubscription().then((p) => setHere(p?.endpoint ?? null)).catch(() => setHere(null))
  }, [token, fail])

  const save = (types: Partial<Record<NotifyType, boolean>>, mutes: NotifyMute[]) => {
    setBusy(true)
    putNotifySettings(token, { types, mutes }).then((r) => { setS(r); setError('') }).catch(fail).finally(() => setBusy(false))
  }

  const subscribe = async () => {
    if (!s?.vapidPublicKey) return
    setBusy(true); setError('')
    try {
      if (await Notification.requestPermission() !== 'granted') throw new Error('the browser did not allow notifications')
      // same-origin script: allowed by the CSP (script-src 'self')
      const reg = await navigator.serviceWorker.register('/sw.js', { scope: '/' })
      await navigator.serviceWorker.ready
      let sub = await reg.pushManager.getSubscription()
      if (!sub) sub = await reg.pushManager.subscribe({ userVisibleOnly: true, applicationServerKey: keyBytes(s.vapidPublicKey) })
      await addPushSubscription(token, sub.toJSON())
      setHere(sub.endpoint)
      setS(await getNotifySettings(token))
    } catch (e) {
      fail(e as Error)
    } finally {
      setBusy(false)
    }
  }

  const unsubscribe = async () => {
    setBusy(true); setError('')
    try {
      const sub = await currentSubscription()
      if (sub) {
        await removePushSubscription(token, sub.endpoint)
        await sub.unsubscribe()
      }
      setHere(null)
      setS(await getNotifySettings(token))
    } catch (e) {
      fail(e as Error)
    } finally {
      setBusy(false)
    }
  }

  const head = <PageHead title="Settings" sub="Notifications for you. They contain the agent, the repository, the request type and the expiry, never text from an agent." />
  if (!s) return <>{head}{error ? <p className="agd-error" role="alert">{error}</p> : <p className="agd-muted">Loading…</p>}</>
  const on = s.channels.includes('webpush')
  const muted = (p: string, t: NotifyType | '*') => s.mutes.some((m) => m.project === p && m.type === t)
  const toggleMute = (p: string, t: NotifyType | '*') => {
    const next = muted(p, t) ? s.mutes.filter((m) => !(m.project === p && m.type === t)) : [...s.mutes, { project: p, type: t }]
    save(s.types, next)
  }
  const registered = here !== null && s.subscriptions.includes(here)
  return (
    <>
      {head}
      {error && <p className="agd-error" role="alert">{error}</p>}
      <section className="agd-section">
        <div className="agd-section-head"><h2 className="agd-section-title">Push notifications on this device</h2></div>
        {!on ? <Empty title="Notifications are off on this instance">An instance admin turns them on (notifications.channels: [webpush]).</Empty>
          : !pushSupported() ? <p className="agd-muted" style={{ margin: 0 }}>This browser cannot receive push notifications. On iPhone and iPad, add the console to the home screen first.</p>
            : (
              <div className="agd-form" style={{ alignItems: 'flex-start' }}>
                <p style={{ margin: 0 }}>{registered ? 'This device gets notifications.' : 'This device gets no notifications.'}
                  {' '}<span className="agd-muted">{s.subscriptions.length} {s.subscriptions.length === 1 ? 'device' : 'devices'} in total.</span></p>
                {registered
                  ? <button className="agd-btn" type="button" disabled={busy} onClick={unsubscribe}>Stop on this device</button>
                  : <button className="agd-btn agd-btn-primary" type="button" disabled={busy} onClick={subscribe}>Get notifications on this device</button>}
              </div>
            )}
      </section>
      <section className="agd-section">
        <div className="agd-section-head"><h2 className="agd-section-title">Types</h2></div>
        <div className="agd-form">
          {TYPES.map(([t, label, note]) => (
            <label key={t} style={{ display: 'flex', gap: 'var(--space-2)', alignItems: 'flex-start' }}>
              <input type="checkbox" checked={s.types[t]} disabled={busy}
                onChange={(e) => save({ ...s.types, [t]: e.target.checked }, s.mutes)} />
              <span><strong>{label}</strong><br /><span className="agd-muted" style={{ fontSize: 13 }}>{note}</span></span>
            </label>
          ))}
        </div>
      </section>
      <section className="agd-section">
        <div className="agd-section-head"><h2 className="agd-section-title">Mute a project</h2><span className="agd-muted">A muted project sends nothing on any channel.</span></div>
        {s.projects.length === 0 ? <p className="agd-muted" style={{ margin: 0 }}>You have no projects with agents.</p> : (
          <div className="agd-table-wrap">
            <table className="agd-table">
              <thead><tr><th>Project</th><th>All</th>{TYPES.map(([t, label]) => <th key={t}>{label}</th>)}</tr></thead>
              <tbody>
                {s.projects.map((p) => (
                  <tr key={p}>
                    <td className="agd-code">{p}</td>
                    <td><input type="checkbox" aria-label={`Mute all of ${p}`} checked={muted(p, '*')} disabled={busy} onChange={() => toggleMute(p, '*')} /></td>
                    {TYPES.map(([t, label]) => (
                      <td key={t}><input type="checkbox" aria-label={`Mute ${label} of ${p}`} checked={muted(p, '*') || muted(p, t)}
                        disabled={busy || muted(p, '*')} onChange={() => toggleMute(p, t)} /></td>
                    ))}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>)}
        <p className="agd-muted" style={{ fontSize: 13 }}>A checked box mutes. Snooze in a notification reminds you again after {s.snoozeMinutes} minutes.</p>
      </section>
    </>
  )
}
