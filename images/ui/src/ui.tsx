// Small design-system components (docs/design-system.md, #341). Markup only: every style is in the design-system CSS.
import { useEffect, useRef } from 'react'
import type { Agent, Severity } from './api'
import { lane } from './format'

// Lucide outlines, 24px viewBox, stroke-width 2.2 (design-system.md, "Icons")
const SHIELD = 'M20 13c0 5-3.5 7.5-7.66 8.95a1 1 0 0 1-.67-.01C7.5 20.5 4 18 4 13V6a1 1 0 0 1 1-1c2 0 4.5-1.2 6.24-2.72a1.17 1.17 0 0 1 1.52 0C14.51 3.81 17 5 19 5a1 1 0 0 1 1 1z'
const ICONS = {
  inbox: ['M22 12h-6l-2 3h-4l-2-3H2', 'M5.45 5.11 2 12v6a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2v-6l-3.45-6.89A2 2 0 0 0 16.76 4H7.24a2 2 0 0 0-1.79 1.11z'],
  projects: ['M20 20a2 2 0 0 0 2-2V8a2 2 0 0 0-2-2h-7.9a2 2 0 0 1-1.69-.9L9.6 3.9A2 2 0 0 0 7.93 3H4a2 2 0 0 0-2 2v13a2 2 0 0 0 2 2Z'],
  agents: ['M21 8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16Z', 'm3.3 7 8.7 5 8.7-5', 'M12 22V12'],
  approval: [SHIELD, 'm9 12 2 2 4-4'],
  security: [SHIELD, 'M12 8v4', 'M12 16h.01'],
  usage: ['m12 14 4-4', 'M3.34 19a10 10 0 1 1 17.32 0'],
  alert: ['m21.73 18-8-14a2 2 0 0 0-3.48 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.73-3', 'M12 9v4', 'M12 17h.01'],
  pause: ['M14 4h4v16h-4z', 'M6 4h4v16H6z'],
  play: ['M6 3l14 9-14 9z'],
  x: ['M18 6 6 18', 'm6 6 12 12'],
  moon: ['M12 3a6 6 0 0 0 9 9 9 9 0 1 1-9-9Z'],
  sun: ['M12 8a4 4 0 1 0 0 8 4 4 0 0 0 0-8z', 'M12 2v2', 'M12 20v2', 'm4.93 4.93 1.41 1.41', 'm17.66 17.66 1.41 1.41', 'M2 12h2', 'M20 12h2', 'm6.34 17.66-1.41 1.41', 'm19.07 4.93-1.41 1.41'],
  clock: ['M12 2a10 10 0 1 0 0 20 10 10 0 0 0 0-20z', 'M12 6v6l4 2'],
  plus: ['M5 12h14', 'M12 5v14'],
  blocked: ['M7.86 2h8.28L22 7.86v8.28L16.14 22H7.86L2 16.14V7.86z', 'm15 9-6 6', 'm9 9 6 6'],
  down: ['m6 9 6 6 6-6'],
  more: ['M12 11a1 1 0 1 0 0 2 1 1 0 0 0 0-2z', 'M19 11a1 1 0 1 0 0 2 1 1 0 0 0 0-2z', 'M5 11a1 1 0 1 0 0 2 1 1 0 0 0 0-2z'],
  ext: ['M15 3h6v6', 'M10 14 21 3', 'M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h3'],
} as const
export type IconName = keyof typeof ICONS

export function Icon({ name }: { name: IconName }) {
  return (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={2.2} strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      {ICONS[name].map((d) => <path key={d} d={d} />)}
    </svg>
  )
}

// The mark inline, with the tokens (design-system.md, "The mark")
export function Mark() {
  return (
    <svg viewBox="0 0 32 32" aria-hidden="true">
      <path d="M27 12V5H5v22h22v-7" fill="none" stroke="var(--gate)" strokeWidth="3" strokeLinecap="square" />
      <circle cx="12.5" cy="16" r="4.5" fill="var(--lamp)" stroke="var(--lamp-ink)" strokeWidth="1" />
      <path d="M17 16h14" stroke="var(--lamp-ink)" strokeWidth="3" />
    </svg>
  )
}

export function AgentTag({ name }: { name: string }) {
  return <span className={`ad-agent ad-lane-${lane(name)}`}><span className="ad-agent-mark" /><span className="ad-agent-name">{name}</span></span>
}

export function PersonTag({ name }: { name: string }) {
  return <span className="ad-agent ad-person"><span className="ad-agent-mark" /><span className="ad-agent-name">{name}</span></span>
}

// The lifecycle lamp. The API's `stopped` (zero pods) is the normal idle state: an empty ring, the word "idle".
type Lifecycle = Agent['phase'] | 'usage-paused'
const LIFE: Record<Lifecycle, string> = {
  running: 'running', starting: 'starting', stopped: 'idle', paused: 'paused', blocked: 'blocked', invalid: 'invalid', 'usage-paused': 'usage paused',
}
export function Lamp({ phase }: { phase: Lifecycle }) {
  return <span className={`ad-lamp ad-lamp-${phase}`}><span className="ad-lamp-dot" /><span className="ad-label">{LIFE[phase]}</span></span>
}

