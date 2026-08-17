import { defineConfig, devices } from '@playwright/test'

/**
 * Mobile-viewport checks against an already-running control plane.
 *
 * The stack is not started here on purpose: it needs PostgreSQL, the control
 * plane, and at least one connected node, and a fixture that tried to own all
 * three would be the thing that breaks. Point DSHF_BASE_URL at a stack you
 * brought up and run these against it.
 */
const baseURL = process.env.DSHF_BASE_URL ?? 'http://127.0.0.1:8080'

export default defineConfig({
  testDir: '.',
  testMatch: '*.spec.ts',
  // Each spec drives the same browser session state through a login, so
  // running them in parallel against one control plane invites interference.
  workers: 1,
  fullyParallel: false,
  reporter: [['list']],
  timeout: 60_000,
  expect: { timeout: 15_000 },
  use: {
    baseURL,
    // Self-signed certificates are the norm for a deployment being tried out.
    ignoreHTTPSErrors: true,
    screenshot: 'only-on-failure',
    trace: 'retain-on-failure',
    video: 'off',
  },
  projects: [
    {
      name: 'iphone',
      use: { ...devices['iPhone 14'] },
    },
    {
      name: 'android',
      use: { ...devices['Pixel 7'] },
    },
  ],
})
