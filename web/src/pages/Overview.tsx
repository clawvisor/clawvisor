import { useState, useEffect, useRef, useMemo } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { api, type QueueItem, type Task, type AuditEntry, type Agent, type NotificationConfig, type ActivityBucket } from '../api/client'
import { formatDistanceToNow, format } from 'date-fns'
import { AreaChart, Area, XAxis, YAxis, Tooltip, ResponsiveContainer } from 'recharts'
import { serviceName, actionName, serviceBrand, formatServiceAction } from '../lib/services'
import { summarizeActions, OUTCOME_STYLE } from '../lib/queue-helpers'
import CountdownTimer from '../components/CountdownTimer'
import StatusBadge from '../components/StatusBadge'
import LifetimeBadge from '../components/LifetimeBadge'
import Onboarding from './Onboarding'

export default function Overview() {
  // Bundled overview data — 5s polling for live feel
  const { data: overview } = useQuery({
    queryKey: ['overview'],
    queryFn: () => api.overview.get(),
    refetchInterval: 5_000,
  })

  // Agents for name resolution
  const { data: agentsData } = useQuery({
    queryKey: ['agents'],
    queryFn: () => api.agents.list(),
  })

  // Services + notifications for onboarding
  const { data: services } = useQuery({
    queryKey: ['services'],
    queryFn: () => api.services.list(),
  })
  const { data: notificationsData } = useQuery({
    queryKey: ['notifications'],
    queryFn: (): Promise<NotificationConfig[]> => api.notifications.list(),
  })

  const agentMap = useMemo(() => {
    const m = new Map<string, string>()
    for (const a of (agentsData ?? []) as Agent[]) {
      m.set(a.id, a.name)
    }
    return m
  }, [agentsData])

  const allServices = services?.services ?? []

  // Onboarding latch
  const onboardingDecided = useRef(false)
  const [showOnboarding, setShowOnboarding] = useState(false)
  const [onboardingInitial, setOnboardingInitial] = useState<number[]>([])

  useEffect(() => {
    if (onboardingDecided.current) return
    if (!services || agentsData === undefined || notificationsData === undefined) return
    onboardingDecided.current = true
    const hasService = (services.services ?? []).some(
      (s: { status: string; requires_activation?: boolean }) => s.status === 'activated' && (s.requires_activation ?? true)
    )
    const hasAgents = (agentsData ?? []).length > 0
    const hasTelegram = notificationsData.some((c: NotificationConfig) => c.channel === 'telegram' && c.config?.bot_token)
    const done: number[] = []
    if (hasService) done.push(1)
    if (hasAgents) done.push(2)
    if (hasTelegram) done.push(3)
    if (done.length < 3) {
      setOnboardingInitial(done)
      setShowOnboarding(true)
    }
  }, [services, agentsData, notificationsData])

  const queueItems = overview?.queue ?? []
  const activeTasks = overview?.active_tasks ?? []
  const activity = overview?.activity ?? []

  return (
    <div className="p-8 space-y-8">
      <h1 className="text-2xl font-bold text-gray-900">Dashboard</h1>

      {/* Onboarding */}
      {showOnboarding && (
        <Onboarding
          allServices={allServices}
          initialCompleted={onboardingInitial}
          onDismiss={() => setShowOnboarding(false)}
        />
      )}

      {/* Queue section */}
      <section>
        <div className="flex items-center gap-3 mb-3">
          <h2 className="text-lg font-semibold text-gray-800">Needs your attention</h2>
          {queueItems.length > 0 && (
            <span className="bg-orange-500 text-white text-xs font-bold rounded-full px-2.5 py-0.5">
              {queueItems.length}
            </span>
          )}
        </div>

        {queueItems.length === 0 ? (
          <div className="rounded-lg border border-green-200 bg-green-50 px-5 py-4 flex items-center gap-3">
            <svg className="w-5 h-5 text-green-600 shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
              <path d="M22 11.08V12a10 10 0 1 1-5.93-9.14" />
              <polyline points="22 4 12 14.01 9 11.01" />
            </svg>
            <span className="text-green-800 font-medium">All clear — nothing needs your attention</span>
          </div>
        ) : (
          <div className="space-y-3">
            {queueItems.map(item =>
              item.type === 'approval' ? (
                <OverviewApprovalCard key={item.id} item={item} />
              ) : (
                <OverviewTaskCard
                  key={item.id}
                  item={item}
                  agentName={item.task ? (agentMap.get(item.task.agent_id) ?? item.task.agent_id.slice(0, 8)) : 'Unknown'}
                />
              )
            )}
          </div>
        )}
      </section>

      {/* Activity graph */}
      <section>
        <h2 className="text-lg font-semibold text-gray-800 mb-3">Activity (last 60 min)</h2>
        {activity.length === 0 ? (
          <div className="rounded-lg border border-gray-200 bg-gray-50 px-5 py-8 text-center text-sm text-gray-400">
            No activity in the last 60 minutes
          </div>
        ) : (
          <ActivityChart data={activity} />
        )}
      </section>

      {/* Active tasks */}
      {activeTasks.length > 0 && (
        <section>
          <div className="flex items-center justify-between mb-3">
            <h2 className="text-lg font-semibold text-gray-800">
              Active tasks
              <span className="ml-2 text-sm font-normal text-gray-400">{activeTasks.length}</span>
            </h2>
            <Link to="/dashboard/tasks" className="text-sm text-blue-600 hover:underline">
              View all
            </Link>
          </div>
          <div className="space-y-3">
            {activeTasks.slice(0, 5).map(task => (
              <OverviewActiveTaskCard
                key={task.id}
                task={task}
                agentName={agentMap.get(task.agent_id) ?? task.agent_id.slice(0, 8)}
              />
            ))}
            {activeTasks.length > 5 && (
              <Link to="/dashboard/tasks" className="block text-center text-sm text-blue-600 hover:underline py-1">
                +{activeTasks.length - 5} more
              </Link>
            )}
          </div>
        </section>
      )}
    </div>
  )
}

