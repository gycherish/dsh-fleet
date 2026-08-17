import { expect, type Page } from '@playwright/test'

/** Shared sign-in, because getting it subtly wrong cost three specs. */

export const NODE_ID = process.env.DSHF_NODE ?? 'devbox'
export const USERNAME = process.env.DSHF_USER ?? 'admin'
export const PASSWORD = process.env.DSHF_PASSWORD ?? 'dev-only-password'

/**
 * Sign in, and do not return until the session actually exists.
 *
 * The wait matters far more than it looks. Navigating to the next URL straight
 * after submitting interrupts the login POST, so the session cookie is never
 * stored and every request afterwards lands back on the login page — where
 * there is no overlay, no machine, and every `/api` call is a 401.
 *
 * Chromium usually won that race and WebKit usually lost it, which is exactly
 * why the three specs that navigated immediately failed on the iPhone profile
 * about a third of the time while the one spec that waited never did. It read
 * like a flaky engine for a while. It was a flaky test.
 */
export async function signIn(page: Page): Promise<void> {
  await page.goto('/_fleet/console')
  await page.getByLabel(/username/i).fill(USERNAME)
  await page.getByLabel(/password/i).fill(PASSWORD)
  await page.getByRole('button', { name: /sign in/i }).click()
  await page.waitForURL(url => !url.pathname.startsWith('/_fleet/login'), { timeout: 30_000 })
  await expect(page.getByRole('heading', { name: /your machines/i })).toBeVisible()
}

/** Sign in and hand this browser one machine, which then owns the origin root. */
export async function openMachine(page: Page, nodeID = NODE_ID): Promise<void> {
  await signIn(page)
  await page.goto(`/_fleet/select/${nodeID}`)
  await page.waitForURL(/\/$/, { timeout: 30_000 })
  await page.waitForLoadState('networkidle')
}
