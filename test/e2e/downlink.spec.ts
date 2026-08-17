import { expect, test } from '@playwright/test'

import { openMachine } from './helpers.ts'

/**
 * The two event downlinks, checked as the browser actually opens them.
 *
 * `/api/events.mux` and `/api/events.host` are WebSocket upgrades, not SSE —
 * a plain GET answers 426 with no fallback. They carry every assistant token
 * and every status change, so a control plane that forwards ordinary requests
 * but drops upgrades produces a UI that loads, renders, and then never
 * updates: the worst kind of broken, because it looks fine.
 */

test('both event downlinks open through the control plane', async ({ page }) => {
  await openMachine(page)

  const result = await page.evaluate(async () => {
    const open = (path: string) => new Promise<string>((resolve) => {
      const url = new URL(path, location.href)
      url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:'
      const ws = new WebSocket(url)
      const done = (outcome: string) => { try { ws.close() } catch { /* already closing */ } resolve(outcome) }
      ws.addEventListener('open', () => done('open'))
      ws.addEventListener('error', () => done('error'))
      ws.addEventListener('close', e => done(`closed ${e.code}`))
      setTimeout(() => done('timeout'), 10_000)
    })
    return {
      mux: await open('/api/events.mux'),
      host: await open('/api/events.host'),
    }
  })

  expect(result.mux, '/api/events.mux must accept a WebSocket upgrade').toBe('open')
  expect(result.host, '/api/events.host must accept a WebSocket upgrade').toBe('open')
})
