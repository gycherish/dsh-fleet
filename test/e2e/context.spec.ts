import { expect, test } from '@playwright/test'

/**
 * Browsers gate a set of APIs behind a "secure context": HTTPS, or the
 * loopback exemption for `localhost` / `127.0.0.1`. A LAN address over plain
 * HTTP is NOT one, and `crypto.randomUUID` is among the gated APIs.
 *
 * That makes it a hard requirement rather than a hardening step: the dsh
 * client calls `crypto.randomUUID`, so reaching a machine from a phone over
 * `http://192.168.x.x` breaks pages that mint ids, while the same build works
 * perfectly when tested from the host itself over loopback.
 *
 * This test states the rule so nobody rediscovers it from a red banner in a
 * settings page three screens deep.
 */
test('the origin under test is a secure context', async ({ page }) => {
  await page.goto('/_fleet/login')

  const probe = await page.evaluate(() => ({
    origin: location.origin,
    secure: window.isSecureContext,
    randomUUID: typeof globalThis.crypto?.randomUUID,
    subtle: typeof globalThis.crypto?.subtle,
  }))

  expect(
    probe.secure,
    `${probe.origin} is not a secure context, so crypto.randomUUID is `
    + `${probe.randomUUID}. Serve the control plane over HTTPS; loopback is the only exemption.`,
  ).toBe(true)
  expect(probe.randomUUID, 'crypto.randomUUID must be callable').toBe('function')
  expect(probe.subtle, 'crypto.subtle must be present').toBe('object')
})
