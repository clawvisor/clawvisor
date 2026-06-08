import { useMemo } from 'react'
import { useQuery } from '@tanstack/react-query'
import { api, type Agent } from '../api/client'
import { filterLiveRuntimeApprovals } from '../pages/Runtime'
import { useAuth } from './useAuth'
import type { AttentionItem } from '../components/attention/types'

export function useAttentionItems() {
  const { features } = useAuth()
  const runtimeActivityUI = !!features?.runtime_activity
  const liveSessionsUI = !!features?.agent_live_sessions

  const { data: overview, isLoading: overviewLoading } = useQuery({
    queryKey: ['overview'],
    queryFn: () => api.overview.get(),
    refetchInterval: 30_000,
  })

  const { data: runtimeStatus } = useQuery({
    queryKey: ['runtime-status'],
    queryFn: async () => {
      try {
        return await api.runtime.status()
      } catch {
        return null
      }
    },
    refetchInterval: 30_000,
    enabled: runtimeActivityUI || liveSessionsUI,
  })

  const { data: runtimeApprovals } = useQuery({
    queryKey: ['runtime-approvals'],
    queryFn: async () => {
      try {
        return await api.runtime.listApprovals()
      } catch {
        return { entries: [], total: 0 }
      }
    },
    refetchInterval: 30_000,
    enabled: !!runtimeStatus?.enabled,
  })

  const { data: runtimeSessions } = useQuery({
    queryKey: ['runtime-sessions'],
    queryFn: async () => {
      try {
        return await api.runtime.listSessions()
      } catch {
        return { entries: [], total: 0 }
      }
    },
    refetchInterval: 30_000,
    enabled: liveSessionsUI && !!runtimeStatus?.enabled,
  })

  const { data: agentsData } = useQuery({
    queryKey: ['agents'],
    queryFn: () => api.agents.list(),
  })

  const agentMap = useMemo(() => {
    const m = new Map<string, string>()
    for (const a of (agentsData ?? []) as Agent[]) {
      m.set(a.id, a.name)
    }
    return m
  }, [agentsData])

  const queueItems = overview?.queue ?? []
  const runtimeApprovalItems = useMemo(
    () => (runtimeStatus?.enabled
      ? filterLiveRuntimeApprovals(runtimeApprovals?.entries ?? [], runtimeSessions?.entries ?? [])
      : []),
    [runtimeApprovals, runtimeSessions, runtimeStatus?.enabled],
  )

  const attentionItems = useMemo<AttentionItem[]>(() => {
    const combined: AttentionItem[] = [
      ...queueItems.map(item => ({ kind: 'queue' as const, createdAt: item.created_at, item })),
      ...runtimeApprovalItems.map(approval => ({
        kind: 'runtime_approval' as const,
        createdAt: approval.created_at,
        approval,
      })),
    ]
    return combined.sort((a, b) => new Date(b.createdAt).getTime() - new Date(a.createdAt).getTime())
  }, [queueItems, runtimeApprovalItems])

  return {
    attentionItems,
    attentionCount: attentionItems.length,
    agentMap,
    isLoading: overviewLoading,
  }
}
