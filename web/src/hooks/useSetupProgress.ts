import { useMemo } from 'react'
import { useQuery } from '@tanstack/react-query'
import { api } from '../api/client'
import { useAuth } from './useAuth'

export interface SetupStep {
  id: string
  label: string
  complete: boolean
  to: string
  optional?: boolean
}

export function useSetupProgress() {
  const { onboardingComplete, currentOrg } = useAuth()
  const orgId = currentOrg?.id

  const { data: services, isLoading: servicesLoading } = useQuery({
    queryKey: ['services', orgId ?? 'personal'],
    queryFn: () => api.services.list(),
  })

  const { data: agents, isLoading: agentsLoading } = useQuery({
    queryKey: ['agents', orgId ?? 'personal'],
    queryFn: () => (orgId ? api.orgs.agents(orgId) : api.agents.list()),
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
    queryKey: ['setup-first-approval', orgId ?? 'personal'],
    queryFn: async () => {
      const listFn = orgId
        ? (status: string) => api.orgs.tasks(orgId, { status, limit: 1 })
        : (status: string) => api.tasks.list({ status, limit: 1 })
      const [completed, denied] = await Promise.all([
        listFn('completed'),
        listFn('denied'),
      ])
      return (completed.total ?? 0) + (denied.total ?? 0) > 0
    },
    enabled: hasAgent,
    staleTime: 60_000,
  })

  // Don't block page render on the first-approval probe — it only affects step completion.
  const isLoading = servicesLoading || agentsLoading

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

    list.push(agentStep, accountStep)

    list.push({
      id: 'approval',
      label: 'See Clawvisor in action',
      complete: !!hasResolvedTask,
      to: '/dashboard/activity',
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
  const isComplete = !isLoading && incompleteRequired.length === 0

  return {
    steps,
    agents: agents ?? [],
    connectedServices,
    incompleteRequired,
    isComplete,
    isLoading,
    progress: requiredSteps.length === 0
      ? 1
      : requiredSteps.filter(s => s.complete).length / requiredSteps.length,
  }
}
