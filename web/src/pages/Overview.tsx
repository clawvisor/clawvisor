import { useState, useEffect, useRef, useMemo, type ReactNode } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { Link, useSearchParams } from 'react-router-dom'
import { api, type QueueItem, type Agent, type NotificationConfig, type ActivityBucket, type VerificationVerdict, type ConnectionRequest } from '../api/client'
import { BarChart, Bar, XAxis, YAxis, Tooltip, ResponsiveContainer } from 'recharts'
import { serviceName, actionName } from '../lib/services'
import CountdownTimer from '../components/CountdownTimer'
import TaskCard from '../components/TaskCard'
import VerificationIcon from '../components/VerificationIcon'
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
              ) : item.type === 'connection' && item.connection ? (
                <ConnectionQueueCard key={item.id} connection={item.connection} />
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

interface ChartRow {
  time: string
  executed: number
  blocked: number
  pending: number
}

function useChartColors() {
  const style = getComputedStyle(document.documentElement)
  return useMemo(() => {
    const r = (name: string) => {
      const channels = style.getPropertyValue(name).trim()
      return `rgb(${channels.replace(/ /g, ', ')})`
    }
    return {
      executed: r('--color-success'),
      blocked: r('--color-danger'),
      pending: r('--color-warning'),
      axisTick: style.getPropertyValue('--color-axis-tick').trim(),
      tooltipBg: style.getPropertyValue('--color-tooltip-bg').trim(),
      tooltipBorder: style.getPropertyValue('--color-tooltip-border').trim(),
      tooltipText: style.getPropertyValue('--color-tooltip-text').trim(),
    }
    // Re-compute when dark class changes
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [document.documentElement.classList.contains('dark')])
}

function ActivityChart({ data }: { data: ActivityBucket[] }) {
  const colors = useChartColors()
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
          <XAxis dataKey="time" tick={{ fontSize: 11, fill: colors.axisTick }} interval="preserveStartEnd" />
          <YAxis allowDecimals={false} tick={{ fontSize: 11, fill: colors.axisTick }} width={30} />
          <Tooltip
            contentStyle={{ fontSize: 12, borderRadius: 6, border: `1px solid ${colors.tooltipBorder}`, backgroundColor: colors.tooltipBg, color: colors.tooltipText }}
          />
          <Bar dataKey="executed" stackId="1" stroke={colors.executed} fill={colors.executed} fillOpacity={0.85} name="Executed" />
          <Bar dataKey="blocked" stackId="1" stroke={colors.blocked} fill={colors.blocked} fillOpacity={0.85} name="Blocked" />
          <Bar dataKey="pending" stackId="1" stroke={colors.pending} fill={colors.pending} fillOpacity={0.85} name="Pending" />
        </BarChart>
      </ResponsiveContainer>
    </div>
  )
}

// ── Approval card (standalone request approvals) ─────────────────────────────

function ApprovalCard({ item }: { item: QueueItem }) {
  const qc = useQueryClient()
  const [result, setResult] = useState<string | null>(null)
  const [verifyOpen, setVerifyOpen] = useState(false)
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
  const hasIssue = a.verification ? hasVerificationIssue(a.verification) : false
  // Auto-expand when there's a problem
  const showPanel = a.verification && (hasIssue || verifyOpen)

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

      {/* Verification — auto-expanded for issues, collapsible toggle for clean */}
      {a.verification && !hasIssue && (
        <div className="px-5 pb-3">
          <button
            onClick={() => setVerifyOpen(o => !o)}
            className="flex items-center gap-1.5 text-xs text-text-tertiary hover:text-text-secondary"
          >
            <svg className={`w-3 h-3 transition-transform ${verifyOpen ? 'rotate-90' : ''}`} fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M9 5l7 7-7 7"/></svg>
            <span className="font-medium">Verification</span>
            <VerificationIcon result={a.verification.param_scope} type="param" />
            <VerificationIcon result={a.verification.reason_coherence} type="reason" />
          </button>
        </div>
      )}
      {showPanel && (
        <VerificationPanel verification={a.verification!} />
      )}

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

function hasVerificationIssue(v: VerificationVerdict): boolean {
  return v.param_scope !== 'ok' || v.reason_coherence !== 'ok'
}

const VERIFY_COLORS = {
  clean:   { bg: 'rgba(34, 197, 94, 0.04)', border: 'rgba(34, 197, 94, 0.15)', headerBorder: 'rgba(34, 197, 94, 0.10)', color: 'rgb(var(--color-success))' },
  warning: { bg: 'rgba(245, 158, 11, 0.05)', border: 'rgba(245, 158, 11, 0.2)', headerBorder: 'rgba(245, 158, 11, 0.12)', color: 'rgb(var(--color-warning))' },
  danger:  { bg: 'rgba(239, 68, 68, 0.06)', border: 'rgba(239, 68, 68, 0.25)', headerBorder: 'rgba(239, 68, 68, 0.15)', color: 'rgb(var(--color-danger))' },
}

function VerificationPanel({ verification: v }: { verification: VerificationVerdict }) {
  const isDanger = v.param_scope === 'violation' || v.reason_coherence === 'incoherent'
  const isClean = v.param_scope === 'ok' && v.reason_coherence === 'ok'
  const colors = isClean ? VERIFY_COLORS.clean : isDanger ? VERIFY_COLORS.danger : VERIFY_COLORS.warning

  let headerIcon: ReactNode
  let headerLabel: string
  if (isClean) {
    headerIcon = <svg className="w-3 h-3" style={{ color: colors.color }} fill="none" stroke="currentColor" strokeWidth="2.5" viewBox="0 0 24 24"><path d="M5 13l4 4L19 7"/></svg>
    headerLabel = 'Verification Passed'
  } else if (isDanger) {
    headerIcon = <svg className="w-3 h-3" style={{ color: colors.color }} fill="none" stroke="currentColor" strokeWidth="2.5" viewBox="0 0 24 24"><path d="M12 9v2m0 4h.01m-6.938 4h13.856c1.54 0 2.502-1.667 1.732-3L13.732 4c-.77-1.333-2.694-1.333-3.464 0L3.34 16c-.77 1.333.192 3 1.732 3z"/></svg>
    headerLabel = 'Verification Warning'
  } else {
    headerIcon = <svg className="w-3 h-3" style={{ color: colors.color }} fill="none" stroke="currentColor" strokeWidth="2.5" viewBox="0 0 24 24"><path d="M12 9v2m0 4h.01m-6.938 4h13.856c1.54 0 2.502-1.667 1.732-3L13.732 4c-.77-1.333-2.694-1.333-3.464 0L3.34 16c-.77 1.333.192 3 1.732 3z"/></svg>
    headerLabel = 'Verification Notice'
  }

  return (
    <div className="px-5 pb-3">
      <div className="rounded overflow-hidden" style={{ background: colors.bg, border: `1px solid ${colors.border}` }}>
        <div className="px-3 py-1.5 flex items-center gap-1.5" style={{ borderBottom: `1px solid ${colors.headerBorder}` }}>
          {headerIcon}
          <span className="text-[10px] font-medium uppercase tracking-wider" style={{ color: colors.color }}>
            {headerLabel}
          </span>
        </div>
        <div className="px-3 py-2.5 space-y-1.5">
          <div className="flex items-center gap-3">
            <span className={`text-[10px] font-mono font-medium ${
              v.param_scope === 'ok' ? 'text-success' : v.param_scope === 'violation' ? 'text-danger' : 'text-text-tertiary'
            }`}>params: {v.param_scope}</span>
            <span className={`text-[10px] font-mono font-medium ${
              v.reason_coherence === 'ok' ? 'text-success'
              : v.reason_coherence === 'incoherent' ? 'text-danger'
              : v.reason_coherence === 'insufficient' ? 'text-warning'
              : 'text-text-tertiary'
            }`}>reason: {v.reason_coherence}</span>
          </div>
          {v.explanation && <p className="text-xs text-text-secondary">{v.explanation}</p>}
          <div className="text-[10px] font-mono text-text-tertiary">{v.model} &middot; {v.latency_ms}ms</div>
        </div>
      </div>
    </div>
  )
}

// ── Connection request card (overview queue) ──────────────────────────────────

function ConnectionQueueCard({ connection: cr }: { connection: ConnectionRequest }) {
  const qc = useQueryClient()
  const [result, setResult] = useState<string | null>(null)

  const approveMut = useMutation({
    mutationFn: () => api.connections.approve(cr.id),
    onSuccess: () => {
      setResult('Approved')
      qc.invalidateQueries({ queryKey: ['overview'] })
      qc.invalidateQueries({ queryKey: ['queue'] })
      qc.invalidateQueries({ queryKey: ['agents'] })
    },
    onError: (err: Error) => setResult(`Failed: ${err.message}`),
  })

  const denyMut = useMutation({
    mutationFn: () => api.connections.deny(cr.id),
    onSuccess: () => {
      setResult('Denied')
      qc.invalidateQueries({ queryKey: ['overview'] })
      qc.invalidateQueries({ queryKey: ['queue'] })
    },
    onError: (err: Error) => setResult(`Failed: ${err.message}`),
  })

  const isPending = approveMut.isPending || denyMut.isPending

  if (result) {
    return (
      <div className="border border-border-default rounded-md bg-surface-1 p-5">
        <div className="p-3 bg-surface-2 rounded text-sm text-text-tertiary">{result}</div>
      </div>
    )
  }

  return (
    <div className="bg-surface-1 border border-border-default rounded-md border-l-[3px] border-l-brand overflow-hidden">
      <div className="px-5 pt-5 pb-4">
        <span className="font-mono text-lg font-semibold text-text-primary">{cr.name}</span>
        {cr.description && (
          <p className="text-sm text-text-secondary mt-1.5">{cr.description}</p>
        )}
        <div className="flex items-center gap-2 mt-2">
          <span className="inline-flex items-center gap-1.5 text-xs font-mono font-medium px-2 py-0.5 rounded bg-brand/15 text-brand">
            <span className="w-1.5 h-1.5 rounded-full bg-brand" />
            agent connection
          </span>
          <span className="text-xs text-text-tertiary">IP: <code className="font-mono">{cr.ip_address}</code></span>
          {cr.expires_at && <CountdownTimer expiresAt={cr.expires_at} />}
        </div>
      </div>

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
