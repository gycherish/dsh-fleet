import { expect, test, type Page } from '@playwright/test'

import { openMachine } from './helpers.ts'

/**
 * The fleet control injected into a machine's own pages.
 *
 * A selected machine owns the origin root, so before this existed the console
 * had nowhere to put its chrome: once you opened a machine there was no way
 * back to the chooser and no way to sign out, on a phone least of all. These
 * check the two escapes actually work, because they are the ones that get
 * quietly broken by a change to the proxy's response path.
 */

/** Room for WebKit, which runs this flow a few times slower than Chromium. */
const SLOW = 30_000

/** The overlay lives in a shadow root, which CSS selectors do not pierce. */
function pill(page: Page) {
  return page.locator('dshf-console').first()
}

async function openPanel(page: Page): Promise<void> {
  await expect(pill(page)).toBeAttached({ timeout: SLOW })
  await pill(page).evaluate((host: Element) => {
    (host.shadowRoot?.querySelector('.pill') as HTMLElement | null)?.click()
  })
}

test('a machine page carries the fleet control', async ({ page }) => {
  await openMachine(page)

  await expect(pill(page), 'the overlay must be injected into the machine page').toBeAttached({ timeout: SLOW })

  // Collapsed, it answers "which machine am I driving" — the question that had
  // no answer at all once the machine took over the address bar.
  const label = await pill(page).evaluate((host: Element) =>
    host.shadowRoot?.querySelector('.pill .name')?.textContent?.trim() ?? '')
  expect(label, 'the pill names the current machine').not.toBe('')
  expect(label).not.toBe('dsh-fleet')
})

test('the way back to the chooser is one tap', async ({ page }) => {
  await openMachine(page)
  await openPanel(page)

  const href = await pill(page).evaluate((host: Element) =>
    host.shadowRoot?.querySelector<HTMLAnchorElement>('.foot a')?.getAttribute('href') ?? '')
  expect(href).toBe('/_fleet/console')

  await page.goto(href)
  await expect(page.getByRole('heading', { name: /your machines/i })).toBeVisible()
})

test('signing out from inside a machine ends the session', async ({ page }) => {
  await openMachine(page)
  await openPanel(page)

  await pill(page).evaluate((host: Element) => {
    (host.shadowRoot?.querySelector('.foot form') as HTMLFormElement | null)?.submit()
  })
  await page.waitForURL(/_fleet\/login/, { timeout: SLOW })

  // And the session is really gone, not just navigated away from.
  await page.goto('/_fleet/console')
  await expect(page.getByRole('button', { name: /sign in/i })).toBeVisible()
})

test('the control can be dragged to the other edge and stays there', async ({ page }) => {
  await openMachine(page)
  await expect(pill(page)).toBeAttached({ timeout: SLOW })

  const box = async () => pill(page).evaluate((host: Element) =>
    (host.shadowRoot?.querySelector('.root') as HTMLElement).getBoundingClientRect().left)

  const started = await box()
  const size = page.viewportSize()!

  // Drag from wherever it is to the opposite side, low down.
  const from = await pill(page).evaluate((host: Element) => {
    const r = (host.shadowRoot?.querySelector('.pill') as HTMLElement).getBoundingClientRect()
    return { x: r.left + r.width / 2, y: r.top + r.height / 2 }
  })
  await page.mouse.move(from.x, from.y)
  await page.mouse.down()
  await page.mouse.move(24, size.height * 0.6, { steps: 12 })
  await page.mouse.up()
  await page.waitForTimeout(600)

  const moved = await box()
  expect(moved, 'the control follows the drag to the left edge').toBeLessThan(started)
  expect(moved, 'and snaps flush to that edge rather than staying mid-screen').toBeLessThan(size.width * 0.25)

  // A drag must not be mistaken for a tap.
  const opened = await pill(page).evaluate((host: Element) =>
    host.shadowRoot?.querySelector('.panel')?.hasAttribute('hidden') === false)
  expect(opened, 'dragging must not open the panel').toBe(false)

  // And the position survives a reload.
  await page.reload()
  await page.waitForLoadState('networkidle')
  await expect(pill(page)).toBeAttached({ timeout: SLOW })
  expect(await box(), 'the control is remembered where it was parked').toBeLessThan(size.width * 0.25)
})

test('the panel lists every machine and marks the current one', async ({ page }) => {
  await openMachine(page)
  await openPanel(page)

  const rows = await pill(page).evaluate((host: Element) =>
    [...(host.shadowRoot?.querySelectorAll('.list .item') ?? [])].map(row => ({
      label: row.querySelector('.label')?.textContent?.trim() ?? '',
      current: row.getAttribute('aria-current') === 'true',
    })))

  expect(rows.length, 'the panel lists the registered machines').toBeGreaterThan(0)
  expect(rows.filter(r => r.current), 'exactly one machine is marked current').toHaveLength(1)
})