// ── Activity chart ───────────────────────────────────────────────────────────

const OUTCOME_COLORS: Record<string, string> = {
  executed: '#22c55e',
  blocked: '#ef4444',
  pending: '#f97316',
}

interface ChartRow {
  time: string
  executed: number
  blocked: number
  pending: number
}

function ActivityChart({ data }: { data: ActivityBucket[] }) {
  const rows = useMemo(() => {
    const map = new Map<string, ChartRow>()
    for (const b of data) {
      const t = new Date(b.bucket)
      const key = t.toISOString()
      const label = `${String(t.getHours()).padStart(2, '0')}:${String(t.getMinutes()).padStart(2, '0')}`
      if (!map.has(key)) {
        map.set(key, { time: label, executed: 0, blocked: 0, pending: 0 })
      }
      const row = map.get(key)!
      if (b.outcome === 'executed') row.executed += b.count
      else if (b.outcome === 'blocked' || b.outcome === 'restricted') row.blocked += b.count
      else row.pending += b.count
    }
    return Array.from(map.values())
  }, [data])

  return (
    <div className="bg-white border rounded-lg p-4">
      <ResponsiveContainer width="100%" height={180}>
        <AreaChart data={rows}>
          <XAxis dataKey="time" tick={{ fontSize: 11 }} interval="preserveStartEnd" />
          <YAxis allowDecimals={false} tick={{ fontSize: 11 }} width={30} />
          <Tooltip
            contentStyle={{ fontSize: 12, borderRadius: 8, border: '1px solid #e5e7eb' }}
          />
          <Area type="monotone" dataKey="executed" stackId="1" stroke={OUTCOME_COLORS.executed} fill={OUTCOME_COLORS.executed} fillOpacity={0.3} name="Executed" />
          <Area type="monotone" dataKey="blocked" stackId="1" stroke={OUTCOME_COLORS.blocked} fill={OUTCOME_COLORS.blocked} fillOpacity={0.3} name="Blocked" />
          <Area type="monotone" dataKey="pending" stackId="1" stroke={OUTCOME_COLORS.pending} fill={OUTCOME_COLORS.pending} fillOpacity={0.3} name="Pending" />
        </AreaChart>
      </ResponsiveContainer>
    </div>
  )
}

// ── Active task card (mirrors Tasks page TaskCard) ───────────────────────────

