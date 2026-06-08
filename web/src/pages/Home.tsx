import { useQuery } from '@tanstack/react-query'
import { useAuth } from '../hooks/useAuth'
import { useSetupProgress } from '../hooks/useSetupProgress'
import { api } from '../api/client'
import GetStarted from './GetStarted'
import Overview from './Overview'

/**
 * Unified Home — onboarding (formerly Quickstart) until setup is complete,
 * then the operational dashboard.
 */
export default function Home() {
  const { currentOrg } = useAuth()
  const orgId = currentOrg?.id
  const { isComplete, isLoading: setupLoading, steps } = useSetupProgress()

  const { data: agents, isLoading: agentsLoading, isError: agentsError } = useQuery({
    queryKey: ['agents', orgId ?? 'personal'],
    queryFn: () => (orgId ? api.orgs.agents(orgId) : api.agents.list()),
    retry: 2,
  })

  if (setupLoading || (agentsLoading && !agentsError)) {
    return <div className="p-4 sm:p-8 ds-page-loading">loading…</div>
  }

  const hasAgent = steps.some(s => s.id === 'agent' && s.complete) || (agents ?? []).length > 0
  const showOnboarding = !isComplete || !hasAgent

  if (showOnboarding) {
    return <GetStarted />
  }

  return <Overview />
}
