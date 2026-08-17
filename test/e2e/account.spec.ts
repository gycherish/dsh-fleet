import { expect, test, type Page } from '@playwright/test'

import { signIn } from './helpers.ts'

/**
 * Accounts, machine tokens, and the two ways this console could lock everyone
 * out of itself.
 *
 * The token page is the front half of self-enrolment: without a token there is
 * nothing to paste into a machine, and without the uplink address beside it
 * there are three things to assemble by hand from two pages.
 */

/** Admin-only routes must refuse, not redirect: a redirect sends an ordinary
 *  account round a login loop it can never finish. */
test('an ordinary account is refused the people page and never shown the link', async ({ page, browser }) => {
  await signIn(page)

  // Make one, if a previous run has not.
  await page.goto('/_fleet/people')
  const add = page.locator('.card').filter({ hasText: /Add someone/ })
  if (await page.getByText('ordinary', { exact: true }).count() === 0) {
    await add.locator('input[name=username]').fill('ordinary')
    await add.locator('input[name=password]').fill('ordinary-password-1')
    await add.locator('input[name=again]').fill('ordinary-password-1')
    await add.getByRole('button', { name: /create account/i }).click()
    await expect(page.locator('.banner.ok')).toBeVisible()
  }

  const theirs = await (await browser.newContext({ ignoreHTTPSErrors: true })).newPage()
  await theirs.goto('/_fleet/login')
  await theirs.getByLabel(/username/i).fill('ordinary')
  await theirs.getByLabel(/password/i).fill('ordinary-password-1')
  await theirs.getByRole('button', { name: /sign in/i }).click()
  await theirs.waitForURL(url => !url.pathname.startsWith('/_fleet/login'))

  const refused = await theirs.goto('/_fleet/people')
  expect(refused?.status(), 'admin-only means 403, not a login redirect').toBe(403)

  const own = await theirs.goto('/_fleet/account')
  expect(own?.status(), 'but their own account page is theirs').toBe(200)
  await expect(theirs.getByRole('link', { name: /^People$/ })).toHaveCount(0)
  await theirs.context().close()
})

test('the last administrator cannot be demoted', async ({ page }) => {
  await signIn(page)
  await page.goto('/_fleet/people')

  const admins = page.locator('.person').filter({ has: page.locator('.tag.admin') })
  const before = await admins.count()

  const self = page.locator('.person').filter({ has: page.locator('.tag.you') })
  await self.getByRole('button', { name: /remove admin/i }).click()

  if (before === 1) {
    await expect(page.locator('.banner.bad')).toContainText(/last administrator/i)
    // And it really did not happen.
    await expect(page.locator('.person').filter({ has: page.locator('.tag.admin') })).toHaveCount(1)
  } else {
    // With another admin present the demotion is allowed, so put it back.
    await expect(page.locator('.banner.ok')).toBeVisible()
    await self.getByRole('button', { name: /make admin/i }).click()
    await expect(page.locator('.banner.ok')).toBeVisible()
  }
})

test('a minted token is shown once, with everything a machine needs', async ({ page }, testInfo) => {
  await signIn(page)
  await page.goto('/_fleet/account')

  // Unique per project, and revoked at the end: a test that leaves tokens
  // behind poisons its own assertions on the next run, which is how this one
  // first failed.
  const label = `e2e ${testInfo.project.name} reveal`
  await page.getByLabel(/what is it for/i).fill(label)
  await page.getByRole('button', { name: /create token/i }).click()

  const reveal = page.locator('.card.minted')
  await expect(reveal).toBeVisible()
  const block = await reveal.locator('pre').innerText()

  // All four fields together: assembling them from separate pages is how a
  // machine ends up half-configured and silently offline.
  expect(block, 'the uplink address, derived from the public URL').toMatch(/url:\s+wss?:\/\/\S+\/uplink/)
  expect(block, 'the account name').toMatch(/username:\s+\S+/)
  expect(block, 'the token itself').toMatch(/token:\s+ut_[A-Za-z0-9_-]{10,}/)
  expect(block, 'and a reminder to name the machine').toMatch(/nodeId:/)
  await expect(reveal).toContainText(/shown once/i)

  // Reloading must not show it again, and must not mint a second one either:
  // rendering the mint straight into the POST response did both.
  const before = await page.locator('tr').filter({ hasText: label }).count()
  await page.reload()
  await expect(page.locator('.card.minted')).toHaveCount(0)
  await expect(page.locator('tr').filter({ hasText: label })).toHaveCount(before)

  // Tidy up, so the next run starts from the same place.
  await page.locator('tr').filter({ hasText: label }).first()
    .getByRole('button', { name: /revoke/i }).click()
  await expect(page.locator('.banner.ok')).toBeVisible()
})

test('a revoked token is listed as revoked rather than vanishing', async ({ page }, testInfo) => {
  await signIn(page)
  await page.goto('/_fleet/account')
  const label = `e2e ${testInfo.project.name} revoke`
  await page.getByLabel(/what is it for/i).fill(label)
  await page.getByRole('button', { name: /create token/i }).click()

  const row = page.locator('tr').filter({ hasText: label }).first()
  await row.getByRole('button', { name: /revoke/i }).click()
  await expect(page.locator('.banner.ok')).toContainText(/revoked/i)

  // Still there, struck through: a token you cannot see is a token you cannot
  // reason about when a machine stops connecting.
  const revoked = page.locator('tr').filter({ hasText: label }).first()
  await expect(revoked).toContainText(/revoked/i)
  await expect(revoked.getByRole('button', { name: /revoke/i })).toHaveCount(0)
})

test('the account page is reachable from a machine, via the overlay', async ({ page }: { page: Page }) => {
  await signIn(page)

  // Whichever machine is actually up. Naming one would make this test fail for
  // the wrong reason the moment the fixture fleet changes shape.
  const online = page.locator('a.card.online').first()
  await expect(online, 'this check needs one connected machine').toBeVisible()
  await online.click()
  await page.waitForURL(/\/$/)
  await page.waitForLoadState('networkidle')

  const host = page.locator('dshf-console').first()
  await expect(host).toBeAttached({ timeout: 30_000 })
  await host.evaluate((el: Element) => {
    (el.shadowRoot?.querySelector('.pill') as HTMLElement | null)?.click()
  })
  const hrefs = await host.evaluate((el: Element) =>
    [...(el.shadowRoot?.querySelectorAll('.foot a') ?? [])].map(a => a.getAttribute('href')))
  expect(hrefs, 'minting a token must not require finding the console first').toContain('/_fleet/account')
})
