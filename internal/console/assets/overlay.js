/*
 * The fleet control, injected into every page a machine serves.
 *
 * A selected machine owns the origin root, because the dsh client addresses
 * `/api` and its assets absolutely. That left the console with nowhere to put
 * its own chrome: the way back to the chooser was a URL you had to remember,
 * and signing out was impossible without it. This is that chrome — the one
 * piece of the control plane that lives inside someone else's application.
 *
 * Because it lives in someone else's application, it must be movable. No fixed
 * corner is safe across every dsh view and viewport, so the control drags
 * anywhere, snaps to whichever side edge is nearer, and remembers where it was
 * put. It stays inside a shadow root so neither side can restyle the other,
 * loads from the control plane's own origin so a future `script-src 'self'`
 * keeps working, and renders nothing at all if the session has expired.
 */
;(() => {
  'use strict'

  const PREFIX = '/_fleet'
  const TAG = 'dshf-console'
  const PLACE_KEY = 'dshf.console.place'
  // Below this a pointer gesture is a tap, above it a drag. Fingers wobble.
  const DRAG_SLOP = 5
  const MARGIN = 8

  if (document.querySelector(TAG) || window.top !== window.self) return

  /** Pick a theme from the host page rather than the OS.
   *
   * dsh's light/dark is its own setting, so `prefers-color-scheme` would put a
   * dark control on a light app whenever the two disagree.
   */
  function hostIsDark() {
    for (const el of [document.body, document.documentElement]) {
      if (!el) continue
      const rgb = getComputedStyle(el).backgroundColor.match(/[\d.]+/g)
      if (!rgb || rgb.length < 3 || (rgb[3] !== undefined && Number(rgb[3]) === 0)) continue
      const [r, g, b] = rgb.map(Number)
      return (0.2126 * r + 0.7152 * g + 0.0722 * b) < 128
    }
    return window.matchMedia?.('(prefers-color-scheme: dark)').matches ?? false
  }

  const css = `
:host { all: initial; }
* { box-sizing: border-box; font-family: system-ui, -apple-system, "Segoe UI", "PingFang SC", sans-serif; }

.root { position: fixed; z-index: 2147483000; display: flex; flex-direction: column; }
.root.right { align-items: flex-end; }
.root.left { align-items: flex-start; }
.root.up { flex-direction: column-reverse; }

.pill {
  display: inline-flex; align-items: center; gap: .4rem;
  max-width: 60vw; padding: .32rem .6rem .32rem .5rem;
  font-size: .78rem; line-height: 1.4; font-weight: 500;
  color: var(--ink); background: var(--surface);
  border: 1px solid var(--rule);
  box-shadow: 0 1px 2px rgba(0,0,0,.06), 0 4px 14px var(--shadow);
  cursor: grab; opacity: .78;
  transition: opacity .12s, border-color .12s, box-shadow .12s;
  touch-action: none; user-select: none; -webkit-user-select: none;
}
.pill:hover, .pill:focus-visible, .pill[aria-expanded="true"] { opacity: 1; border-color: var(--accent); outline: none; }
.pill .name { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; pointer-events: none; }
.pill svg { width: .6rem; height: .6rem; flex: none; opacity: .5; pointer-events: none; }
.pill .dot { pointer-events: none; }

/* Docked: the outer corners square off against the edge so it reads as parked
   rather than floating, and it takes up less room doing it. */
.root.right .pill { border-radius: 999px 0 0 999px; border-right-width: 0; padding-right: .45rem; }
.root.left .pill  { border-radius: 0 999px 999px 0; border-left-width: 0;  padding-left: .45rem; }
.root.dragging .pill {
  border-radius: 999px; border-width: 1px; cursor: grabbing; opacity: 1;
  box-shadow: 0 6px 22px var(--shadow), 0 2px 5px rgba(0,0,0,.10);
}

.dot { width: .45rem; height: .45rem; border-radius: 50%; flex: none; background: var(--off); }
.dot.online { background: var(--ok); }
.dot.offline { background: var(--off); }
.dot.revoked, .dot.never-seen { background: var(--warn); }

.panel {
  width: min(19rem, calc(100vw - 1rem));
  margin-top: .4rem;
  background: var(--surface); color: var(--ink);
  border: 1px solid var(--rule); border-radius: 12px;
  box-shadow: 0 8px 34px var(--shadow);
  overflow: hidden;
}
.root.up .panel { margin-top: 0; margin-bottom: .4rem; }
.panel[hidden] { display: none; }

.head {
  display: flex; align-items: center; gap: .5rem;
  padding: .6rem .75rem; border-bottom: 1px solid var(--rule);
  font-size: .7rem; text-transform: uppercase; letter-spacing: .07em; color: var(--muted);
}
.head .who { margin-left: auto; text-transform: none; letter-spacing: 0; font-size: .75rem; }

.list { max-height: min(24rem, 50vh); overflow-y: auto; }
.item {
  display: flex; align-items: center; gap: .5rem; width: 100%;
  padding: .55rem .75rem; border: 0; border-bottom: 1px solid var(--rule);
  background: none; color: inherit; font: inherit; font-size: .82rem;
  text-align: left; text-decoration: none; cursor: pointer;
}
.item:last-child { border-bottom: 0; }
.item:hover, .item:focus-visible { background: var(--hover); outline: none; }
.item[aria-current="true"] { background: var(--hover); cursor: default; }
.item .label { font-weight: 500; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.item .meta { margin-left: auto; font-size: .7rem; color: var(--muted); white-space: nowrap; }
.item .here { font-size: .62rem; text-transform: uppercase; letter-spacing: .06em; color: var(--accent); }

.foot { display: flex; align-items: center; gap: .5rem; padding: .55rem .75rem; border-top: 1px solid var(--rule); }
.foot form { margin: 0; margin-left: auto; }
.foot a, .foot button {
  font: inherit; font-size: .76rem; padding: .25rem .5rem; border-radius: 7px;
  border: 1px solid var(--rule); background: var(--surface); color: var(--ink);
  text-decoration: none; cursor: pointer;
}
.foot a:hover, .foot button:hover, .foot a:focus-visible, .foot button:focus-visible { border-color: var(--accent); outline: none; }
.foot .out { color: var(--danger); }
.note { padding: .8rem .75rem; font-size: .78rem; color: var(--muted); }
.warn { padding: .8rem .75rem; font-size: .8rem; line-height: 1.5; color: var(--muted); }
.warn p { margin: 0; }
.warn strong { color: var(--ink); }
.foot .go { margin-left: auto; color: var(--accent); }
.hint { padding: .45rem .75rem; border-top: 1px solid var(--rule); font-size: .68rem; color: var(--muted); }

@media (prefers-reduced-motion: no-preference) {
  .root:not(.dragging) { transition: left .18s ease, top .18s ease; }
}
`

  const THEMES = {
    light: `--surface:#fff;--ink:#16233a;--muted:#64758f;--rule:#dde3ec;--hover:#f2f5f9;
            --accent:#1f5fa8;--ok:#2e7259;--warn:#a96f1c;--off:#94a2b4;--danger:#a4322c;--shadow:rgba(16,32,56,.14)`,
    dark: `--surface:#182231;--ink:#e3eaf4;--muted:#93a3b9;--rule:#2c3b52;--hover:#1f2b3d;
           --accent:#7db0ea;--ok:#66bb96;--warn:#d5a55e;--off:#6b7a8c;--danger:#e08b84;--shadow:rgba(0,0,0,.45)`,
  }

  const host = document.createElement(TAG)
  document.body.appendChild(host)
  const shadow = host.attachShadow({ mode: 'open' })
  const style = document.createElement('style')
  style.textContent = css
  shadow.appendChild(style)

  const root = document.createElement('div')
  root.className = 'root right'
  const palette = THEMES[hostIsDark() ? 'dark' : 'light']

  const pill = document.createElement('button')
  pill.type = 'button'
  pill.className = 'pill'
  pill.setAttribute('aria-expanded', 'false')
  pill.setAttribute('aria-label', 'Machines and account. Drag to move.')
  pill.innerHTML = '<span class="dot"></span><span class="name">dsh-fleet</span>'
    + '<svg viewBox="0 0 10 6" aria-hidden="true"><path d="M1 1l4 4 4-4" fill="none" stroke="currentColor" stroke-width="1.6"/></svg>'

  const panel = document.createElement('div')
  panel.className = 'panel'
  panel.hidden = true
  panel.setAttribute('role', 'dialog')
  panel.setAttribute('aria-label', 'dsh-fleet')

  root.append(pill, panel)
  shadow.appendChild(root)

  // ── placement ──────────────────────────────────────────────────────────────
  //
  // Stored as a fraction of the viewport height rather than pixels, so the
  // control keeps its relative place when a phone rotates or a window resizes.

  let place = { side: 'right', y: 0.02 }
  try {
    const saved = JSON.parse(localStorage.getItem(PLACE_KEY) ?? 'null')
    if (saved && (saved.side === 'left' || saved.side === 'right') && Number.isFinite(saved.y)) place = saved
  } catch { /* a corrupt or blocked store just means the default corner */ }

  const savePlace = () => {
    try { localStorage.setItem(PLACE_KEY, JSON.stringify(place)) } catch { /* private mode */ }
  }

  function inset(name, fallback) {
    const probe = document.createElement('div')
    probe.style.cssText = `position:fixed;visibility:hidden;height:env(safe-area-inset-${name},0px)`
    document.body.appendChild(probe)
    const value = probe.getBoundingClientRect().height
    probe.remove()
    return Number.isFinite(value) ? value : fallback
  }

  /** Put the control where `place` says, clamped inside the viewport. */
  function applyPlace() {
    const box = pill.getBoundingClientRect()
    const height = box.height || 28
    const top = inset('top', 0) + MARGIN
    const bottom = inset('bottom', 0) + MARGIN
    const limit = Math.max(top, window.innerHeight - height - bottom)
    const y = Math.min(limit, Math.max(top, place.y * window.innerHeight))

    root.classList.toggle('right', place.side === 'right')
    root.classList.toggle('left', place.side === 'left')
    // Open upward when the control sits low, so the panel never runs off-screen.
    root.classList.toggle('up', y > window.innerHeight * 0.55)

    root.style.top = `${y}px`
    if (place.side === 'right') {
      root.style.right = `${inset('right', 0)}px`
      root.style.left = 'auto'
    } else {
      root.style.left = `${inset('left', 0)}px`
      root.style.right = 'auto'
    }
  }

  root.setAttribute('style', palette)
  applyPlace()
  window.addEventListener('resize', applyPlace)

  // ── dragging ───────────────────────────────────────────────────────────────

  let drag = null
  // A click always fires after the pointerup that ended a drag. Without this
  // the panel would spring open every time the control was moved.
  let swallowClick = false

  pill.addEventListener('pointerdown', event => {
    if (event.button !== undefined && event.button !== 0) return
    const box = root.getBoundingClientRect()
    drag = {
      id: event.pointerId,
      fromX: event.clientX, fromY: event.clientY,
      offsetX: event.clientX - box.left, offsetY: event.clientY - box.top,
      moved: false,
    }
    pill.setPointerCapture(event.pointerId)
  })

  pill.addEventListener('pointermove', event => {
    if (!drag || event.pointerId !== drag.id) return
    const dx = event.clientX - drag.fromX
    const dy = event.clientY - drag.fromY
    if (!drag.moved && Math.hypot(dx, dy) < DRAG_SLOP) return

    if (!drag.moved) {
      drag.moved = true
      close()
      root.classList.add('dragging')
      root.classList.remove('right', 'left', 'up')
    }
    // Free movement while held; the snap happens on release.
    root.style.left = `${event.clientX - drag.offsetX}px`
    root.style.right = 'auto'
    root.style.top = `${event.clientY - drag.offsetY}px`
  })

  function endDrag(event) {
    if (!drag || event.pointerId !== drag.id) return
    const moved = drag.moved
    drag = null
    if (!moved) return
    swallowClick = true

    const box = root.getBoundingClientRect()
    root.classList.remove('dragging')
    // Snap to whichever side edge is nearer — the control is a bookmark, not a
    // window, and parking it against an edge keeps it out of the application.
    place = {
      side: (box.left + box.width / 2) < window.innerWidth / 2 ? 'left' : 'right',
      y: box.top / window.innerHeight,
    }
    savePlace()
    applyPlace()
  }

  pill.addEventListener('pointerup', endDrag)
  pill.addEventListener('pointercancel', endDrag)

  // ── contents ───────────────────────────────────────────────────────────────

  const esc = s => String(s ?? '').replace(/[&<>"']/g, c => (
    { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]))

  let state = null

  function paintPill() {
    const here = state?.nodes.find(n => n.current)
    pill.querySelector('.dot').className = 'dot ' + (here ? here.status : '')
    pill.querySelector('.name').textContent = here ? here.label : (state ? 'Pick a machine' : 'dsh-fleet')
    applyPlace()
  }

  function paintPanel() {
    if (!state) {
      panel.innerHTML = `<p class="note">Session expired. <a href="${PREFIX}/login">Sign in</a></p>`
      return
    }
    const rows = state.nodes.length ? state.nodes.map(n => n.current
      ? `<div class="item" aria-current="true">
           <span class="dot ${esc(n.status)}"></span>
           <span class="label">${esc(n.label)}</span>
           <span class="here">here</span>
         </div>`
      : `<a class="item" href="${PREFIX}/select/${encodeURIComponent(n.id)}"
            ${n.status === 'online' ? '' : `data-offline="${esc(n.label)}" data-why="${esc(n.status)}"`}>
           <span class="dot ${esc(n.status)}"></span>
           <span class="label">${esc(n.label)}</span>
           <span class="meta">${esc(n.status === 'online' ? 'online' : n.lastSeen)}</span>
         </a>`).join('')
      : '<p class="note">No machines registered yet.</p>'

    panel.innerHTML = `
      <div class="head">Machines<span class="who">${esc(state.user)}</span></div>
      <div class="list">${rows}</div>
      <div class="foot">
        <a href="${PREFIX}/console">All machines</a>
        <form method="post" action="${PREFIX}/logout"><button class="out" type="submit">Sign out</button></form>
      </div>
      <p class="hint">Drag this button anywhere; it parks on the nearest edge.</p>`

    // Switching to a machine that cannot answer would replace the page you are
    // on with an error, so ask first rather than doing it.
    for (const row of panel.querySelectorAll('.item[data-offline]')) {
      row.addEventListener('click', event => {
        event.preventDefault()
        warn(row.dataset.offline, row.dataset.why, row.getAttribute('href'))
      })
    }
  }

  const REASON = {
    'offline': 'It is registered, but nothing is answering for it right now.',
    'never-seen': 'This machine has never connected.',
    'revoked': 'Its token was withdrawn, so it can no longer connect.',
  }

  /** Replace the panel with a confirmation, in place — a machine's own page is
   *  no place to stack a second modal on top of whatever dsh already has open. */
  function warn(label, why, href) {
    panel.innerHTML = `
      <div class="head">Not connected</div>
      <div class="warn">
        <p><strong>${esc(label)}</strong> ${esc(REASON[why] ?? REASON.offline)}
        ${why === 'revoked' ? '' : ' Opening it shows an empty screen until it reconnects.'}</p>
      </div>
      <div class="foot">
        <button class="back" type="button">Back</button>
        ${why === 'revoked' ? '' : `<a class="go" href="${href}">Open anyway</a>`}
      </div>`
    panel.querySelector('.back').addEventListener('click', paintPanel)
  }

  async function refresh() {
    try {
      const response = await fetch(`${PREFIX}/state`, { headers: { accept: 'application/json' } })
      state = response.ok ? await response.json() : null
    } catch {
      state = null
    }
    paintPill()
  }

  function open() {
    pill.setAttribute('aria-expanded', 'true')
    panel.hidden = false
    paintPanel()
    refresh().then(paintPanel)
    document.addEventListener('pointerdown', onOutside, true)
    document.addEventListener('keydown', onKey, true)
  }

  function close() {
    if (panel.hidden) return
    pill.setAttribute('aria-expanded', 'false')
    panel.hidden = true
    document.removeEventListener('pointerdown', onOutside, true)
    document.removeEventListener('keydown', onKey, true)
  }

  // composedPath sees through the shadow boundary; contains() does not.
  const onOutside = event => { if (!event.composedPath().includes(root)) close() }
  const onKey = event => { if (event.key === 'Escape') { close(); pill.focus() } }

  pill.addEventListener('click', () => {
    if (swallowClick) { swallowClick = false; return }
    if (panel.hidden) open()
    else close()
  })

  refresh()
  // A machine that goes offline while the console sits open should say so.
  setInterval(() => { if (!document.hidden) refresh() }, 30_000)
})()
