import { Navigate, useLocation, useSearchParams } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { api, type Agent } from '../api/client'
import { useAuth } from '../hooks/useAuth'

// Decides where `/dashboard` should land. The PRD calls for:
//   - deep-link query params (?action=… with request_id or task_id) →
//     forward to /dashboard/inbox so approvals are never blocked by the
//     agent-gate redirect, even for first-run users
//   - zero agents → /dashboard/quickstart (first-run setup)
//   - ≥1 agent  → /dashboard/home (Overview content)
//
// The agents query is gated on the deep-link check so deep-link redirects
// don't trigger an extra network round-trip.
export default function DashboardIndex() {
  const { currentOrg } = useAuth()
  const location = useLocation()
  const [searchParams] = useSearchParams()

  const action = searchParams.get('action')
  const requestId = searchParams.get('request_id')
  const taskId = searchParams.get('task_id')
  const hasDeepLink = !!action && (!!requestId || !!taskId)

  // Hooks must be called in the same order on every render, so the agents
  // query is hoisted above the deep-link branch. `enabled: !hasDeepLink`
  // skips the network round-trip when the deep-link path will redirect
  // away before this component ever uses `agents`.
  const orgId = currentOrg?.id
  const { data: agents, isLoading, isError } = useQuery({
    queryKey: orgId ? ['org-agents', orgId] : ['agents'],
    queryFn: (): Promise<Agent[]> =>
      orgId ? api.orgs.agents(orgId) : api.agents.list(),
    enabled: !hasDeepLink,
  })

  if (hasDeepLink) {
    return (
      <Navigate
        to={{ pathname: '/dashboard/inbox', search: location.search }}
        replace
      />
    )
  }

  if (isLoading) {
    return (
      <div className="p-8 text-sm text-text-tertiary">Loading dashboard…</div>
    )
  }

  // On error, fall through to Quickstart. A transient `/api/agents` failure
  // shouldn't pin every magic-link landing on a permanent loading spinner;
  // Quickstart is the safe first-run default and any nav item still works.
  const agentCount = isError ? 0 : agents?.length ?? 0
  if (agentCount === 0) {
    return <Navigate to="/dashboard/quickstart" replace />
  }

  return <Navigate to="/dashboard/home" replace />
}
