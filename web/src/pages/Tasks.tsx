import { useState, useEffect } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { useSearchParams, Link, Navigate } from 'react-router-dom'
import { api, APIError, type Agent } from '../api/client'
import { useAuth } from '../hooks/useAuth'
import TaskCard from '../components/TaskCard'
import PageLayout from '../components/layout/PageLayout'
import { useAttentionItems } from '../hooks/useAttentionItems'

const STATUS_FILTER_OPTIONS = [
  { value: '', label: 'All tasks' },
  { value: 'active', label: 'Active' },
  { value: 'completed', label: 'Completed' },
  { value: 'expired', label: 'Expired' },
  { value: 'denied', label: 'Denied' },
  { value: 'revoked', label: 'Revoked' },
  { value: 'pending_approval', label: 'Pending approval' },
  { value: 'pending_scope_expansion', label: 'Scope expansion' },
]

const PAGE_SIZE = 25

export default function Tasks() {
  const { currentOrg } = useAuth()
  const orgId = currentOrg?.id
  const [searchParams, setSearchParams] = useSearchParams()
  const initialFilter = searchParams.get('filter') ?? ''
  const [filter, setFilter] = useState(
    initialFilter === 'actionable' ? '' : initialFilter,
  )
  const [offset, setOffset] = useState(0)
  const qc = useQueryClient()
  const [deepLinkResult, setDeepLinkResult] = useState<string | null>(null)
  const { attentionCount } = useAttentionItems()

  const deepApprove = useMutation({
    mutationFn: (taskId: string) => api.tasks.approve(taskId),
    onSuccess: () => { setDeepLinkResult('Task approved.'); qc.invalidateQueries({ queryKey: ['tasks'] }) },
    onError: (err: Error) => {
      if (err instanceof APIError && err.code === 'INLINE_CHAT_BOUND') {
        setDeepLinkResult('Reply approve/deny in the agent chat')
      } else {
        setDeepLinkResult(`Approve failed: ${err.message}`)
      }
    },
  })
  const deepDeny = useMutation({
    mutationFn: (taskId: string) => api.tasks.deny(taskId),
    onSuccess: () => { setDeepLinkResult('Task denied.'); qc.invalidateQueries({ queryKey: ['tasks'] }) },
    onError: (err: Error) => {
      if (err instanceof APIError && err.code === 'INLINE_CHAT_BOUND') {
        setDeepLinkResult('Reply approve/deny in the agent chat')
      } else {
        setDeepLinkResult(`Deny failed: ${err.message}`)
      }
    },
  })
  const deepExpandApprove = useMutation({
    mutationFn: (taskId: string) => api.tasks.expandApprove(taskId),
    onSuccess: () => { setDeepLinkResult('Scope expansion approved.'); qc.invalidateQueries({ queryKey: ['tasks'] }) },
    onError: (err: Error) => setDeepLinkResult(`Expansion approve failed: ${err.message}`),
  })
  const deepExpandDeny = useMutation({
    mutationFn: (taskId: string) => api.tasks.expandDeny(taskId),
    onSuccess: () => { setDeepLinkResult('Scope expansion denied.'); qc.invalidateQueries({ queryKey: ['tasks'] }) },
    onError: (err: Error) => setDeepLinkResult(`Expansion deny failed: ${err.message}`),
  })

  // Handle deep link actions from Telegram buttons (personal context only)
  useEffect(() => {
    if (orgId) return
    const action = searchParams.get('action')
    const taskId = searchParams.get('task_id')
    if (!action || !taskId) return

    setSearchParams({}, { replace: true })

    switch (action) {
      case 'approve': deepApprove.mutate(taskId); break
      case 'deny': deepDeny.mutate(taskId); break
      case 'expand_approve': deepExpandApprove.mutate(taskId); break
      case 'expand_deny': deepExpandDeny.mutate(taskId); break
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const queryParams = (() => {
    const params: { status?: string; limit: number; offset: number } = { limit: PAGE_SIZE, offset }
    if (filter) params.status = filter
    return params
  })()

  const listFn = orgId
    ? (params: typeof queryParams) => api.orgs.tasks(orgId, params)
    : (params: typeof queryParams) => api.tasks.list(params)

  const { data, isLoading, refetch } = useQuery({
    queryKey: ['tasks', orgId ?? 'personal', { filter, offset }],
    queryFn: () => listFn(queryParams),
    refetchInterval: 30_000,
  })

  const { data: agentsData } = useQuery({
    queryKey: ['agents', orgId ?? 'personal'],
    queryFn: () => orgId ? api.orgs.agents(orgId) : api.agents.list(),
  })

  const agentMap = new Map<string, string>()
  for (const a of (agentsData ?? []) as Agent[]) {
    agentMap.set(a.id, a.name)
  }

  const tasks = data?.tasks ?? []
  const total = data?.total ?? 0

  const sorted = [...tasks].sort(
    (a, b) => new Date(b.created_at).getTime() - new Date(a.created_at).getTime(),
  )

  if (initialFilter === 'actionable') {
    return <Navigate to="/dashboard/activity" replace />
  }

  return (
    <PageLayout
      title="task history"
      description="Browse and review past and active tasks. Items that need your decision live in Inbox."
      actions={
        <button onClick={() => refetch()} className="dev-btn-ghost">
          refresh
        </button>
      }
    >
      {attentionCount > 0 && (
        <div className="dev-banner--warning">
          <span className="text-sm text-text-primary font-mono">
            {attentionCount} item{attentionCount === 1 ? '' : 's'} need attention in Inbox
          </span>
          <Link to="/dashboard/activity" className="font-mono text-xs text-brand hover:underline whitespace-nowrap">
            open inbox →
          </Link>
        </div>
      )}

      {deepLinkResult && (
        <div className="dev-banner--info">
          <span className="text-brand text-sm font-mono">{deepLinkResult}</span>
          <button onClick={() => setDeepLinkResult(null)} className="dev-btn-ghost text-brand">dismiss</button>
        </div>
      )}

      <div className="flex flex-wrap items-center gap-3">
        <select
          value={filter}
          onChange={e => { setFilter(e.target.value); setOffset(0) }}
          className="ds-select !h-auto !py-1.5 !text-xs !w-auto"
        >
          {STATUS_FILTER_OPTIONS.map(o => (
            <option key={o.value} value={o.value}>{o.label}</option>
          ))}
        </select>
        {total > 0 && (
          <span className="font-mono text-xs text-text-tertiary">{total} total</span>
        )}
      </div>

      {isLoading && <div className="ds-page-loading">loading…</div>}

      {!isLoading && sorted.length === 0 && (
        <div className="dev-panel py-8 text-center ds-page-loading">
          {filter
            ? 'no tasks match this filter'
            : (
              <>
                tasks appear here as agents request permission.
                {(agentsData ?? []).length === 0 && (
                  <> <Link to="/dashboard/agents" className="text-brand hover:underline">connect an agent</Link> to get started.</>
                )}
              </>
            )}
        </div>
      )}

      <div className="space-y-3">
        {sorted.map(task => (
          <TaskCard
            key={task.id}
            task={task}
            agentName={agentMap.get(task.agent_id) ?? task.agent_id.slice(0, 8)}
            onRevoke={orgId ? (tid) => api.orgs.revokeTask(orgId, tid) : undefined}
          />
        ))}
      </div>

      {/* Pagination */}
      {total > PAGE_SIZE && (
        <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-2 text-sm text-text-tertiary">
          <span>Showing {offset + 1}–{Math.min(offset + PAGE_SIZE, total)} of {total}</span>
          <div className="flex gap-2">
            <button
              disabled={offset === 0}
              onClick={() => setOffset(Math.max(0, offset - PAGE_SIZE))}
              className="px-3 py-1 rounded border border-border-strong disabled:opacity-40 hover:bg-surface-2"
            >
              Previous
            </button>
            <button
              disabled={offset + PAGE_SIZE >= total}
              onClick={() => setOffset(offset + PAGE_SIZE)}
              className="px-3 py-1 rounded border border-border-strong disabled:opacity-40 hover:bg-surface-2"
            >
              Next
            </button>
          </div>
        </div>
      )}
    </PageLayout>
  )
}
