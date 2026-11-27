// agentdev service worker (#347): Web Push only. No fetch handler, no cache: the console works as before.
// The payload is built by the API from fixed text, agent, repository, request type and expiry (decision 3 on #340);
// it never holds text from the model. Deny and Snooze call the API with the signed, single-use action token of the
// payload; there is no Approve action: a click opens the approval drawer in the console.
/* eslint-env serviceworker */

self.addEventListener('install', () => self.skipWaiting())
self.addEventListener('activate', (e) => e.waitUntil(self.clients.claim()))

const sameOrigin = (u) => {
  try { return new URL(u, self.location.origin).origin === self.location.origin } catch { return false }
}

self.addEventListener('push', (e) => {
  let p = {}
  try { p = e.data ? e.data.json() : {} } catch { p = {} }
  const title = typeof p.title === 'string' && p.title ? p.title.slice(0, 120) : 'agentdev'
  const url = typeof p.url === 'string' && p.url.startsWith('/') && sameOrigin(p.url) ? p.url : '/#today'
  const actions = Array.isArray(p.actions)
    ? p.actions.filter((a) => a && (a.action === 'deny' || a.action === 'snooze') && typeof a.token === 'string')
    : []
  const tokens = Object.fromEntries(actions.map((a) => [a.action, a.token]))
  e.waitUntil(self.registration.showNotification(title, {
    body: typeof p.body === 'string' ? p.body.slice(0, 200) : '',
    tag: typeof p.tag === 'string' ? p.tag.slice(0, 120) : undefined,
    icon: '/icon-192.png',
    badge: '/icon-192.png',
    requireInteraction: p.type === 'approvals',
    data: { url, tokens },
    actions: actions.map((a) => ({ action: a.action, title: a.action === 'deny' ? 'Deny' : String(a.title || 'Snooze').slice(0, 40) })),
  }))
})

async function runAction(action, token) {
  let msg = ''
  try {
    const r = await fetch('/api/push/action', {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ token }), credentials: 'omit',
    })
    if (r.ok) msg = action === 'deny' ? 'Request denied.' : 'Snoozed. You get a reminder later.'
    else {
      let err = ''
      try { err = (await r.json()).error || '' } catch { /* not JSON */ }
      msg = `Not done: ${err || r.status}. Open the console.`
    }
  } catch {
    msg = 'Not done: no connection. Open the console.'
  }
  return self.registration.showNotification('agentdev', { body: msg, tag: 'agentdev-action', icon: '/icon-192.png' })
}

async function openUrl(url) {
  const all = await self.clients.matchAll({ type: 'window', includeUncontrolled: true })
  for (const c of all) {
    if (sameOrigin(c.url) && 'focus' in c) {
      await c.focus()
      if ('navigate' in c) return c.navigate(url)
      return undefined
    }
  }
  return self.clients.openWindow(url)
}

self.addEventListener('notificationclick', (e) => {
  const n = e.notification
  const data = n.data || {}
  n.close()
  if ((e.action === 'deny' || e.action === 'snooze') && data.tokens && data.tokens[e.action]) {
    e.waitUntil(runAction(e.action, data.tokens[e.action]))
    return
  }
  e.waitUntil(openUrl(data.url || '/#today'))
})
