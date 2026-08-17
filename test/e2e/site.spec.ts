import { expect, test } from '@playwright/test'
import { pathToFileURL } from 'node:url'
import { resolve } from 'node:path'

/**
 * The published site, checked the way it is actually read.
 *
 * These load `docs/` from disk, so they need no server and no control plane —
 * unlike the rest of this suite. They run under the mobile projects because
 * the failure that matters here is a page that scrolls sideways on a phone,
 * and because a colour defined only inside one theme's media query renders as
 * one theme's text on the other theme's ground.
 */

const DOCS = resolve(import.meta.dirname, '../../docs')
const pages = [
  { name: 'index.html', heading: /your machines/i },
  { name: 'zh.html', heading: /你的机器/ },
]

for (const { name, heading } of pages) {
  for (const scheme of ['light', 'dark'] as const) {
    test(`${name} renders in ${scheme}`, async ({ page }) => {
      const errors: string[] = []
      page.on('console', m => { if (m.type() === 'error') errors.push(m.text()) })
      page.on('pageerror', e => errors.push(String(e)))

      await page.emulateMedia({ colorScheme: scheme })
      await page.goto(pathToFileURL(resolve(DOCS, name)).href)

      await expect(page.getByRole('heading', { level: 1 })).toContainText(heading)

      // A transparent body borrows whatever the host paints behind it, which is
      // how a page ends up unreadable in exactly one theme.
      const background = await page.evaluate(() => getComputedStyle(document.body).backgroundColor)
      expect(background, 'body must paint its own background').not.toBe('rgba(0, 0, 0, 0)')

      const overflow = await page.evaluate(() =>
        document.documentElement.scrollWidth - document.documentElement.clientWidth)
      expect(overflow, 'the page must not scroll sideways on a phone').toBeLessThanOrEqual(1)

      // The diagram is the one element wide enough to force it, so it carries
      // its own scroll container rather than dragging the page with it.
      const diagram = page.locator('figure svg')
      await expect(diagram).toBeVisible()

      expect(errors, 'console errors').toEqual([])
    })
  }
}

test('the language switch round-trips', async ({ page }) => {
  await page.goto(pathToFileURL(resolve(DOCS, 'index.html')).href)
  await page.getByRole('link', { name: '中文' }).first().click()
  await expect(page.getByRole('heading', { level: 1 })).toContainText(/你的机器/)
  await page.getByRole('link', { name: 'English' }).first().click()
  await expect(page.getByRole('heading', { level: 1 })).toContainText(/your machines/i)
})
