import { test, expect, request } from '@playwright/test'
import { baseURL } from './helpers'

// Replicates and supersedes e2e/browser/scripts/verify_dashboard.mjs: drive the
// magic-link exchange in the real SPA and assert the user lands on /dashboard
// (authenticated) rather than back on the login/magic-link page. This spec owns
// the raw login flow, so it does not use the login() helper.
test('magic-link exchange lands on the dashboard', async ({ page }) => {
  // Mint a one-time magic token via the localhost-only endpoint, exactly as the
  // CLI/daemon does when it prints a dashboard link.
  const ctx = await request.newContext({ baseURL: baseURL() })
  const magic = await ctx.post('/api/auth/magic/local')
  expect(magic.ok(), `magic/local: ${magic.status()}`).toBeTruthy()
  const { token } = await magic.json()
  await ctx.dispose()

  // The magic-link page exchanges the token and does a HARD redirect
  // (window.location.href) to /dashboard. waitForURL can race that navigation,
  // so we poll for the settled URL — the same defensive shape verify_dashboard
  // used.
  await page.goto(`/magic-link?token=${encodeURIComponent(token)}`, { waitUntil: 'load' })

  await expect
    .poll(() => new URL(page.url()).pathname, { timeout: 15_000, intervals: [250, 500, 1000] })
    .toContain('/dashboard')

  // Sanity: we are NOT sitting on the magic-link page anymore.
  expect(page.url()).not.toContain('/magic-link')
})
