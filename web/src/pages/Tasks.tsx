import { useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api, type Task, type TaskAction, type Agent } from '../api/client'
import { formatDistanceToNow, differenceInSeconds } from 'date-fns'

const STATUS_STYLES: Record<string, string> = {
  pending_approval: 'bg-orange-100 text-orange-800',
  pending_scope_expansion: 'bg-orange-100 text-orange-800',
  active: 'bg-green-100 text-green-800',
  completed: 'bg-gray-100 text-gray-600',
  expired: 'bg-gray-100 text-gray-500',
  denied: 'bg-red-100 text-red-700',
}

const STATUS_LABELS: Record<string, string> = {
  pending_approval: 'Pending Approval',
  pending_scope_expansion: 'Scope Expansion',
  active: 'Active',
  completed: 'Completed',
  expired: 'Expired',
  denied: 'Denied',
}

function StatusBadge({ status }: { status: string }) {
  return (
    <span className={`inline-block px-2 py-0.5 rounded-full text-xs font-medium ${STATUS_STYLES[status] ?? 'bg-gray-100 text-gray-600'}`}>
      {STATUS_LABELS[status] ?? status}
    </span>
  )
}

function ExpiryCountdown({ expiresAt }: { expiresAt: string }) {
  const secs = Math.max(0, differenceInSeconds(new Date(expiresAt), new Date()))
  if (secs <= 0) return <span className="text-xs text-red-500 font-medium">Expired</span>
  const mins = Math.floor(secs / 60)
  const s = secs % 60
  const urgent = secs < 120
  return (
    <span className={`font-mono text-xs tabular-nums ${urgent ? 'text-red-600' : 'text-gray-400'}`}>
      {mins}:{String(s).padStart(2, '0')} remaining
    </span>
  )
}

function ActionsList({ actions, highlight }: { actions: TaskAction[]; highlight?: TaskAction }) {
  return (
    <div className="flex flex-wrap gap-1.5">
      {actions.map((a, i) => {
        const isHighlight = highlight && a.service === highlight.service && a.action === highlight.action
        return (
          <span
            key={i}
            className={`inline-flex items-center gap-1 text-xs px-2 py-1 rounded border ${
              isHighlight
                ? 'bg-orange-50 border-orange-200 text-orange-700'
                : 'bg-gray-50 border-gray-200 text-gray-700'
            }`}
          >
            <span className="font-mono">{a.service}:{a.action}</span>
            {a.auto_execute && (
              <span className="text-green-600 font-medium" title="Auto-execute enabled">auto</span>
            )}
          </span>
        )
      })}
    </div>
  )
}

function TaskCard({ task, agentName }: { task: Task; agentName: string }) {
  const qc = useQueryClient()
  const [result, setResult] = useState<string | null>(null)

  const approveMut = useMutation({
    mutationFn: () => api.tasks.approve(task.id),
    onSuccess: () => {
      setResult('Approved')
      qc.invalidateQueries({ queryKey: ['tasks'] })
    },
  })

  const denyMut = useMutation({
    mutationFn: () => api.tasks.deny(task.id),
    onSuccess: () => {
      setResult('Denied')
      qc.invalidateQueries({ queryKey: ['tasks'] })
    },
  })

  const expandApproveMut = useMutation({
    mutationFn: () => api.tasks.expandApprove(task.id),
    onSuccess: () => {
      setResult('Expansion approved')
      qc.invalidateQueries({ queryKey: ['tasks'] })
    },
  })

  const expandDenyMut = useMutation({
    mutationFn: () => api.tasks.expandDeny(task.id),
    onSuccess: () => {
      setResult('Expansion denied')
      qc.invalidateQueries({ queryKey: ['tasks'] })
    },
  })

  const isPending = approveMut.isPending || denyMut.isPending || expandApproveMut.isPending || expandDenyMut.isPending
  const needsApproval = task.status === 'pending_approval'
  const needsExpansion = task.status === 'pending_scope_expansion'

  return (
    <div className={`border rounded-lg p-5 bg-white space-y-3 ${
      needsApproval || needsExpansion ? 'border-orange-200' : ''
    }`}>
      {/* Header */}
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-2 mb-1">
            <StatusBadge status={task.status} />
            <span className="text-xs text-gray-400">
              {formatDistanceToNow(new Date(task.created_at), { addSuffix: true })}
            </span>
          </div>
          <p className="text-sm font-medium text-gray-900">{task.purpose}</p>
          <p className="text-xs text-gray-500 mt-0.5">Agent: {agentName}</p>
        </div>
        <div className="text-right shrink-0 space-y-1">
          {task.status === 'active' && task.expires_at && (
            <ExpiryCountdown expiresAt={task.expires_at} />
          )}
          {task.request_count > 0 && (
            <div className="text-xs text-gray-400">{task.request_count} request{task.request_count !== 1 ? 's' : ''}</div>
          )}
        </div>
      </div>

      {/* Authorized actions */}
      <div>
        <div className="text-xs text-gray-500 mb-1">Authorized actions</div>
        <ActionsList actions={task.authorized_actions} highlight={task.pending_action ?? undefined} />
      </div>

      {/* Scope expansion detail */}
      {needsExpansion && task.pending_action && (
        <div className="bg-orange-50 border border-orange-200 rounded-lg p-3 space-y-2">
          <div className="text-xs font-medium text-orange-800">Scope expansion requested</div>
          <div className="flex items-center gap-2">
            <span className="text-xs font-mono bg-white border border-orange-200 rounded px-2 py-0.5 text-orange-700">
              {task.pending_action.service}:{task.pending_action.action}
            </span>
            {task.pending_action.auto_execute && (
              <span className="text-xs text-green-600 font-medium">auto-execute</span>
            )}
          </div>
          {task.pending_reason && (
            <p className="text-xs text-orange-700 italic">"{task.pending_reason}"</p>
          )}
        </div>
      )}

      {/* Result message */}
      {result && (
        <div className="p-2 bg-gray-50 rounded text-sm text-gray-500">{result}</div>
      )}

      {/* Action buttons */}
      {!result && needsApproval && (
        <div className="flex gap-2 pt-1">
          <button
            onClick={() => approveMut.mutate()}
            disabled={isPending}
            className="flex-1 py-1.5 text-sm rounded bg-green-600 text-white hover:bg-green-700 disabled:opacity-50"
          >
            {approveMut.isPending ? 'Approving...' : 'Approve Task'}
          </button>
          <button
            onClick={() => denyMut.mutate()}
            disabled={isPending}
            className="flex-1 py-1.5 text-sm rounded bg-red-100 text-red-700 hover:bg-red-200 disabled:opacity-50"
          >
            Deny
          </button>
        </div>
      )}

      {!result && needsExpansion && (
        <div className="flex gap-2 pt-1">
          <button
            onClick={() => expandApproveMut.mutate()}
            disabled={isPending}
            className="flex-1 py-1.5 text-sm rounded bg-green-600 text-white hover:bg-green-700 disabled:opacity-50"
          >
            {expandApproveMut.isPending ? 'Approving...' : 'Approve Expansion'}
          </button>
          <button
            onClick={() => expandDenyMut.mutate()}
            disabled={isPending}
            className="flex-1 py-1.5 text-sm rounded bg-red-100 text-red-700 hover:bg-red-200 disabled:opacity-50"
          >
            Deny Expansion
          </button>
        </div>
      )}
    </div>
  )
}

