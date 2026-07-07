import { defineConfig, devices } from '@playwright/test'

// The browser lane boots a real clawvisor-server subprocess (via serve/main.go)
// in global-setup, logs in over the magic-link API, and saves storageState so
// every spec starts authenticated. BASE_URL is written to .env-browser by
// global-setup and read back here at runtime.
//
// Chromium only in v1 — matches the install lane's single-browser pin
// (Playwright 1.52.0). Firefox/WebKit are a later addition, not a v1 gap.

const baseURL = process.env.BASE_URL || readBaseURLFile()

function readBaseURLFile(): string | undefined {
  // global-setup writes BASE_URL here so the config (evaluated before setup on
  // the first pass, and after on the worker pass) can pick it up. Missing on
  // the very first evaluation — that's fine, global-setup sets process.env.
  try {
    // eslint-disable-next-line @typescript-eslint/no-var-requires
    const fs = require('node:fs')
    const raw = fs.readFileSync(new URL('./.env-browser', import.meta.url), 'utf8')
    const m = raw.match(/^BASE_URL=(.*)$/m)
    return m ? m[1].trim() : undefined
  } catch {
    return undefined
  }
}

export default defineConfig({
  testDir: './tests',
  // The server is a single shared subprocess; keep specs serial-safe by not
  // sharding across processes that would each expect isolated state.
  fullyParallel: false,
  workers: 1,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  timeout: 30_000,
  expect: { timeout: 10_000 },
  reporter: [
    ['list'],
    ['html', { open: 'never', outputFolder: 'playwright-report' }],
  ],
  // global-setup returns an async teardown function that stops the server.
  globalSetup: './global-setup.ts',
  use: {
    baseURL,
    // No shared storageState: the refresh token is single-use/rotating
    // server-side, so each spec logs in its own context via helpers.login().
    trace: 'on-first-retry',
    screenshot: 'only-on-failure',
  },
  projects: [
    {
      name: 'chromium',
      use: {
        ...devices['Desktop Chrome'],
        // --no-sandbox mirrors the install-lane invocation and is required in
        // the CI container.
        launchOptions: { args: ['--no-sandbox', '--disable-setuid-sandbox'] },
      },
    },
  ],
})
