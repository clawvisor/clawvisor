import { test, expect } from '@playwright/test'
import { api, authHeaders, login } from './helpers'

// Approvals: an admin resolves a pending request in the UI and the API state
// flips. The proxy-lite tool-use hold flow needs live LLM traffic (spec 02 /
// the realclient lane), so the browser lane exercises the shipped, API-seedable
// human-approval surface that exists today — the agent connection request, which
// parks in "Pending Connections" until an admin approves it in-UI.
test('a pending connection request is approved in the UI and the API state flips', async ({ page }) => {
  const { ctx, token, userId } = await api()

  // Seed a pending connection request (the unauthenticated bootstrap endpoint a
  // fresh agent hits). It parks until an admin approves.
  const name = `e2e-approval-${Date.now()}`
  const connect = await ctx.post('/api/agents/connect', {
    data: { name, user_id: userId },
    headers: { 'Content-Type': 'application/json' },
  })
  expect(connect.ok(), `connect seed: ${connect.status()} ${await connect.text()}`).toBeTruthy()

  // Wait for the seed to be visible server-side before driving the UI.
  await expect
    .poll(async () => {
      const resp = await ctx.get('/api/agents/connections', { headers: authHeaders(token) })
      const list = (await resp.json()) as Array<{ name: string; status: string }>
      return list.some((c) => c.name === name && c.status === 'pending')
    }, { timeout: 10_000 })
    .toBeTruthy()

  await login(page)
  await page.goto('/dashboard/agents')
  await expect(page.getByRole('heading', { name: /Pending Connections/i })).toBeVisible()
  await expect(page.getByText(name, { exact: true })).toBeVisible()

  // Approve in the UI.
  await page.getByRole('button', { name: 'Approve', exact: true }).click()

  // Assert the UI's *durable* post-approval state, not the inline "Approved"
  // badge. That badge lives in ConnectionCard's local `result` state
  // (web/src/pages/Agents.tsx), and the same onSuccess handler that sets it
  // also invalidates the ['connections'] query. GET /api/agents/connections
  // returns only PENDING requests (ConnectionsHandler.List ->
  // ListPendingConnectionRequests), so the refetch drops this request, the
  // parent's `pending.length > 0` guard goes false, and the whole Pending
  // Connections section — badge and all — unmounts. The badge's lifetime is
  // therefore one refetch round-trip; asserting on it is a race the test loses
  // whenever the server answers quickly (always, locally).
  //
  // The durable signals are: the pending section is gone, and the approved
  // agent has materialised in "Your Agents".
  await expect(page.getByRole('heading', { name: /Pending Connections/i })).toBeHidden()
  await expect(page.getByRole('link', { name: new RegExp(name) })).toBeVisible()

  // API state flipped: the request is no longer pending and the agent now exists.
  await expect
    .poll(async () => {
      const resp = await ctx.get('/api/agents', { headers: authHeaders(token) })
      const agents = (await resp.json()) as Array<{ name: string }>
      return agents.some((a) => a.name === name)
    }, { timeout: 10_000 })
    .toBeTruthy()

  await ctx.dispose()
})
