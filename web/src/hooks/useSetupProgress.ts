import { useMemo } from 'react'
import { useQuery } from '@tanstack/react-query'
import { api, type Agent } from '../api/client'
import { useAuth } from './useAuth'

export interface SetupStep {
  id: 'security' | 'agent' | 'account' | 'approval' | 'llm'
  label: string
  complete: boolean
  to: string
  optional?: boolean
}

// Drives the new Quickstart / Home setup checklist (PRD Epic C). Steps are
// computed from the same data sources OnboardingBanner used (services list +
// agents list) plus a probe for the first resolved task and the optional
// LLM key reminder.
export function useSetupProgress() {
  const { onboardingComplete, currentOrg, isLoading: authLoading } = useAuth()
  const orgId = currentOrg?.id

  // `/api/services` is a global endpoint — it does not change with org
  // context. Using a plain `['services']` key keeps the cache shared with
  // OnboardingBanner and Services.tsx; keying it by orgId would issue a
  // duplicate request whose response is identical to the personal one.
  const { data: services, isLoading: servicesLoading } = useQuery({
    queryKey: ['services'],
    queryFn: () => api.services.list(),
  })

  // Agents IS org-scoped (different endpoint per context), so the cache key
  // varies with orgId.
  const { data: agents, isLoading: agentsLoading } = useQuery({
    queryKey: orgId ? ['org-agents', orgId] : ['agents'],
    queryFn: (): Promise<Agent[]> => (orgId ? api.orgs.agents(orgId) : api.agents.list()),
  })

  const { data: llmStatus } = useQuery({
    queryKey: ['llm-status'],
    queryFn: () => api.llm.status(),
  })

  const hasAgent = (agents ?? []).length > 0
  const connectedServices = (services?.services ?? []).filter(
    s => s.status === 'activated' && (s.requires_activation ?? true),
  )
  const hasService = connectedServices.length > 0

  const { data: hasResolvedTask } = useQuery({
    queryKey: orgId ? ['setup-first-approval-org', orgId] : ['setup-first-approval'],
    queryFn: async () => {
      const listFn = orgId
        ? (status: string) => api.orgs.tasks(orgId, { status, limit: 1 })
        : (status: string) => api.tasks.list({ status, limit: 1 })
      const [completed, denied] = await Promise.all([listFn('completed'), listFn('denied')])
      return (completed.total ?? 0) + (denied.total ?? 0) > 0
    },
    enabled: hasAgent,
    staleTime: 60_000,
  })

  // `isLoading` waits for every signal a step depends on. The auth check is
  // critical: useAuth resolves `isLoading` before `onboardingComplete`
  // settles, so a strict `=== false` check on a still-`null` value would
  // otherwise skip the security step during the first render and let the
  // checklist briefly flip to "complete" for users who haven't onboarded.
  const isLoading = authLoading || onboardingComplete === null || servicesLoading || agentsLoading

  const steps = useMemo<SetupStep[]>(() => {
    const list: SetupStep[] = []

    if (onboardingComplete === false) {
      list.push({
        id: 'security',
        label: 'Complete security setup',
        complete: false,
        to: '/onboarding',
      })
    }

    const agentStep: SetupStep = {
      id: 'agent',
      label: 'Connect an agent',
      complete: hasAgent,
      to: '/dashboard/agents',
    }
    const accountStep: SetupStep = {
      id: 'account',
      label: 'Connect an account',
      complete: hasService,
      to: '/dashboard/accounts',
    }

    // Agent before account: proxy-lite needs the token first; non-proxy-lite
    // doesn't care about order per the PRD.
    list.push(agentStep, accountStep)

    list.push({
      id: 'approval',
      label: 'Resolve your first task',
      complete: !!hasResolvedTask,
      to: '/dashboard/inbox',
    })

    if (llmStatus?.spend_cap_exhausted) {
      list.push({
        id: 'llm',
        label: 'Add an LLM API key',
        complete: false,
        to: '/dashboard/settings',
        optional: true,
      })
    }

    return list
  }, [hasAgent, hasService, hasResolvedTask, llmStatus?.spend_cap_exhausted, onboardingComplete])

  const requiredSteps = steps.filter(s => !s.optional)
  const incompleteRequired = requiredSteps.filter(s => !s.complete)
  // `isComplete` is conservative: only true when nothing is loading AND every
  // required step is done. While anything is still loading we report false.
  const isComplete = !isLoading && incompleteRequired.length === 0

  return {
    steps,
    agents: agents ?? [],
    connectedServices,
    incompleteRequired,
    isComplete,
    isLoading,
    progress:
      requiredSteps.length === 0
        ? 1
        : requiredSteps.filter(s => s.complete).length / requiredSteps.length,
  }
}
