import { test, expect } from '@playwright/test'
import { collectConsoleErrors, login } from './helpers'

// Authenticated dashboard render: the saved storageState re-auths the SPA on
// mount, the Overview page renders, and no error-level console messages fire.
//
// NB: the dashboard opens a persistent SSE stream (useEventStream), so
// `networkidle` never settles — we wait on concrete Overview landmarks instead.
test('dashboard overview renders with no console errors', async ({ page }) => {
  const consoleErrors = collectConsoleErrors(page)

  // login() lands the page on /dashboard via the magic-link exchange.
  await login(page)
  await page.goto('/dashboard')
  await expect
    .poll(() => new URL(page.url()).pathname, { timeout: 15_000 })
    .toContain('/dashboard')

  // Sidebar shell + Overview content landmarks (Overview renders an <h1>
  // Dashboard heading and an activity panel once its queries resolve).
  await expect(page.getByRole('link', { name: 'Overview' })).toBeVisible()
  await expect(page.getByRole('heading', { name: 'Dashboard', exact: true })).toBeVisible()
  await expect(page.getByRole('heading', { name: /Activity \(last 60 min\)/i })).toBeVisible()

  expect(consoleErrors(), `unexpected console errors:\n${consoleErrors().join('\n')}`).toEqual([])
})
