// Platform page (#344, docs/console-ux.md §7), agentdev-admins only: one health lamp per platform part, the instance
// version and the command to look deeper. Platform parts use the health lamp (ok, degraded, down), not the agent
// lamp: for an agent amber means "the agent", for a platform part it means "degraded".
import { useCallback, useEffect, useState } from 'react'
import { getPlatform, statusOf, Unauthorized, type Health, type Platform } from './api'
import { ago } from './format'
import { Empty, PageHead } from './ui'
import { useData } from './useData'

const NAMES: Record<string, string> = {
  api: 'API', router: 'Webhook router', operator: 'Operator', 'claude-proxy': 'Claude credential proxy', authentik: 'authentik (sign-in)',
  'forge-token': 'Forge credential', 'egress-proxy': 'Egress proxy', 'agent-pool': 'Agent pool',
}
const RANK: Record<Health, number> = { down: 0, degraded: 1, unknown: 2, ok: 3, absent: 4 }

function HealthLamp({ status }: { status: Health }) {
  const cls = status === 'ok' ? 'agd-health-ok' : status === 'degraded' ? 'agd-health-degraded' : status === 'down' ? 'agd-health-down' : 'agd-health-zero'
  const label = status === 'absent' ? 'not installed' : status
  return <span className={`agd-health ${cls}`}><span className="agd-health-dot" aria-hidden="true" /><span className="agd-label">{label}</span></span>
}

function Copy({ text }: { text: string }) {
  const [done, setDone] = useState(false)
  return (
    <button className="agd-btn agd-btn-sm agd-btn-quiet" type="button" aria-label={`Copy the command ${text}`}
      onClick={() => navigator.clipboard?.writeText(text).then(() => { setDone(true); setTimeout(() => setDone(false), 2000) }).catch(() => undefined)}>
      {done ? 'Copied' : 'Copy'}
    </button>
  )
}

export function PlatformPage({ token, onExpired }: { token: string; onExpired: () => void }) {
  const d = useData()
  const [p, setP] = useState<Platform | null>(null)
  const [error, setError] = useState<{ status: number; msg: string } | null>(null)
  const [busy, setBusy] = useState(false)
  const load = useCallback(() => {
    getPlatform(token).then((r) => { setP(r); setError(null) })
      .catch((e) => (e instanceof Unauthorized ? onExpired() : setError({ status: statusOf(e), msg: (e as Error).message })))
      .finally(() => setBusy(false))
  }, [token, onExpired])
  useEffect(() => { load(); const t = setInterval(load, 30000); return () => clearInterval(t) }, [load])

  const head = (
    <PageHead sub="The parts of this agentdev instance. Admins only."
      actions={!(error?.status === 403) && <button className="agd-btn" type="button" disabled={busy} onClick={() => { setBusy(true); load() }}>Check again</button>} />
  )
  if (error?.status === 403) return <>{head}<Empty title="Only agentdev-admins can see the platform">Ask an administrator to add you to the group.</Empty></>
  if (error?.status === 404 && !p) return <>{head}<Empty title="This API has no platform check">Update the API to a version with <code className="agd-code">GET /api/platform</code>.</Empty></>
  const comps = p ? [...p.components].sort((a, b) => RANK[a.status] - RANK[b.status]) : []
  const bad = comps.filter((c) => c.status === 'down' || c.status === 'degraded').length
  return (
    <>
      {head}
      {error && <p className="agd-error" role="alert">Cannot read the platform state: {error.msg}{p ? '. The data below can be old.' : ''}</p>}
      {!p ? (!error && <p className="agd-muted">Loading…</p>) : (
        <>
          <div className="agd-kpis" style={{ marginBottom: 'var(--space-5)' }}>
            <div className={`agd-kpi ${bad ? 'agd-kpi-alert' : ''}`}><span className="agd-kpi-label agd-label">Parts with a problem</span>
              <span className="agd-kpi-value">{bad}<small>/ {comps.length}</small></span><span className="agd-kpi-foot">down or degraded</span></div>
            <div className="agd-kpi"><span className="agd-kpi-label agd-label">Version</span>
              <span className="agd-kpi-value" style={{ fontSize: 24 }}>{p.version || '–'}</span><span className="agd-kpi-foot">image tag of the API</span></div>
            <div className="agd-kpi"><span className="agd-kpi-label agd-label">Checked</span>
              <span className="agd-kpi-value" style={{ fontSize: 24 }}>{ago(Date.parse(p.checked), d.now)}</span><span className="agd-kpi-foot">again every 30 s on this page</span></div>
          </div>
          <div className="agd-table-wrap">
            <table className="agd-table agd-table-2l">
              <thead><tr><th>Part</th><th>Health</th><th>Detail</th><th className="agd-hide-phone">Look deeper</th></tr></thead>
              <tbody>
                {comps.map((c) => (
                  <tr key={c.name}>
                    <td><strong>{NAMES[c.name] ?? c.name}</strong></td>
                    <td className="agd-nowrap"><HealthLamp status={c.status} /></td>
                    <td className="agd-muted">{c.detail}
                      <div className="agd-phone-only" style={{ marginTop: 4 }}><div className="agd-cmd"><code className="agd-code">{c.command}</code><Copy text={c.command} /></div></div></td>
                    <td className="agd-hide-phone"><div className="agd-cmd"><code className="agd-code">{c.command}</code><Copy text={c.command} /></div></td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </>
      )}
    </>
  )
}