export function SevTag({ severity }: { severity: Severity }) {
  return <span className={`ad-tag ad-sev-${severity}`}><span className="ad-label">{severity}</span></span>
}

export function Banner({ kind, icon, children, action }: {
  kind?: 'warn' | 'down' | 'paused'; icon: IconName; children: React.ReactNode; action?: React.ReactNode
}) {
  return (
    <div className={`ad-banner ${kind ? `ad-banner-${kind}` : ''}`} role={kind === 'down' ? 'alert' : 'status'}>
      <Icon name={icon} /><span>{children}</span>{action}
    </div>
  )
}

export function PageHead({ title, sub, actions }: { title: string; sub?: React.ReactNode; actions?: React.ReactNode }) {
  return (
    <div className="ad-page-head">
      <div><h1 className="ad-page-title">{title}</h1>{sub && <p className="ad-page-sub">{sub}</p>}</div>
      {actions && <div className="ad-row-actions">{actions}</div>}
    </div>
  )
}

export function Empty({ title, children }: { title: string; children?: React.ReactNode }) {
  return <div className="ad-empty"><strong>{title}</strong>{children}</div>
}

// One subscription window: amber fill, a tick at the limit; at or over the limit the meter says "paused".
export function Meter({ label, pct, limit, foot }: { label: string; pct: number | null; limit: number; foot: string }) {
  const over = pct !== null && pct >= limit
  return (
    <div className={`ad-meter ${over ? 'ad-meter-over' : ''}`}>
      <div className="ad-meter-head"><span className="ad-label">{label}</span><span className="ad-data">{pct === null ? '–' : `${pct}%`}{over ? ' · paused' : ''}</span></div>
      <div className="ad-meter-track" role="meter" aria-valuenow={pct ?? 0} aria-valuemin={0} aria-valuemax={100} aria-label={label}>
        <div className="ad-meter-fill" style={{ width: `${Math.min(pct ?? 0, 100)}%` }} />
        <div className="ad-meter-limit" style={{ left: `${limit}%` }} />
      </div>
      <div className="ad-meter-foot">{foot}</div>
    </div>
  )
}

// Detail beside the list, as a native <dialog>: focus stays inside, Escape closes it. `gate` when it holds a decision.
export function Drawer({ title, onClose, gate, children, foot }: {
  title: React.ReactNode; onClose: () => void; gate?: boolean; children: React.ReactNode; foot?: React.ReactNode
}) {
  const ref = useRef<HTMLDialogElement>(null)
  useEffect(() => {
    const d = ref.current
    if (d && !d.open) d.showModal()
    return () => d?.close()
  }, [])
  return (
    <dialog ref={ref} className={`ad-drawer ${gate ? 'ad-drawer-gate' : ''}`} aria-labelledby="drawer-title"
      onCancel={(e) => { e.preventDefault(); onClose() }}
      onClick={(e) => { if (e.target === e.currentTarget) onClose() }}>
      <div className="ad-drawer-head">
        <h2 className="ad-drawer-title" id="drawer-title">{title}</h2>
        <button className="ad-iconbtn" type="button" aria-label="Close" onClick={onClose}><Icon name="x" /></button>
      </div>
      <div className="ad-drawer-body">{children}</div>
      {foot && <div className="ad-drawer-foot">{foot}</div>}
    </dialog>
  )
}

// Text the agent wrote: plain text only, never HTML or a link (React escapes it).
export function Untrusted({ text, note }: { text: string; note?: string }) {
  return (
    <div>
      <div className="ad-untrusted-head ad-label"><Icon name="alert" />Written by the agent · not checked</div>
      {note && <p className="ad-muted" style={{ fontSize: 13, margin: '4px 0 8px' }}>{note}</p>}
      <pre className="ad-untrusted">{text}</pre>
    </div>
  )
}

// The second axis of agent state: what the agent waits for. Always a word.
export function Attention({ attn, label }: { attn: string; label: string }) {
  return <span className={`ad-attn ad-attn-${attn}`}>{label}</span>
}

// One dot per agent of a project: round = lifecycle, square = attention (console.css, "Agent dots")
export function Dots({ agents }: { agents: { name: string; life: string; attn: string }[] }) {
  return (
    <span className="ad-dots">
      {agents.map((a) => (
        <i key={a.name} title={`${a.name}: ${a.life === 'stopped' ? 'idle' : a.life}, ${a.attn}`}
          className={a.attn === 'you' ? 'ad-d-you' : a.attn === 'blocked' || a.attn === 'guardrail' ? 'ad-d-alert'
            : a.life === 'paused' || a.life === 'usage-paused' ? 'ad-d-paused' : a.life === 'running' || a.life === 'starting' ? 'ad-d-run' : ''} />
      ))}
    </span>
  )
}
