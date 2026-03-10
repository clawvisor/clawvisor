import { useState, useEffect, useRef, useMemo } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { Link, useSearchParams } from 'react-router-dom'
import { api, type QueueItem, type Agent, type NotificationConfig, type ActivityBucket } from '../api/client'
import { BarChart, Bar, XAxis, YAxis, Tooltip, ResponsiveContainer } from 'recharts'
import { serviceName, actionName } from '../lib/services'
import CountdownTimer from '../components/CountdownTimer'
import TaskCard from '../components/TaskCard'
import Onboarding from './Onboarding'

export default function Overview() {
  const qc = useQueryClient()
  const [searchParams, setSearchParams] = useSearchParams()
  const [deepLinkResult, setDeepLinkResult] = useState<string | null>(null)

  // Deep link mutations for approval requests (moved from Queue)
  const deepApproveRequest = useMutation({
    mutationFn: (requestId: string) => api.approvals.approve(requestId),
    onSuccess: (_data, requestId) => {
      setDeepLinkResult(`Request ${requestId.slice(0, 8)}... approved.`)
      qc.invalidateQueries({ queryKey: ['overview'] })
    },
    onError: (err: Error) => setDeepLinkResult(`Approve failed: ${err.message}`),
  })
  const deepDenyRequest = useMutation({
    mutationFn: (requestId: string) => api.approvals.deny(requestId),
    onSuccess: (_data, requestId) => {
      setDeepLinkResult(`Request ${requestId.slice(0, 8)}... denied.`)
      qc.invalidateQueries({ queryKey: ['overview'] })
    },
    onError: (err: Error) => setDeepLinkResult(`Deny failed: ${err.message}`),
  })

  // Handle deep link actions for approvals
  useEffect(() => {
    const action = searchParams.get('action')
    const requestId = searchParams.get('request_id')
    if (!action || !requestId) return

    setSearchParams({}, { replace: true })

    if (action === 'approve') deepApproveRequest.mutate(requestId)
    else if (action === 'deny') deepDenyRequest.mutate(requestId)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // Bundled overview data (fallback polling; SSE pushes invalidations)
  const { data: overview } = useQuery({
    queryKey: ['overview'],
    queryFn: () => api.overview.get(),
    refetchInterval: 30_000,
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
    if (!hasService || !hasAgents) {
      setOnboardingInitial(done)
      setShowOnboarding(true)
    }
  }, [services, agentsData, notificationsData])

  const queueItems = overview?.queue ?? []
  const activeTasks = overview?.active_tasks ?? []
  const activity = overview?.activity ?? []

  return (
    <div className="p-8 space-y-8">
      <h1 className="text-2xl font-bold text-text-primary">Dashboard</h1>

      {/* Deep link result banner */}
      {deepLinkResult && (
        <div className="rounded-md border border-brand/30 bg-brand/10 px-5 py-3 flex items-center justify-between">
          <span className="text-brand text-sm">{deepLinkResult}</span>
          <button onClick={() => setDeepLinkResult(null)} className="text-brand text-xs hover:underline">Dismiss</button>
        </div>
      )}

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
          <h2 className="text-lg font-semibold text-text-primary">Needs your attention</h2>
          {queueItems.length > 0 && (
            <span className="bg-warning text-surface-0 text-xs font-bold rounded px-2.5 py-0.5 font-mono">
              {queueItems.length}
            </span>
          )}
        </div>

        {queueItems.length === 0 ? (
          <div className="rounded-md border border-success/30 bg-success/10 px-5 py-4 flex items-center gap-3">
            <svg className="w-5 h-5 text-success shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
              <path d="M22 11.08V12a10 10 0 1 1-5.93-9.14" />
              <polyline points="22 4 12 14.01 9 11.01" />
            </svg>
            <span className="text-success font-medium">All clear — nothing needs your attention</span>
          </div>
        ) : (
          <div className="space-y-3">
            {queueItems.map(item =>
              item.type === 'approval' ? (
                <ApprovalCard key={item.id} item={item} />
              ) : item.task ? (
                <TaskCard
                  key={item.id}
                  task={item.task}
                  agentName={agentMap.get(item.task.agent_id) ?? item.task.agent_id.slice(0, 8)}
                />
              ) : null
            )}
          </div>
        )}
      </section>

      {/* Activity graph */}
      <section>
        <h2 className="text-lg font-semibold text-text-primary mb-3">Activity (last 60 min)</h2>
        {activity.length === 0 ? (
          <div className="rounded-md border border-border-subtle bg-surface-1 px-5 py-8 text-center text-sm text-text-tertiary">
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
            <h2 className="text-lg font-semibold text-text-primary">
              Active tasks
              <span className="ml-2 text-sm font-normal text-text-tertiary">{activeTasks.length}</span>
            </h2>
            <Link to="/dashboard/tasks" className="text-sm text-brand hover:underline">
              View all
            </Link>
          </div>
          <div className="space-y-3">
            {activeTasks.slice(0, 5).map(task => (
              <TaskCard
                key={task.id}
                task={task}
                agentName={agentMap.get(task.agent_id) ?? task.agent_id.slice(0, 8)}
              />
            ))}
            {activeTasks.length > 5 && (
              <Link to="/dashboard/tasks" className="block text-center text-sm text-brand hover:underline py-1">
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
    // Build lookup from bucket timestamp → aggregated counts
    const counts = new Map<number, ChartRow>()
    for (const b of data) {
      const ms = new Date(b.bucket).getTime()
      if (!counts.has(ms)) {
        counts.set(ms, { time: '', executed: 0, blocked: 0, pending: 0 })
      }
      const row = counts.get(ms)!
      if (b.outcome === 'executed') row.executed += b.count
      else if (b.outcome === 'blocked' || b.outcome === 'restricted') row.blocked += b.count
      else row.pending += b.count
    }

    // Generate all 12 five-minute buckets covering the last hour
    const now = new Date()
    const startMs = now.getTime() - 60 * 60 * 1000
    const firstBucket = startMs - (startMs % (5 * 60 * 1000))
    const result: ChartRow[] = []
    for (let ms = firstBucket; ms <= now.getTime(); ms += 5 * 60 * 1000) {
      const t = new Date(ms)
      const label = `${String(t.getHours()).padStart(2, '0')}:${String(t.getMinutes()).padStart(2, '0')}`
      const existing = counts.get(ms)
      result.push(existing ? { ...existing, time: label } : { time: label, executed: 0, blocked: 0, pending: 0 })
    }
    return result
  }, [data])

  return (
    <div className="bg-surface-1 border border-border-default rounded-md p-4">
      <ResponsiveContainer width="100%" height={180}>
        <BarChart data={rows}>
          <XAxis dataKey="time" tick={{ fontSize: 11, fill: '#6b7280' }} interval="preserveStartEnd" />
          <YAxis allowDecimals={false} tick={{ fontSize: 11, fill: '#6b7280' }} width={30} />
          <Tooltip
            contentStyle={{ fontSize: 12, borderRadius: 6, border: '1px solid #1f2937', backgroundColor: '#161923', color: '#f0f1f3' }}
          />
          <Bar dataKey="executed" stackId="1" stroke={OUTCOME_COLORS.executed} fill={OUTCOME_COLORS.executed} fillOpacity={0.85} name="Executed" />
          <Bar dataKey="blocked" stackId="1" stroke={OUTCOME_COLORS.blocked} fill={OUTCOME_COLORS.blocked} fillOpacity={0.85} name="Blocked" />
          <Bar dataKey="pending" stackId="1" stroke={OUTCOME_COLORS.pending} fill={OUTCOME_COLORS.pending} fillOpacity={0.85} name="Pending" />
        </BarChart>
      </ResponsiveContainer>
    </div>
  )
}

// ── Approval card (standalone request approvals) ─────────────────────────────

function ApprovalCard({ item }: { item: QueueItem }) {
  const qc = useQueryClient()
  const [result, setResult] = useState<string | null>(null)
  const a = item.approval!

  const approveMut = useMutation({
    mutationFn: () => api.approvals.approve(a.request_id),
    onSuccess: (res) => {
      setResult(res.status === 'executed' ? 'Approved & executed' : `Outcome: ${res.status}`)
      qc.invalidateQueries({ queryKey: ['overview'] })
    },
  })

  const denyMut = useMutation({
    mutationFn: () => api.approvals.deny(a.request_id),
    onSuccess: () => {
      setResult('Denied')
      qc.invalidateQueries({ queryKey: ['overview'] })
    },
  })

  const isPending = approveMut.isPending || denyMut.isPending
  const params = a.params ?? {}
  const paramEntries = Object.entries(params)

  if (result) {
    return (
      <div className="border border-border-default rounded-md bg-surface-1 p-5">
        <div className="p-3 bg-surface-2 rounded text-sm text-text-tertiary">{result}</div>
      </div>
    )
  }

  return (
    <div className="bg-surface-1 border border-border-default rounded-md border-l-[3px] border-l-warning overflow-hidden">
      {/* Header */}
      <div className="px-5 pt-5 pb-4">
        <span className="font-mono text-lg font-semibold text-text-primary">{serviceName(a.service)} · {actionName(a.action)}</span>
        {a.reason && (
          <p className="text-sm text-text-secondary mt-1.5">{a.reason}</p>
        )}
        <div className="flex items-center gap-2 mt-2">
          <span className="inline-flex items-center gap-1.5 text-xs font-mono font-medium px-2 py-0.5 rounded bg-warning/15 text-warning">
            <span className="w-1.5 h-1.5 rounded-full bg-warning" />
            approval
          </span>
          {item.expires_at && <CountdownTimer expiresAt={item.expires_at} />}
        </div>
      </div>

      {/* Parameters */}
      {paramEntries.length > 0 && (
        <div className="px-5 pb-3">
          <div className="bg-surface-0 border border-border-subtle rounded overflow-hidden">
            <table className="w-full text-xs">
              <tbody>
                {paramEntries.map(([key, value], i) => (
                  <tr key={key} className={i < paramEntries.length - 1 ? 'border-b border-border-subtle' : ''}>
                    <td className="px-3 py-1.5 font-mono text-text-tertiary w-28 align-top">{key}</td>
                    <td className="px-3 py-1.5 font-mono text-text-primary break-all">
                      {typeof value === 'string' ? value : JSON.stringify(value)}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}

      {/* Actions */}
      <div className="px-4 py-3 border-t border-border-subtle flex items-center justify-end gap-2">
        <button
          onClick={() => denyMut.mutate()}
          disabled={isPending}
          className="rounded px-4 py-1.5 text-sm font-medium bg-danger/10 text-danger border border-danger/20 hover:bg-danger/20 disabled:opacity-50"
        >
          Deny
        </button>
        <button
          onClick={() => approveMut.mutate()}
          disabled={isPending}
          className="bg-brand text-surface-0 font-medium rounded px-5 py-1.5 text-sm hover:bg-brand-strong disabled:opacity-50"
        >
          {approveMut.isPending ? 'Approving...' : 'Approve'}
        </button>
      </div>
    </div>
  )
}
