import { useState, useEffect } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { useSearchParams, Link } from 'react-router-dom'
import { api, type Agent } from '../api/client'
import { useAuth } from '../hooks/useAuth'
import TaskCard from '../components/TaskCard'

const STATUS_FILTER_OPTIONS = [
  { value: '', label: 'All tasks' },
  { value: 'actionable', label: 'Needs action' },
  { value: 'active', label: 'Active' },
  { value: 'completed', label: 'Completed' },
  { value: 'expired', label: 'Expired' },
  { value: 'denied', label: 'Denied' },
  { value: 'revoked', label: 'Revoked' },
]

const PAGE_SIZE = 25

export default function Tasks() {
  const { currentOrg } = useAuth()
  const orgId = currentOrg?.id
  const [filter, setFilter] = useState('')
  const [offset, setOffset] = useState(0)
  const [searchParams, setSearchParams] = useSearchParams()
  const qc = useQueryClient()
  const [deepLinkResult, setDeepLinkResult] = useState<string | null>(null)

  const deepApprove = useMutation({
    mutationFn: (taskId: string) => api.tasks.approve(taskId),
    onSuccess: () => { setDeepLinkResult('Task approved.'); qc.invalidateQueries({ queryKey: ['tasks'] }) },
    onError: (err: Error) => setDeepLinkResult(`Approve failed: ${err.message}`),
  })
  const deepDeny = useMutation({
    mutationFn: (taskId: string) => api.tasks.deny(taskId),
    onSuccess: () => { setDeepLinkResult('Task denied.'); qc.invalidateQueries({ queryKey: ['tasks'] }) },
    onError: (err: Error) => setDeepLinkResult(`Deny failed: ${err.message}`),
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

  // Build query params for the API call.
  // "actionable" uses active_only (pending_approval + pending_scope_expansion + active),
  // then we filter client-side for just the pending statuses.
  // All other filter values map to exact status matches.
  const queryParams = (() => {
    const params: { status?: string; limit: number; offset: number } = { limit: PAGE_SIZE, offset }
    if (filter && filter !== 'actionable') {
      params.status = filter
    }
    return params
  })()

  const listFn = orgId
    ? (params: typeof queryParams) => api.orgs.tasks(orgId, params)
    : (params: typeof queryParams) => api.tasks.list(params)

  const { data, isLoading, refetch } = useQuery({
    queryKey: ['tasks', orgId ?? 'personal', { filter, offset }],
    queryFn: async () => {
      if (filter === 'actionable') {
        const [approvals, expansions] = await Promise.all([
          listFn({ status: 'pending_approval', limit: PAGE_SIZE, offset }),
          listFn({ status: 'pending_scope_expansion', limit: PAGE_SIZE, offset }),
        ])
        return {
          tasks: [...(approvals.tasks ?? []), ...(expansions.tasks ?? [])],
          total: (approvals.total ?? 0) + (expansions.total ?? 0),
        }
      }
      return listFn(queryParams)
    },
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

  // Sort: actionable first, then active, then by created_at desc
  const sorted = [...tasks].sort((a, b) => {
    const priority = (s: string) => {
      if (s === 'pending_approval' || s === 'pending_scope_expansion') return 0
      if (s === 'active') return 1
      return 2
    }
    const pa = priority(a.status), pb = priority(b.status)
    if (pa !== pb) return pa - pb
    return new Date(b.created_at).getTime() - new Date(a.created_at).getTime()
  })

  return (
    <div className="p-8 space-y-4">
      <div className="flex items-center justify-between">
        <div className="flex items-center gap-3">
          <h1 className="text-2xl font-bold text-text-primary">Tasks</h1>
          {total > 0 && (
            <span className="text-xs text-text-tertiary font-mono">
              {total} total
            </span>
          )}
        </div>
        <button
          onClick={() => refetch()}
          className="text-sm text-brand hover:underline"
        >
          Refresh
        </button>
      </div>

      {/* Deep link result banner */}
      {deepLinkResult && (
        <div className="rounded-md border border-brand/30 bg-brand/10 px-5 py-3 flex items-center justify-between">
          <span className="text-brand text-sm">{deepLinkResult}</span>
          <button onClick={() => setDeepLinkResult(null)} className="text-brand text-xs hover:underline">Dismiss</button>
        </div>
      )}

      {/* Filters */}
      <div className="flex gap-3">
        <select
          value={filter}
          onChange={e => { setFilter(e.target.value); setOffset(0) }}
          className="text-sm rounded border border-border-default bg-surface-0 text-text-primary px-2 py-1.5 focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand"
        >
          {STATUS_FILTER_OPTIONS.map(o => (
            <option key={o.value} value={o.value}>{o.label}</option>
          ))}
        </select>
      </div>

      {isLoading && <div className="text-sm text-text-tertiary">Loading...</div>}

      {!isLoading && sorted.length === 0 && (
        <div className="text-sm text-text-tertiary py-8 text-center">
          {filter
            ? 'No tasks match this filter.'
            : <>When your agent requests permission to run a task, it'll appear here for your approval.{(agentsData ?? []).length === 0 && (<>{' '}<Link to="/dashboard/agents" className="text-brand hover:underline">Create an agent</Link> to get started.</>)}</>
          }
        </div>
      )}

      {/* Task list */}
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
        <div className="flex items-center justify-between text-sm text-text-tertiary">
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
    </div>
  )
}
