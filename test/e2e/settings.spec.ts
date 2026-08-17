import { expect, test } from '@playwright/test'

/**
 * The settings pages, which are the ones the privileged-access tier decides.
 *
 * `settings.describe`, `credentials.describe` and `agentPreset.read` are pinned
 * to loopback by dsh, so a custom carrier has to choose whether to forward
 * them. Refusing them all — the first thing this control plane did — left a UI
 * that logged in, listed sessions, and answered every other call, then showed
 * "HTTP 403" the moment anyone opened Models. The suite passed anyway, because
 * nothing in it opened Models.
 *
 * So this asserts the reads land and the writes do not, through the real UI.
 */

const NODE_ID = process.env.DSHF_NODE ?? 'devbox'
const USERNAME = process.env.DSHF_USER ?? 'admin'
const PASSWORD = process.env.DSHF_PASSWORD ?? 'dev-only-password'

/** dsh greets a fresh browser with a modal that swallows every other click. */
async function signInAndOpen(page: import('@playwright/test').Page): Promise<string[]> {
  const failures: string[] = []
  page.on('response', response => {
    if (response.url().includes('/api/') && response.status() >= 400) {
      failures.push(`${response.status()} ${new URL(response.url()).pathname}`)
    }
  })

  await page.goto('/_fleet/console')
  await page.getByLabel(/username/i).fill(USERNAME)
  await page.getByLabel(/password/i).fill(PASSWORD)
  await page.getByRole('button', { name: /sign in/i }).click()
  await page.goto(`/_fleet/select/${NODE_ID}`)
  await page.waitForLoadState('networkidle')

  const notice = page.getByRole('button', { name: /^Continue$/ })
  if (await notice.count()) await notice.first().click()
  return failures
}

test('the Models page loads its providers through the control plane', async ({ page }) => {
  const failures = await signInAndOpen(page)

  // Phones collapse the sidebar behind a toggle; both layouts reach the panel.
  const sidebar = page.getByRole('button', { name: /open sidebar/i })
  if (await sidebar.count() && await sidebar.first().isVisible()) await sidebar.first().click()
  await page.getByRole('button', { name: 'Settings', exact: true }).click()
  await page.getByText(/^Models$/).first().click()

  // The heading proves the panel switched; the copy proves `settings.describe`
  // and `credentials.describe` came back with something to render.
  await expect(page.getByText(/Enter your API keys/i)).toBeVisible()
  await expect(page.getByText(/^DeepSeek$/).first()).toBeVisible()

  expect(failures, 'no /api call may fail while the settings pages load').toEqual([])
})

test('the whole pinned set reaches the node by default', async ({ page }) => {
  await signInAndOpen(page)

  const status = await page.evaluate(async () => {
    const call = async (method: string) => (await fetch(`/api/${method}`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: '{}',
    })).status
    const out: Record<string, number> = {}
    for (const method of [
      'settings.describe', 'credentials.describe', 'agentPreset.read',
      'settings.update', 'settings.replace', 'settings.mutate', 'settings.openDocument',
      'credentials.set', 'credentials.unset',
      'agentPreset.copy', 'agentPreset.openDocument', 'agentPreset.remove',
      'host.pickDirectory', 'host.openPath', 'llm.discoverModels',
    ]) out[method] = await call(method)
    return out
  })

  // 200 means the gate forwarded and the node answered. What the node makes of
  // a deliberately empty payload is its own business — the point here is that
  // nothing comes back 403, which is what broke the Agent presets page.
  for (const [method, code] of Object.entries(status)) {
    expect(code, `${method} must not be refused by the control plane`).not.toBe(403)
    expect(code, `${method} must reach the node`).toBe(200)
  }
})
