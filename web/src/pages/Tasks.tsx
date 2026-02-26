import { useState, useEffect } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { useSearchParams, Link } from 'react-router-dom'
import { api, type Task, type Agent, type AuditEntry } from '../api/client'
import { formatDistanceToNow, format } from 'date-fns'
import { serviceName, actionName, serviceBrand, formatServiceAction } from '../lib/services'
import { summarizeActions, OUTCOME_STYLE } from '../lib/queue-helpers'
import CountdownTimer from '../components/CountdownTimer'
import StatusBadge from '../components/StatusBadge'
import LifetimeBadge from '../components/LifetimeBadge'

function TaskCard({ task, agentName }: { task: Task; agentName: string }) {
  const qc = useQueryClient()
  const [result, setResult] = useState<string | null>(null)
  const [expanded, setExpanded] = useState(false)

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

  const revokeMut = useMutation({
    mutationFn: () => api.tasks.revoke(task.id),
    onSuccess: () => {
      setResult('Revoked')
      qc.invalidateQueries({ queryKey: ['tasks'] })
    },
  })

  const { data: auditData, isLoading: auditLoading } = useQuery({
    queryKey: ['audit', { task_id: task.id }],
    queryFn: () => api.audit.list({ task_id: task.id, limit: 50 }),
    enabled: expanded,
  })

  const isPending = approveMut.isPending || denyMut.isPending || expandApproveMut.isPending || expandDenyMut.isPending || revokeMut.isPending
  const needsApproval = task.status === 'pending_approval'
  const needsExpansion = task.status === 'pending_scope_expansion'
  const isStanding = task.lifetime === 'standing'

  const auditEntries = auditData?.entries ?? []

  return (
    <div className={`border rounded-lg bg-white ${
      needsApproval || needsExpansion ? 'border-orange-200' : ''
    }`}>
      {/* Clickable header */}
      <div
        className="p-5 space-y-3 cursor-pointer"
        onClick={() => setExpanded(e => !e)}
      >
      {/* Header — purpose as hero */}
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0 flex-1">
          <p className="text-base font-semibold text-gray-900">{task.purpose}</p>
          <div className="flex items-center gap-2 mt-1.5">
            <StatusBadge status={task.status} />
            <LifetimeBadge lifetime={task.lifetime} />
            <span className="text-xs text-gray-400">
              {agentName} · {formatDistanceToNow(new Date(task.created_at), { addSuffix: true })}
            </span>
          </div>
        </div>
        <div className="text-right shrink-0 space-y-1">
          {task.status === 'active' && !isStanding && task.expires_at && (
            <CountdownTimer expiresAt={task.expires_at} showLabel />
          )}
          {task.status === 'active' && isStanding && (
            <span className="text-xs text-purple-600 font-medium">No expiry</span>
          )}
          <div className="flex items-center gap-2 justify-end">
            {task.request_count > 0 && (
              <span className="text-xs text-gray-400">{task.request_count} request{task.request_count !== 1 ? 's' : ''}</span>
            )}
            <span className="text-xs text-gray-300">{expanded ? '▲' : '▼'}</span>
          </div>
        </div>
      </div>

      {/* Authorized actions — prose summary */}
      <p className="text-sm text-gray-600">{summarizeActions(task.authorized_actions)}</p>

      {/* Scope expansion detail */}
      {needsExpansion && task.pending_action && (
        <div className="bg-orange-50 border border-orange-200 rounded-lg p-3 space-y-2">
          <div className="text-xs font-medium text-orange-800">Scope expansion requested</div>
          <div className="flex items-center gap-2">
            <span className="text-xs bg-white border border-orange-200 rounded px-2 py-0.5 text-orange-700">
              {serviceName(task.pending_action.service)}: {actionName(task.pending_action.action)}
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
      </div>{/* end clickable area */}

      {/* Result message */}
      {result && (
        <div className="px-5 pb-3">
          <div className="p-2 bg-gray-50 rounded text-sm text-gray-500">{result}</div>
        </div>
      )}

      {/* Agent-declared expected use (read-only, shown when present) */}
      {!result && needsApproval && task.authorized_actions.some(a => a.expected_use) && (
        <div className="px-5 pb-2 space-y-1">
          <div className="text-xs font-medium text-gray-500">Agent-declared expected use:</div>
          {task.authorized_actions.filter(a => a.expected_use).map(a => (
            <div key={`${a.service}|${a.action}`} className="flex items-start gap-2 text-xs">
              <span className="text-gray-500 w-40 shrink-0 truncate" title={`${a.service}:${a.action}`}>
                {serviceName(a.service)}: {actionName(a.action)}
              </span>
              <span className="text-gray-700 italic">{a.expected_use}</span>
            </div>
          ))}
        </div>
      )}

      {/* Action buttons */}
      {!result && needsApproval && (
        <div className="flex gap-2 px-5 pb-4">
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
        <div className="flex gap-2 px-5 pb-4">
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

      {/* Revoke button for any active task */}
      {!result && task.status === 'active' && (
        <div className="flex gap-2 px-5 pb-4">
          <button
            onClick={() => revokeMut.mutate()}
            disabled={isPending}
            className="py-1.5 px-4 text-sm rounded bg-gray-100 text-gray-700 hover:bg-gray-200 disabled:opacity-50"
          >
            {revokeMut.isPending ? 'Revoking...' : 'Revoke'}
          </button>
        </div>
      )}

      {/* Expanded audit entries */}
      {expanded && (
        <div className="border-t px-5 py-4 space-y-2">
          <div className="text-xs font-medium text-gray-500 uppercase tracking-wide">Actions</div>
          {auditLoading && <div className="text-xs text-gray-400">Loading...</div>}
          {!auditLoading && auditEntries.length === 0 && (
            <div className="text-xs text-gray-400">No actions recorded yet.</div>
          )}
          {auditEntries.length > 0 && (
            <div className="bg-gray-50 border rounded-lg overflow-hidden">
              <table className="w-full">
                <thead className="bg-gray-100 text-xs text-gray-500 font-medium">
                  <tr>
                    <th className="px-3 py-1.5 text-left">Time</th>
                    <th className="px-3 py-1.5 text-left">Service</th>
                    <th className="px-3 py-1.5 text-left">Action</th>
                    <th className="px-3 py-1.5 text-left">Outcome</th>
                    <th className="px-3 py-1.5 text-left">Duration</th>
                  </tr>
                </thead>
                <tbody>
                  {auditEntries.map(e => (
                    <TaskAuditRow key={e.id} entry={e} />
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>
      )}
    </div>
  )
}

function TaskAuditRow({ entry }: { entry: AuditEntry }) {
  const [expanded, setExpanded] = useState(false)
  return (
    <>
      <tr
        className="border-t hover:bg-white cursor-pointer text-xs"
        onClick={ev => { ev.stopPropagation(); setExpanded(e => !e) }}
      >
        <td className="px-3 py-1.5 text-gray-400 whitespace-nowrap" title={format(new Date(entry.timestamp), 'PPpp')}>
          {formatDistanceToNow(new Date(entry.timestamp), { addSuffix: true })}
        </td>
        <td className="px-3 py-1.5">
          <span className="inline-flex items-center gap-1">
            <span className={`w-1.5 h-1.5 rounded-full shrink-0 ${serviceBrand(entry.service).dot}`} />
            {serviceName(entry.service)}
          </span>
        </td>
        <td className="px-3 py-1.5">{actionName(entry.action)}</td>
        <td className="px-3 py-1.5">
          <span className={`inline-block px-1.5 py-0.5 rounded-full text-xs font-medium ${OUTCOME_STYLE[entry.outcome] ?? 'bg-gray-100 text-gray-600'}`}>
            {entry.outcome}
          </span>
        </td>
        <td className="px-3 py-1.5 text-gray-400">{entry.duration_ms}ms</td>
      </tr>
      {expanded && (
        <tr className="border-t bg-white">
          <td colSpan={5} className="px-3 py-2">
            <div className="grid grid-cols-2 gap-3 text-xs">
              <div>
                <div className="text-gray-500 font-medium mb-1">
                  {formatServiceAction(entry.service, entry.action)}
                </div>
                <pre className="bg-gray-50 border rounded p-2 overflow-auto max-h-32 text-gray-700 text-[11px]">
                  {JSON.stringify(entry.params_safe, null, 2)}
                </pre>
              </div>
              <div className="space-y-1.5">
                {entry.reason && (
                  <div className="bg-blue-50 rounded p-1.5">
                    <div className="text-blue-600 font-medium">Reason</div>
                    <div className="text-gray-700">{entry.reason}</div>
                  </div>
                )}
                {entry.error_msg && (
                  <div><span className="text-gray-500">Error:</span> <span className="text-red-600">{entry.error_msg}</span></div>
                )}
                {entry.safety_flagged && (
                  <div className="text-orange-600">Safety flagged{entry.safety_reason ? `: ${entry.safety_reason}` : ''}</div>
                )}
                {entry.verification && (
                  <div className="bg-orange-50 rounded p-1.5 space-y-1">
                    <div className="text-orange-700 font-medium">Intent Verification</div>
                    <div className="flex gap-2 flex-wrap">
                      <span className={`px-1.5 py-0.5 rounded text-[10px] font-medium ${
                        entry.verification.param_scope === 'ok' ? 'bg-green-100 text-green-700'
                        : entry.verification.param_scope === 'violation' ? 'bg-red-100 text-red-700'
                        : 'bg-gray-100 text-gray-500'
                      }`}>params: {entry.verification.param_scope}</span>
                      <span className={`px-1.5 py-0.5 rounded text-[10px] font-medium ${
                        entry.verification.reason_coherence === 'ok' ? 'bg-green-100 text-green-700'
                        : entry.verification.reason_coherence === 'incoherent' ? 'bg-red-100 text-red-700'
                        : entry.verification.reason_coherence === 'insufficient' ? 'bg-yellow-100 text-yellow-700'
                        : 'bg-gray-100 text-gray-500'
                      }`}>reason: {entry.verification.reason_coherence}</span>
                      {entry.verification.cached && (
                        <span className="px-1.5 py-0.5 rounded text-[10px] font-medium bg-blue-100 text-blue-600">cached</span>
                      )}
                    </div>
                    <div className="text-gray-600 text-[11px]">{entry.verification.explanation}</div>
                    <div className="text-gray-400 text-[10px]">{entry.verification.model} · {entry.verification.latency_ms}ms</div>
                  </div>
                )}
              </div>
            </div>
          </td>
        </tr>
      )}
    </>
  )
}

const STATUS_FILTER_OPTIONS = [
  { value: '', label: 'All tasks' },
  { value: 'actionable', label: 'Needs action' },
  { value: 'active', label: 'Active' },
  { value: 'completed', label: 'Completed' },
  { value: 'expired', label: 'Expired' },
  { value: 'denied', label: 'Denied' },
  { value: 'revoked', label: 'Revoked' },
]

export default function Tasks() {
  const [filter, setFilter] = useState('')
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

  // Handle deep link actions from Telegram buttons
  useEffect(() => {
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

      {/* Deep link result banner */}
      {deepLinkResult && (
        <div className="rounded-lg border border-blue-200 bg-blue-50 px-5 py-3 flex items-center justify-between">
          <span className="text-blue-800 text-sm">{deepLinkResult}</span>
          <button onClick={() => setDeepLinkResult(null)} className="text-blue-500 text-xs hover:underline">Dismiss</button>
        </div>
      )}

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
          {filter
            ? 'No tasks match this filter.'
            : <>When your agent requests permission to run a task, it'll appear here for your approval.{(agentsData ?? []).length === 0 && (<>{' '}<Link to="/dashboard/agents" className="text-blue-600 hover:underline">Create an agent</Link> to get started.</>)}</>
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
          />
        ))}
      </div>
    </div>
  )
}
