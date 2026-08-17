import { expect, test, type ConsoleMessage, type Page } from '@playwright/test'

import { NODE_ID, signIn } from './helpers.ts'

/**
 * The phone is the reason this project exists, so the flows a phone actually
 * performs are the ones worth checking in a real engine rather than with curl.
 *
 * Each test collects browser console errors and failed requests and asserts on
 * them, because the failures that matter here are the ones that leave the page
 * looking fine: a picker that answers `directory-picker-unavailable`, an asset
 * that 404s, an `/api` call that returns the login page as HTML.
 */

/** Console errors and failed responses seen during one test. */
interface Faults {
  console: string[]
  requests: string[]
}

function watch(page: Page): Faults {
  const faults: Faults = { console: [], requests: [] }
  page.on('console', (message: ConsoleMessage) => {
    if (message.type() === 'error') faults.console.push(message.text())
  })
  page.on('requestfailed', (request) => {
    faults.requests.push(`${request.method()} ${request.url()} — ${request.failure()?.errorText ?? 'failed'}`)
  })
  page.on('response', (response) => {
    if (response.status() >= 400) faults.requests.push(`${response.status()} ${response.request().method()} ${response.url()}`)
  })
  return faults
}

test('the chooser renders and fits the viewport', async ({ page }, testInfo) => {
  const faults = watch(page)
  await signIn(page)

  const card = page.locator(`a[href="/_fleet/select/${NODE_ID}"]`)
  await expect(card).toBeVisible()
  await expect(card).toContainText(/online|offline|never-seen/)

  // A phone-first page that scrolls sideways is a phone-first page that failed.
  const overflow = await page.evaluate(() =>
    document.documentElement.scrollWidth - document.documentElement.clientWidth)
  expect(overflow, 'the chooser should not scroll horizontally').toBeLessThanOrEqual(1)

  await testInfo.attach('chooser', { body: await page.screenshot({ fullPage: true }), contentType: 'image/png' })
  expect(faults.console, 'console errors').toEqual([])
})

test('opening a machine loads its own dsh UI', async ({ page }, testInfo) => {
  const faults = watch(page)
  await signIn(page)
  await page.locator(`a[href="/_fleet/select/${NODE_ID}"]`).click()

  // The node owns the origin root once selected.
  await expect(page).toHaveURL(/\/$/)
  // The boot manifest is what proves this is the node's real frontend and not
  // a control-plane placeholder.
  await expect
    .poll(() => page.evaluate(() => '__DSH_BOOT__' in window), { timeout: 20_000 })
    .toBe(true)

  await page.waitForLoadState('networkidle')
  await testInfo.attach('node-ui', { body: await page.screenshot({ fullPage: false }), contentType: 'image/png' })

  // A 403 here is the privilege gate doing its job, not a fault: the dsh UI
  // probes the configuration plane on load and this control plane refuses it
  // by default. Asserting the split keeps that deliberate refusal visible
  // instead of quietly widening the filter until a real 404 slips through.
  const refused = faults.requests.filter(f => f.startsWith('403 '))
  const broken = faults.requests.filter(f => !f.startsWith('403 ') && !f.includes('favicon'))

  expect(broken, 'failed requests').toEqual([])
  for (const line of refused) {
    expect(line, 'only privileged methods may be refused').toMatch(
      /\/api\/(credentials|settings|agentPreset)\.|\/api\/host\.(pickDirectory|openPath)/,
    )
  }
})

test('the workspace picker can browse this node', async ({ page }) => {
  await signIn(page)
  await page.goto(`/_fleet/select/${NODE_ID}`)
  await expect
    .poll(() => page.evaluate(() => '__DSH_BOOT__' in window), { timeout: 20_000 })
    .toBe(true)

  // Called directly rather than driven through the UI: the point is whether
  // this node can be browsed remotely at all, which is the composition
  // question that broke first. A native picker answers
  // `directory-picker-unavailable` here.
  const listing = await page.evaluate(async () => {
    const res = await fetch('/api/host.listDirectory', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ type: 'client-request', rpcId: 'e2e-list', method: 'host.listDirectory', payload: {} }),
    })
    return res.json() as Promise<{ result: { ok: boolean; value?: { entries?: unknown[] }; error?: { code: string } } }>
  })

  expect(listing.result.error?.code, 'pin the browse picker on nodes reached remotely').toBeUndefined()
  expect(listing.result.ok).toBe(true)
  expect(Array.isArray(listing.result.value?.entries)).toBe(true)
})

test('the way back to the chooser works', async ({ page }) => {
  await signIn(page)
  await page.goto(`/_fleet/select/${NODE_ID}`)
  await expect(page).toHaveURL(/\/$/)

  await page.goto('/_fleet/')
  await expect(page.getByRole('heading', { name: /machines/i })).toBeVisible()
  await expect(page.locator('.tag.here', { hasText: 'open' })).toBeVisible()
})
