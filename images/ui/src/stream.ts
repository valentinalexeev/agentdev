// Live updates (#345): GET /api/stream, server-sent events. EventSource cannot send the bearer token, so the stream is
// read with fetch() and a ReadableStream. The envelope (images/api/stream.go) is
//   event: <type> / id: <n> / data: {"type", "time", "data"}
// The stream reconnects with exponential backoff and jitter. The caller keeps polling while the stream is not open,
// so the console never depends on the stream alone.

interface StreamMsg { type: string; time: string; data: unknown }

interface StreamHandlers {
  onOpen: () => void // the server sent "hello"
  onMessage: (m: StreamMsg) => void
  onDown: (retryInMs: number, failures: number) => void // closed or failed; next try after retryInMs
  onUnauthorized: () => void
}

const MIN_MS = 1000
const MAX_MS = 60000

function backoff(failures: number): number {
  const base = Math.min(MAX_MS, MIN_MS * 2 ** Math.max(0, failures - 1))
  return Math.round(base / 2 + Math.random() * (base / 2)) // jitter: 50 to 100 % of the step
}

// Parses SSE text: returns the complete messages and the rest of the buffer.
function parseSSE(buf: string): [StreamMsg[], string] {
  const out: StreamMsg[] = []
  const parts = buf.replace(/\r\n/g, '\n').split('\n\n')
  const rest = parts.pop() ?? ''
  for (const block of parts) {
    const data = block.split('\n').filter((l) => l.startsWith('data:')).map((l) => l.slice(5).replace(/^ /, '')).join('\n')
    if (!data) continue
    try {
      const m = JSON.parse(data)
      if (m && typeof m.type === 'string') out.push(m)
    } catch { /* not JSON: skip */ }
  }
  return [out, rest]
}

// openStream runs until stop() is called. Heartbeats come every `heartbeatSeconds`; a stream without any message for
// three heartbeats is treated as dead (a proxy can hold a broken connection open).
export function openStream(token: string, h: StreamHandlers): () => void {
  let stopped = false
  let failures = 0
  let ctrl: AbortController | null = null
  let timer: ReturnType<typeof setTimeout> | undefined
  let watchdog: ReturnType<typeof setTimeout> | undefined

  const retry = () => {
    if (stopped) return
    failures += 1
    const ms = backoff(failures)
    h.onDown(ms, failures)
    timer = setTimeout(run, ms)
  }

  const run = async () => {
    if (stopped) return
    ctrl = new AbortController()
    let quietMs = 45000
    const arm = () => { clearTimeout(watchdog); watchdog = setTimeout(() => ctrl?.abort(), quietMs) }
    try {
      const r = await fetch('/api/stream', { headers: { Authorization: `Bearer ${token}`, Accept: 'text/event-stream' }, signal: ctrl.signal, cache: 'no-store' })
      if (r.status === 401) { stopped = true; h.onUnauthorized(); return }
      if (!r.ok || !r.body || !(r.headers.get('content-type') ?? '').includes('text/event-stream')) throw new Error(`stream: ${r.status}`)
      const reader = r.body.pipeThrough(new TextDecoderStream()).getReader()
      let buf = ''
      arm()
      for (;;) {
        const { value, done } = await reader.read()
        if (done || stopped) break
        arm()
        const [msgs, rest] = parseSSE(buf + value)
        buf = rest
        for (const m of msgs) {
          if (m.type === 'hello') {
            failures = 0
            const s = (m.data as { heartbeatSeconds?: number } | null)?.heartbeatSeconds
            if (s && s > 0) quietMs = s * 3000
            arm()
            h.onOpen()
          } else if (m.type === 'bye') {
            failures = 0 // a planned end (token age): reconnect soon, without a long wait
          } else {
            h.onMessage(m)
          }
        }
      }
    } catch { /* aborted or network error: retry below */ }
    clearTimeout(watchdog)
    retry()
  }

  run()
  return () => { stopped = true; clearTimeout(timer); clearTimeout(watchdog); ctrl?.abort() }
}
