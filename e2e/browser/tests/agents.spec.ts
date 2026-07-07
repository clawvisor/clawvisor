import { test, expect } from '@playwright/test'
import { api, authHeaders, login } from './helpers'

// Agents: register an agent through the UI and confirm it appears with its
// attribution fields, and that the API records it. This is the create-side of
// the agent lifecycle the provider (spec 06b) and the realclient lane both
// depend on.
test('register an agent in the UI and see it with attribution', async ({ page }) => {
  const { ctx, token } = await api()

  await login(page)
  await page.goto('/dashboard/agents')

  const name = `e2e-agent-${Date.now()}`

  // Open the inline create form and register the agent.
  await page.getByRole('button', { name: 'Add Agent', exact: true }).click()
  await page.getByPlaceholder('Agent name').fill(name)
  await page.getByRole('button', { name: 'Create', exact: true }).click()

  // The one-time token reveal confirms creation.
  await expect(page.getByText(/copy your token now/i)).toBeVisible()

  // The agent appears in the list with its attribution (name + id + created time).
  await expect(page.getByText(name, { exact: true })).toBeVisible()

  // API records the agent with a stable id (the attribution key).
  const listResp = await ctx.get('/api/agents', { headers: authHeaders(token) })
  expect(listResp.ok()).toBeTruthy()
  const agents = (await listResp.json()) as Array<{ id: string; name: string; created_at: string }>
  const created = agents.find((a) => a.name === name)
  expect(created, 'agent should exist via API').toBeTruthy()
  expect(created!.id, 'agent has an attributable id').toBeTruthy()
  expect(created!.created_at, 'agent has a created_at attribution field').toBeTruthy()

  // Clean up so re-runs stay deterministic.
  await ctx.delete(`/api/agents/${created!.id}`, { headers: authHeaders(token) })
  await ctx.dispose()
})
