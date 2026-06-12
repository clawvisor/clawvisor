import { useMemo } from 'react'
import { useQuery } from '@tanstack/react-query'
import { api } from '../api/client'
import { useAuth } from './useAuth'
import { filterLiveRuntimeApprovals, isActiveRuntimeSession } from '../pages/Runtime'
import type { AttentionItem } from '../components/attention/types'

// Source of truth for inbox + sidebar badge math. Merges queue items and
// runtime approvals (when runtime is enabled) into one sorted list.
//
// `isLoading` covers every query that contributes to the merged list — not
// just overview. Reflecting only `overview` would let consumers (e.g. the
// nav badge or any "agents.length === 0" gate) compute a stale count
// before runtime data has resolved.
export function useAttentionItems() {
  const { features } = useAuth()
  const runtimeActivityUI = !!features?.runtime_activity
  const liveSessionsUI = !!features?.agent_live_sessions

  const overviewQuery = useQuery({
    queryKey: ['overview'],
    queryFn: () => api.overview.get(),
    refetchInterval: 30_000,
  })

  const runtimeStatusQuery = useQuery({
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

  const runtimeEnabled = !!runtimeStatusQuery.data?.enabled
  // Per PRD §8.3, `runtime_activity` gates runtime approvals in the inbox;
  // `agent_live_sessions` is a separate flag that controls the live-session
  // count on Home. Deployments can flip these independently via FeaturesHook,
  // so the approvals fetch is gated on both flag and proxy state to keep
  // the badge math correct when a deployment intentionally disables the
  // runtime-approval UX while still showing live sessions.
  const showRuntimeApprovals = runtimeActivityUI && runtimeEnabled

  const runtimeApprovalsQuery = useQuery({
    queryKey: ['runtime-approvals'],
    queryFn: async () => {
      try {
        return await api.runtime.listApprovals()
      } catch {
        return { entries: [], total: 0 }
      }
    },
    refetchInterval: 30_000,
    enabled: showRuntimeApprovals,
  })

  const runtimeSessionsQuery = useQuery({
    queryKey: ['runtime-sessions'],
    queryFn: async () => {
      try {
        return await api.runtime.listSessions()
      } catch {
        return { entries: [], total: 0 }
      }
    },
    refetchInterval: 30_000,
    enabled: runtimeEnabled,
  })

  const queueItems = overviewQuery.data?.queue ?? []
  const liveRuntimeApprovals = useMemo(
    () =>
      showRuntimeApprovals
        ? filterLiveRuntimeApprovals(
            runtimeApprovalsQuery.data?.entries ?? [],
            runtimeSessionsQuery.data?.entries ?? [],
          )
        : [],
    [showRuntimeApprovals, runtimeApprovalsQuery.data, runtimeSessionsQuery.data],
  )

  const items = useMemo<AttentionItem[]>(() => {
    const combined: AttentionItem[] = [
      ...queueItems.map(item => ({
        kind: 'queue' as const,
        id: `queue:${item.id}`,
        createdAt: item.created_at,
        item,
      })),
      ...liveRuntimeApprovals.map(approval => ({
        kind: 'runtime_approval' as const,
        id: `runtime:${approval.id}`,
        createdAt: approval.created_at,
        approval,
      })),
    ]
    return combined.sort((a, b) => new Date(b.createdAt).getTime() - new Date(a.createdAt).getTime())
  }, [queueItems, liveRuntimeApprovals])

  const activeRuntimeSessions = useMemo(
    () =>
      liveSessionsUI && runtimeEnabled
        ? (runtimeSessionsQuery.data?.entries ?? []).filter(isActiveRuntimeSession)
        : [],
    [runtimeSessionsQuery.data, liveSessionsUI, runtimeEnabled],
  )

  // `isLoading` waits for every query whose result feeds `items`. When runtime
  // is disabled the runtime queries are inert (enabled=false) and stay
  // `isLoading=false` so they don't artificially block the inbox.
  const isLoading =
    overviewQuery.isLoading ||
    runtimeStatusQuery.isLoading ||
    runtimeApprovalsQuery.isLoading ||
    runtimeSessionsQuery.isLoading

  return {
    items,
    count: items.length,
    isLoading,
    runtimeStatus: runtimeStatusQuery.data ?? null,
    activeRuntimeSessions,
  }
}