const STATUS_FILTER_OPTIONS = [
  { value: '', label: 'All tasks' },
  { value: 'actionable', label: 'Needs action' },
  { value: 'active', label: 'Active' },
  { value: 'completed', label: 'Completed' },
  { value: 'expired', label: 'Expired' },
  { value: 'denied', label: 'Denied' },
]

export default function Tasks() {
  const [filter, setFilter] = useState('')

  const { data, isLoading, refetch } = useQuery({
    queryKey: ['tasks'],
    queryFn: () => api.tasks.list(),
    refetchInterval: 10_000,
  })

  const { data: agentsData } = useQuery({
    queryKey: ['agents'],
    queryFn: () => api.agents.list(),
  })

  const agentMap = new Map<string, string>()
  for (const a of (agentsData ?? []) as Agent[]) {
    agentMap.set(a.id, a.name)
  }

  const allTasks = data?.tasks ?? []

  const filtered = allTasks.filter(t => {
    if (!filter) return true
    if (filter === 'actionable') return t.status === 'pending_approval' || t.status === 'pending_scope_expansion'
    return t.status === filter
  })

  // Sort: actionable first, then active, then by created_at desc
  const sorted = [...filtered].sort((a, b) => {
    const priority = (s: string) => {
      if (s === 'pending_approval' || s === 'pending_scope_expansion') return 0
      if (s === 'active') return 1
      return 2
    }
    const pa = priority(a.status), pb = priority(b.status)
    if (pa !== pb) return pa - pb
    return new Date(b.created_at).getTime() - new Date(a.created_at).getTime()
  })

  const actionableCount = allTasks.filter(
    t => t.status === 'pending_approval' || t.status === 'pending_scope_expansion'
  ).length

  return (
    <div className="p-8 space-y-4">
      <div className="flex items-center justify-between">
        <div className="flex items-center gap-3">
          <h1 className="text-2xl font-bold text-gray-900">Tasks</h1>
          {actionableCount > 0 && (
            <span className="bg-orange-500 text-white text-xs font-bold rounded-full px-2.5 py-0.5">
              {actionableCount} awaiting action
            </span>
          )}
        </div>
        <button
          onClick={() => refetch()}
          className="text-sm text-blue-600 hover:underline"
        >
          Refresh
        </button>
      </div>

      {/* Filters */}
      <div className="flex gap-3">
        <select
          value={filter}
          onChange={e => setFilter(e.target.value)}
          className="text-sm rounded border border-gray-300 px-2 py-1.5 focus:outline-none focus:ring-1 focus:ring-blue-400"
        >
          {STATUS_FILTER_OPTIONS.map(o => (
            <option key={o.value} value={o.value}>{o.label}</option>
          ))}
        </select>
      </div>

      {isLoading && <div className="text-sm text-gray-400">Loading...</div>}

      {!isLoading && sorted.length === 0 && (
        <div className="text-sm text-gray-400 py-8 text-center">
          {filter ? 'No tasks match this filter.' : 'No tasks yet. Tasks are created by agents using task-scoped authorization.'}
        </div>
      )}

      {/* Task list */}
      <div className="space-y-3">
        {sorted.map(task => (
          <TaskCard
            key={task.id}
            task={task}
            agentName={agentMap.get(task.agent_id) ?? task.agent_id.slice(0, 8)}
          />
        ))}
      </div>
    </div>
  )
}
