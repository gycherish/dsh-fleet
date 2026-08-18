/*
 * Landing-page motion. Nothing here is required to read the page: the markup
 * is complete without it. Canvas work is skipped when the visitor asked for
 * less motion, and clipboard is skipped off a secure context (file:// tests).
 */
(() => {
  const reduce = matchMedia('(prefers-reduced-motion: reduce)').matches

  paintSky(document.getElementById('sky'), reduce)
  if (window.isSecureContext) wireCopy()
})()

function cssVar(name) {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim()
}

function paintSky(canvas, reduce) {
  if (!canvas || !canvas.getContext) return
  const ctx = canvas.getContext('2d')
  if (!ctx) return

  const stage = canvas.parentElement
  let width = 0
  let height = 0
  let frame = 0
  let raf = 0
  let t = 0

  const nodes = [
    { ang: -2.4, dist: 0.34, r: 3.6, phase: 0.2 },
    { ang: -1.5, dist: 0.28, r: 3.1, phase: 1.1 },
    { ang: -0.4, dist: 0.38, r: 3.4, phase: 2.0 },
    { ang: 0.6, dist: 0.30, r: 2.8, phase: 0.7 },
    { ang: 1.7, dist: 0.36, r: 3.2, phase: 1.6 },
    { ang: 2.5, dist: 0.26, r: 2.6, phase: 2.4 },
    { ang: 3.3, dist: 0.42, r: 3.0, phase: 0.9 },
  ]
  const packets = nodes.map((_, i) => (i * 0.13) % 1)

  function resize() {
    const box = stage.getBoundingClientRect()
    const dpr = Math.min(window.devicePixelRatio || 1, 2)
    width = Math.max(1, box.width)
    height = Math.max(1, box.height)
    canvas.width = Math.round(width * dpr)
    canvas.height = Math.round(height * dpr)
    canvas.style.width = width + 'px'
    canvas.style.height = height + 'px'
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0)
  }

  function hub() {
    if (width < 720) return { x: width * 0.86, y: height * 0.18 }
    return { x: width * 0.88, y: height * 0.42 }
  }

  function layout(h) {
    const span = Math.min(width, height) * (width < 720 ? 0.55 : 0.72)
    return nodes.map((n) => {
      let x = h.x + Math.cos(n.ang) * n.dist * span * 1.55
      let y = h.y + Math.sin(n.ang) * n.dist * span
      x = Math.min(width - 18, Math.max(18, x))
      y = Math.min(height - 18, Math.max(18, y))
      return { x, y, r: n.r, phase: n.phase }
    })
  }

  function draw(dt) {
    t += dt
    const accent = cssVar('--accent') || '#5fc4cd'
    const up = cssVar('--up') || '#5fbf92'
    const ink = cssVar('--ink') || '#dde5ee'
    const h = hub()
    const pts = layout(h)

    ctx.clearRect(0, 0, width, height)

    ctx.save()
    ctx.translate(h.x, h.y)
    ctx.rotate(t * 0.35)
    ctx.beginPath()
    ctx.moveTo(0, 0)
    ctx.arc(0, 0, Math.min(width, height) * 0.42, 0, 0.7)
    ctx.closePath()
    ctx.fillStyle = accent
    ctx.globalAlpha = 0.13
    ctx.fill()
    ctx.restore()
    ctx.globalAlpha = 1

    ctx.beginPath()
    ctx.arc(h.x, h.y, Math.min(width, height) * 0.22, 0, Math.PI * 2)
    ctx.strokeStyle = accent
    ctx.globalAlpha = 0.22
    ctx.lineWidth = 1.15
    ctx.stroke()
    ctx.beginPath()
    ctx.arc(h.x, h.y, Math.min(width, height) * 0.36, 0, Math.PI * 2)
    ctx.globalAlpha = 0.12
    ctx.stroke()
    ctx.globalAlpha = 1

    for (const p of pts) {
      ctx.beginPath()
      ctx.moveTo(h.x, h.y)
      ctx.lineTo(p.x, p.y)
      ctx.strokeStyle = accent
      ctx.globalAlpha = 0.42
      ctx.lineWidth = 1.2
      ctx.stroke()
    }
    ctx.globalAlpha = 1

    for (let i = 0; i < pts.length; i++) {
      packets[i] = (packets[i] + dt * 0.18) % 1
      const u = packets[i]
      const x = pts[i].x + (h.x - pts[i].x) * u
      const y = pts[i].y + (h.y - pts[i].y) * u
      ctx.beginPath()
      ctx.arc(x, y, 2.1, 0, Math.PI * 2)
      ctx.fillStyle = up
      ctx.globalAlpha = 0.25 + 0.75 * (1 - Math.abs(u - 0.55) * 1.4)
      ctx.fill()
    }
    ctx.globalAlpha = 1

    for (const p of pts) {
      const pulse = 1 + Math.sin(t * 2.2 + p.phase) * 0.18
      ctx.beginPath()
      ctx.arc(p.x, p.y, p.r * 2.6 * pulse, 0, Math.PI * 2)
      ctx.fillStyle = up
      ctx.globalAlpha = 0.22
      ctx.fill()
      ctx.globalAlpha = 1
      ctx.beginPath()
      ctx.arc(p.x, p.y, p.r, 0, Math.PI * 2)
      ctx.fillStyle = up
      ctx.fill()
    }

    ctx.beginPath()
    ctx.arc(h.x, h.y, 5.5, 0, Math.PI * 2)
    ctx.fillStyle = accent
    ctx.shadowColor = accent
    ctx.shadowBlur = 16
    ctx.fill()
    ctx.shadowBlur = 0
    ctx.beginPath()
    ctx.arc(h.x, h.y, 2.2, 0, Math.PI * 2)
    ctx.fillStyle = ink
    ctx.globalAlpha = 0.85
    ctx.fill()
    ctx.globalAlpha = 1
  }

  function loop(now) {
    if (document.hidden) {
      raf = 0
      return
    }
    const dt = Math.min(0.05, (now - (frame || now)) / 1000)
    frame = now
    draw(reduce ? 0 : dt)
    if (!reduce) raf = requestAnimationFrame(loop)
  }

  resize()
  draw(0)
  if (!reduce) raf = requestAnimationFrame(loop)

  const ro = new ResizeObserver(() => {
    resize()
    if (reduce) draw(0)
  })
  ro.observe(stage)

  document.addEventListener('visibilitychange', () => {
    if (document.hidden || reduce) return
    if (!raf) {
      frame = 0
      raf = requestAnimationFrame(loop)
    }
  })
}

function wireCopy() {
  const copied = document.documentElement.lang === 'zh-CN' ? '已复制' : 'Copied'
  const label = document.documentElement.lang === 'zh-CN' ? '复制' : 'Copy'

  for (const pre of document.querySelectorAll('pre')) {
    const btn = document.createElement('button')
    btn.type = 'button'
    btn.className = 'copy'
    btn.textContent = label
    btn.addEventListener('click', async () => {
      const text = (pre.querySelector('code') || pre).innerText.replace(/\s+$/, '')
      try {
        await navigator.clipboard.writeText(text)
        btn.textContent = copied
        setTimeout(() => { btn.textContent = label }, 1400)
      } catch {
        /* leave the block selectable */
      }
    })
    pre.appendChild(btn)
  }
}
