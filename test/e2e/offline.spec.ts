import { expect, test } from '@playwright/test'

import { signIn } from './helpers.ts'

/**
 * Machines that cannot answer.
 *
 * Opening one used to replace the whole screen with a bare "node is not
 * connected" — and because a selected machine owns the origin root, that page
 * carried none of the console's chrome. There was no way back and no way to
 * sign out, which is the same trap the injected control was built to close,
 * reached through the error path instead.
 *
 * Two halves now: the chooser refuses to walk into it, and the page you get if
 * a machine dies while you are driving it is a real one.
 */

/** Registered in the fixture fleet and deliberately never started. */
const ABSENT = process.env.DSHF_ABSENT_NODE ?? 'workstation'

test('clicking an offline machine asks instead of navigating', async ({ page }) => {
  await signIn(page)

  const card = page.locator(`a[href="/_fleet/select/${ABSENT}"]`)
  await expect(card).toBeVisible()
  await card.click()

  // Still on the chooser, with an explanation rather than a dead end.
  await expect(page).toHaveURL(/_fleet\/console$/)
  const dialog = page.locator('dialog#offline')
  await expect(dialog).toBeVisible()
  await expect(dialog).toContainText(/is not connected/i)

  // Cancel leaves everything as it was.
  await dialog.getByRole('button', { name: /cancel/i }).click()
  await expect(dialog).toBeHidden()
  await expect(page).toHaveURL(/_fleet\/console$/)
})

test('opening one anyway lands on a page with a way back', async ({ page }) => {
  await signIn(page)
  await page.locator(`a[href="/_fleet/select/${ABSENT}"]`).click()
  await page.locator('dialog#offline').getByRole('link', { name: /open anyway/i }).click()

  // The machine owns the root, so this is the whole screen. It must carry the
  // two escapes the bare 502 did not.
  await expect(page.getByRole('heading', { name: /not connected|revoked/i })).toBeVisible()
  await expect(page.getByRole('link', { name: /all machines/i })).toBeVisible()
  await expect(page.getByRole('button', { name: /sign out/i })).toBeVisible()

  await page.getByRole('link', { name: /all machines/i }).click()
  await expect(page.getByRole('heading', { name: /your machines/i })).toBeVisible()
})

test('an unreachable machine still fails as a status for a fetch', async ({ page }) => {
  await signIn(page)
  await page.goto(`/_fleet/select/${ABSENT}`)

  // A page is for navigations only. Returning HTML to an `/api` call would be
  // parsed as its answer, which is how an outage turns into a JSON parse error.
  const probe = await page.evaluate(async () => {
    const response = await fetch('/api/session.list', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: '{}',
    })
    return { status: response.status, type: response.headers.get('content-type') ?? '' }
  })
  expect(probe.status).toBe(502)
  expect(probe.type, 'a fetch must not be answered with a page').not.toContain('text/html')
})
