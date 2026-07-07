import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, resolve } from 'node:path'
import { request, expect, type APIRequestContext, type Page } from '@playwright/test'

const __dirname = dirname(fileURLToPath(import.meta.url))

// baseURL is written to .env-browser by global-setup. Worker processes don't
// inherit the main process's env mutation, so we read the file.
export function baseURL(): string {
  if (process.env.BASE_URL) return process.env.BASE_URL
  const raw = readFileSync(resolve(__dirname, '..', '.env-browser'), 'utf8')
  const m = raw.match(/^BASE_URL=(.*)$/m)
  if (!m) throw new Error('BASE_URL not found in .env-browser — global-setup did not run')
  return m[1].trim()
}

// login authenticates a browser page via the local magic-link flow: mint a
// one-time token, drive the /magic-link exchange, and wait for the SPA to land
// on /dashboard. Each spec logs in its OWN context — the refresh token is
// single-use/rotating server-side, so a shared storageState would only
// authenticate the first page load.
export async function login(page: Page): Promise<void> {
  const ctx = await request.newContext({ baseURL: baseURL() })
  const magic = await ctx.post('/api/auth/magic/local')
  expect(magic.ok(), `magic/local: ${magic.status()} ${await magic.text()}`).toBeTruthy()
  const { token } = await magic.json()
  await ctx.dispose()

  await page.goto(`/magic-link?token=${encodeURIComponent(token)}`, { waitUntil: 'load' })
  await expect
    .poll(() => new URL(page.url()).pathname, { timeout: 15_000, intervals: [250, 500, 1000] })
    .toContain('/dashboard')
}

// api returns an authenticated APIRequestContext for server-state assertions and
// seeding. It performs its own magic-link login (the local flow always mints the
// same single-user identity) and returns the access token + user id.
export async function api(): Promise<{ ctx: APIRequestContext; token: string; userId: string }> {
  const ctx = await request.newContext({ baseURL: baseURL() })
  const magic = await ctx.post('/api/auth/magic/local')
  if (!magic.ok()) throw new Error(`magic/local: ${magic.status()} ${await magic.text()}`)
  const { token: magicToken } = await magic.json()
  const exchange = await ctx.post('/api/auth/magic', { data: { token: magicToken } })
  if (!exchange.ok()) throw new Error(`magic exchange: ${exchange.status()} ${await exchange.text()}`)
  const body = await exchange.json()
  return { ctx, token: body.access_token, userId: body.user?.id ?? '' }
}

// authHeaders is the Bearer header for the authenticated API context.
export function authHeaders(token: string): Record<string, string> {
  return { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' }
}

// collectConsoleErrors attaches a console listener and returns a getter for the
// accumulated error-level messages. Known-benign noise is filtered.
export function collectConsoleErrors(page: Page): () => string[] {
  const errors: string[] = []
  page.on('console', (msg) => {
    if (msg.type() !== 'error') return
    const text = msg.text()
    if (/favicon|ResizeObserver loop|Failed to load resource/i.test(text)) return
    errors.push(text)
  })
  page.on('pageerror', (err) => errors.push(String(err)))
  return () => errors
}
