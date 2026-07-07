import { test, expect } from '@playwright/test'
import { api, authHeaders, login } from './helpers'

// Key vault: add an upstream provider credential through the UI, confirm it is
// stored (masked input) and reported by the API, then delete it. The KeyVault
// deep-link page (/dashboard/keys/:provider) is the shipped credential-entry
// surface today; the generic multi-secret vault UI is spec-04b/later work.
test('vault a provider key via the UI, verify via API, then delete', async ({ page }) => {
  const { ctx, token } = await api()

  // Start clean so the assertion order is deterministic regardless of prior runs.
  await ctx.delete('/api/runtime/llm-credentials/anthropic', { headers: authHeaders(token) })

  await login(page)
  await page.goto('/dashboard/keys/anthropic')
  await expect(page.getByRole('heading', { name: /Add your Anthropic API key/i })).toBeVisible()

  // The key input is masked by default (type=password).
  const keyInput = page.getByPlaceholder(/sk-ant-/i)
  await expect(keyInput).toHaveAttribute('type', 'password')

  await keyInput.fill('sk-ant-e2e-browser-lane-fake-key-value')
  await page.getByRole('button', { name: /Save key|Replace key/i }).click()

  // UI confirms the save.
  await expect(page.getByText(/Key saved\./i)).toBeVisible()

  // API confirms the credential is stored for anthropic.
  const listResp = await ctx.get('/api/runtime/llm-credentials', { headers: authHeaders(token) })
  expect(listResp.ok()).toBeTruthy()
  const list = await listResp.json()
  const anthropic = list.credentials.find((c: { provider: string }) => c.provider === 'anthropic')
  expect(anthropic?.stored, 'anthropic credential should be stored after UI save').toBeTruthy()

  // Reload: the page now shows the credential as already vaulted (masked; the
  // raw value is never rendered back).
  await page.reload()
  await expect(page.getByText(/Anthropic key is already vaulted/i)).toBeVisible()

  // Delete it and confirm the API reflects removal.
  const delResp = await ctx.delete('/api/runtime/llm-credentials/anthropic', { headers: authHeaders(token) })
  expect(delResp.ok()).toBeTruthy()
  const afterResp = await ctx.get('/api/runtime/llm-credentials', { headers: authHeaders(token) })
  const after = await afterResp.json()
  const gone = after.credentials.find((c: { provider: string }) => c.provider === 'anthropic')
  expect(gone?.stored ?? false, 'anthropic credential should be gone after delete').toBeFalsy()

  await ctx.dispose()
})
