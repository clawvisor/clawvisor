import { test, expect } from '@playwright/test'
import { api, authHeaders, login } from './helpers'

// Policies: the Policy page renders the governance surface, and a policy
// mutation persists through the API. The per-service restriction toggles in the
// UI require an activated service (a harness-mock dependency the browser-lane
// server does not wire), so the mutation itself is driven at the API layer and
// the Policy surface render is asserted in-UI. When spec 02/06a land their
// instance-policy UI, the mutation half moves fully in-browser here.
test('the policy surface renders and a restriction persists via the API', async ({ page }) => {
  const { ctx, token } = await api()

  // UI: the Policy page renders its governance surface.
  await login(page)
  await page.goto('/dashboard/policy')
  await expect(page.getByRole('heading', { name: 'Policy', exact: true })).toBeVisible()

  // Mutation: create a restriction and confirm it persists via the API.
  const service = `e2e-svc-${Date.now()}`
  const action = 'issues.create'
  const createResp = await ctx.post('/api/restrictions', {
    data: { service, action, reason: 'browser-lane policy test' },
    headers: authHeaders(token),
  })
  expect(createResp.ok(), `create restriction: ${createResp.status()} ${await createResp.text()}`).toBeTruthy()
  const created = await createResp.json()

  const listResp = await ctx.get('/api/restrictions', { headers: authHeaders(token) })
  expect(listResp.ok()).toBeTruthy()
  const restrictions = (await listResp.json()) as Array<{ id: string; service: string; action: string }>
  expect(
    restrictions.some((r) => r.service === service && r.action === action),
    'restriction should be persisted',
  ).toBeTruthy()

  // Clean up so re-runs stay deterministic.
  await ctx.delete(`/api/restrictions/${created.id}`, { headers: authHeaders(token) })
  await ctx.dispose()
})
