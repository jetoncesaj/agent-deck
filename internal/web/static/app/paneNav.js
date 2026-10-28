// paneNav.js -- a side pane's goto/open reaching this page.
//
// A tap in a session's side pane (internal/panehost) can ask to show another
// session or open a link. The pane host hands the request to the web server
// that attached this page's tmux client (internal/panenav), which sends this
// page a `pane_nav` message on its terminal socket:
//   { type: 'pane_nav', event: 'goto', target: '<session id>' }
//   { type: 'pane_nav', event: 'open', url: 'https://...' }
// The page, not tmux, changes what it shows: selecting the session moves the
// header, the sidebar, the URL and the terminal's reconnect target together.
// Pure: every effect goes through deps, so the decision is unit-tested.

// safeURL returns url when it is an absolute http(s) URL, else ''.
export function safeURL(url) {
  if (typeof url !== 'string' || url.length === 0 || url.length > 2048) return ''
  if (/[\s\u0000-\u001f\u007f]/.test(url)) return ''
  let u
  try { u = new URL(url) } catch (_e) { return '' }
  if ((u.protocol !== 'http:' && u.protocol !== 'https:') || !u.host) return ''
  return url
}

// knownSession reports whether id is a session in the raw SSE menu items.
export function knownSession(items, id) {
  for (const it of items || []) {
    if (it && it.type === 'session' && it.session && it.session.id === id) return true
  }
  return false
}

// handlePaneNav acts on one pane_nav payload. deps:
//   items       the SSE menu items (sessionsSignal.value)
//   current     the session this terminal shows
//   select(id)  select a session (state.js selectSession)
//   open(url)   open a URL in a new tab; returns false when the browser
//               blocked it (no user gesture on a phone)
//   offer(url)  show the URL as a tappable link instead
// Returns what it did: 'goto', 'same', 'open', 'offer' or 'ignored'.
export function handlePaneNav(payload, deps) {
  if (!payload || payload.type !== 'pane_nav') return 'ignored'
  if (payload.event === 'goto') {
    const id = payload.target
    if (typeof id !== 'string' || !id || !knownSession(deps.items, id)) return 'ignored'
    if (id === deps.current) return 'same'
    deps.select(id)
    return 'goto'
  }
  if (payload.event === 'open') {
    const url = safeURL(payload.url)
    if (!url) return 'ignored'
    if (deps.open(url) === false) {
      deps.offer(url)
      return 'offer'
    }
    return 'open'
  }
  return 'ignored'
}

// openInNewTab is the browser's open: false when a popup blocker stopped it.
export function openInNewTab(url) {
  try {
    // No 'noopener' feature: with it window.open always returns null, and a
    // blocked popup could not be told from an opened one. The opener link
    // is cut by hand instead.
    const w = window.open(url, '_blank')
    if (!w) return false
    try { w.opener = null } catch (_e) { /* cross-origin already */ }
    return true
  } catch (_e) {
    return false
  }
}
