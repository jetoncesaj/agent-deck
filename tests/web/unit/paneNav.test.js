// unit/paneNav.test.js -- a side pane's goto/open reaching the page.
//
// The web server sends a terminal page a `pane_nav` message when a tap in a
// side pane asks to show another session or open a link. These pin the pure
// decision in paneNav.js; TerminalPanel wires it to selectSession and
// window.open.

import { describe, it, expect } from 'vitest'

const APP = '../../../internal/web/static/app/'
const nav = () => import(APP + 'paneNav.js')

const items = [
  { type: 'group', group: { path: 'g' } },
  { type: 'session', session: { id: 'a', status: 'running' } },
  { type: 'session', session: { id: 'b', status: 'waiting' } },
]

function recorder(openResult = true) {
  const calls = { select: [], open: [], offer: [] }
  return {
    calls,
    deps: {
      items,
      current: 'a',
      select: (id) => calls.select.push(id),
      open: (url) => { calls.open.push(url); return openResult },
      offer: (url) => calls.offer.push(url),
    },
  }
}

describe('paneNav: goto', () => {
  it('selects a known session', async () => {
    const { handlePaneNav } = await nav()
    const r = recorder()
    expect(handlePaneNav({ type: 'pane_nav', event: 'goto', target: 'b' }, r.deps)).toBe('goto')
    expect(r.calls.select).toEqual(['b'])
  })

  it('does nothing for the session already shown', async () => {
    const { handlePaneNav } = await nav()
    const r = recorder()
    expect(handlePaneNav({ type: 'pane_nav', event: 'goto', target: 'a' }, r.deps)).toBe('same')
    expect(r.calls.select).toEqual([])
  })

  it('ignores a session the page does not know', async () => {
    const { handlePaneNav } = await nav()
    const r = recorder()
    expect(handlePaneNav({ type: 'pane_nav', event: 'goto', target: 'zzz' }, r.deps)).toBe('ignored')
    expect(handlePaneNav({ type: 'pane_nav', event: 'goto' }, r.deps)).toBe('ignored')
    expect(r.calls.select).toEqual([])
  })
})

describe('paneNav: open', () => {
  it('opens an http(s) URL in a new tab', async () => {
    const { handlePaneNav } = await nav()
    const r = recorder(true)
    expect(handlePaneNav({ type: 'pane_nav', event: 'open', url: 'https://example.com/x' }, r.deps)).toBe('open')
    expect(r.calls.open).toEqual(['https://example.com/x'])
    expect(r.calls.offer).toEqual([])
  })

  it('offers the link when the popup was blocked', async () => {
    const { handlePaneNav } = await nav()
    const r = recorder(false)
    expect(handlePaneNav({ type: 'pane_nav', event: 'open', url: 'http://plane.home/i/1' }, r.deps)).toBe('offer')
    expect(r.calls.offer).toEqual(['http://plane.home/i/1'])
  })

  it('never opens anything but http(s)', async () => {
    const { handlePaneNav } = await nav()
    const r = recorder()
    for (const url of ['javascript:alert(1)', 'file:///etc/passwd', 'data:text/html,x', 'https://a b', '', undefined, 'example.com']) {
      expect(handlePaneNav({ type: 'pane_nav', event: 'open', url }, r.deps)).toBe('ignored')
    }
    expect(r.calls.open).toEqual([])
    expect(r.calls.offer).toEqual([])
  })
})

describe('paneNav: other messages', () => {
  it('ignores other types and events', async () => {
    const { handlePaneNav } = await nav()
    const r = recorder()
    expect(handlePaneNav({ type: 'status', event: 'goto', target: 'b' }, r.deps)).toBe('ignored')
    expect(handlePaneNav({ type: 'pane_nav', event: 'exec', target: 'b' }, r.deps)).toBe('ignored')
    expect(handlePaneNav(null, r.deps)).toBe('ignored')
    expect(r.calls.select).toEqual([])
  })
})