function OverviewActiveTaskCard({ task, agentName }: { task: Task; agentName: string }) {
  const qc = useQueryClient()
  const [result, setResult] = useState<string | null>(null)
  const [expanded, setExpanded] = useState(false)
  const isStanding = task.lifetime === 'standing'

  const revokeMut = useMutation({
    mutationFn: () => api.tasks.revoke(task.id),
    onSuccess: () => {
      setResult('Revoked')
      qc.invalidateQueries({ queryKey: ['overview'] })
      qc.invalidateQueries({ queryKey: ['tasks'] })
    },
  })

  const { data: auditData, isLoading: auditLoading } = useQuery({
    queryKey: ['audit', { task_id: task.id }],
    queryFn: () => api.audit.list({ task_id: task.id, limit: 50 }),
    enabled: expanded,
  })

  const auditEntries = auditData?.entries ?? []

  return (
    <div className="border rounded-lg bg-white">
      {/* Clickable header */}
      <div
        className="p-5 space-y-3 cursor-pointer"
        onClick={() => setExpanded(e => !e)}
      >
        <div className="flex items-start justify-between gap-3">
          <div className="min-w-0 flex-1">
            <p className="text-base font-semibold text-gray-900">{task.purpose}</p>
            <div className="flex items-center gap-2 mt-1.5">
              <StatusBadge status={task.status} />
              <LifetimeBadge lifetime={task.lifetime} />
              <span className="text-xs text-gray-400">
                {agentName} &middot; {formatDistanceToNow(new Date(task.created_at), { addSuffix: true })}
              </span>
            </div>
          </div>
          <div className="text-right shrink-0 space-y-1">
            {!isStanding && task.expires_at && (
              <CountdownTimer expiresAt={task.expires_at} showLabel />
            )}
            {isStanding && (
              <span className="text-xs text-purple-600 font-medium">No expiry</span>
            )}
            <div className="flex items-center gap-2 justify-end">
              {task.request_count > 0 && (
                <span className="text-xs text-gray-400">{task.request_count} request{task.request_count !== 1 ? 's' : ''}</span>
              )}
              <span className="text-xs text-gray-300">{expanded ? '\u25B2' : '\u25BC'}</span>
            </div>
          </div>
        </div>

        <p className="text-sm text-gray-600">{summarizeActions(task.authorized_actions)}</p>
      </div>

      {/* Result message */}
      {result && (
        <div className="px-5 pb-3">
          <div className="p-2 bg-gray-50 rounded text-sm text-gray-500">{result}</div>
        </div>
      )}

      {/* Revoke button */}
      {!result && task.status === 'active' && (
        <div className="flex gap-2 px-5 pb-4">
          <button
            onClick={() => revokeMut.mutate()}
            disabled={revokeMut.isPending}
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
                    <OverviewAuditRow key={e.id} entry={e} />
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

function OverviewAuditRow({ entry }: { entry: AuditEntry }) {
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
                    <div className="text-gray-400 text-[10px]">{entry.verification.model} &middot; {entry.verification.latency_ms}ms</div>
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

// ── Queue cards (local, mutation wiring differs from Queue page) ─────────────

function OverviewApprovalCard({ item }: { item: QueueItem }) {
  const qc = useQueryClient()
  const [result, setResult] = useState<string | null>(null)
  const [paramsOpen, setParamsOpen] = useState(false)
  const a = item.approval!

  const approveMut = useMutation({
    mutationFn: () => api.approvals.approve(a.request_id),
    onSuccess: (res) => {
      setResult(res.status === 'executed' ? 'Approved & executed' : `Outcome: ${res.status}`)
      qc.invalidateQueries({ queryKey: ['overview'] })
      qc.invalidateQueries({ queryKey: ['queue'] })
    },
  })

  const denyMut = useMutation({
    mutationFn: () => api.approvals.deny(a.request_id),
    onSuccess: () => {
      setResult('Denied')
      qc.invalidateQueries({ queryKey: ['overview'] })
      qc.invalidateQueries({ queryKey: ['queue'] })
    },
  })

  const isPending = approveMut.isPending || denyMut.isPending
  const hasParams = Object.keys(a.params ?? {}).length > 0

  if (result) {
    return (
      <div className="border rounded-lg bg-white p-5">
        <div className="p-3 bg-gray-50 rounded text-sm text-gray-500">{result}</div>
      </div>
    )
  }

  return (
    <div className="border rounded-lg bg-white p-5 space-y-3">
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0 flex-1">
          <p className="text-base font-semibold text-gray-900">
            {serviceName(a.service)} &middot; {actionName(a.action)}
          </p>
          {a.reason && (
            <p className="text-sm text-gray-500 italic mt-1">"{a.reason}"</p>
          )}
        </div>
        <div className="flex items-center gap-2 shrink-0">
          {item.expires_at && <CountdownTimer expiresAt={item.expires_at} />}
          <span className="text-xs px-2 py-0.5 rounded-full bg-blue-100 text-blue-700 font-medium">Request</span>
        </div>
      </div>

      {hasParams && (
        <div>
          <button
            onClick={() => setParamsOpen(o => !o)}
            className="text-xs text-gray-500 hover:text-gray-700 flex items-center gap-1"
          >
            <span>{paramsOpen ? '\u25BC' : '\u25B6'}</span> Parameters
          </button>
          {paramsOpen && (
            <pre className="mt-1 text-xs bg-gray-50 border rounded p-3 overflow-auto max-h-64 font-mono text-gray-700">
              {JSON.stringify(a.params, null, 2)}
            </pre>
          )}
        </div>
      )}

      <div className="flex gap-2">
        <button
          onClick={() => approveMut.mutate()}
          disabled={isPending}
          className="flex-1 py-2 text-sm rounded bg-green-600 text-white hover:bg-green-700 disabled:opacity-50 font-medium"
        >
          {approveMut.isPending ? 'Approving...' : 'Approve'}
        </button>
        <button
          onClick={() => denyMut.mutate()}
          disabled={isPending}
          className="flex-1 py-2 text-sm rounded bg-red-100 text-red-700 hover:bg-red-200 disabled:opacity-50 font-medium"
        >
          Deny
        </button>
      </div>
    </div>
  )
}

function OverviewTaskCard({ item, agentName }: { item: QueueItem; agentName: string }) {
  const qc = useQueryClient()
  const [result, setResult] = useState<string | null>(null)
  const task = item.task!
  const isExpansion = task.status === 'pending_scope_expansion'
  const needsApproval = task.status === 'pending_approval'

  const approveMut = useMutation({
    mutationFn: () => isExpansion ? api.tasks.expandApprove(task.id) : api.tasks.approve(task.id),
    onSuccess: () => {
      setResult(isExpansion ? 'Expansion approved' : 'Approved')
      qc.invalidateQueries({ queryKey: ['overview'] })
      qc.invalidateQueries({ queryKey: ['queue'] })
      qc.invalidateQueries({ queryKey: ['tasks'] })
    },
  })

  const denyMut = useMutation({
    mutationFn: () => isExpansion ? api.tasks.expandDeny(task.id) : api.tasks.deny(task.id),
    onSuccess: () => {
      setResult(isExpansion ? 'Expansion denied' : 'Denied')
      qc.invalidateQueries({ queryKey: ['overview'] })
      qc.invalidateQueries({ queryKey: ['queue'] })
      qc.invalidateQueries({ queryKey: ['tasks'] })
    },
  })

  const isPending = approveMut.isPending || denyMut.isPending

  if (result) {
    return (
      <div className="border rounded-lg bg-white p-5">
        <div className="p-3 bg-gray-50 rounded text-sm text-gray-500">{result}</div>
      </div>
    )
  }

  return (
    <div className="border border-orange-200 rounded-lg bg-white p-5 space-y-3">
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0 flex-1">
          <p className="text-base font-semibold text-gray-900">{task.purpose}</p>
          <div className="flex items-center gap-2 mt-1.5">
            <StatusBadge status={task.status} />
            <LifetimeBadge lifetime={task.lifetime} />
            <span className="text-xs text-gray-400">
              {agentName} &middot; {formatDistanceToNow(new Date(task.created_at), { addSuffix: true })}
            </span>
          </div>
        </div>
        <span className={`text-xs px-2 py-0.5 rounded-full font-medium shrink-0 ${
          isExpansion ? 'bg-orange-100 text-orange-700' : 'bg-purple-100 text-purple-700'
        }`}>
          {isExpansion ? 'Scope expansion' : 'New task'}
        </span>
      </div>

      <p className="text-sm text-gray-600">{summarizeActions(task.authorized_actions)}</p>

      {isExpansion && task.pending_action && (
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

      {needsApproval && task.authorized_actions.some(a => a.expected_use) && (
        <div className="space-y-1">
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

      <div className="flex gap-2">
        <button
          onClick={() => approveMut.mutate()}
          disabled={isPending}
          className="flex-1 py-2 text-sm rounded bg-green-600 text-white hover:bg-green-700 disabled:opacity-50 font-medium"
        >
          {approveMut.isPending ? 'Approving...' : isExpansion ? 'Approve Expansion' : 'Approve Task'}
        </button>
        <button
          onClick={() => denyMut.mutate()}
          disabled={isPending}
          className="flex-1 py-2 text-sm rounded bg-red-100 text-red-700 hover:bg-red-200 disabled:opacity-50 font-medium"
        >
          {isExpansion ? 'Deny Expansion' : 'Deny'}
        </button>
      </div>
    </div>
  )
}
