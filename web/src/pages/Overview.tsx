import { useState, useEffect, useRef, useCallback, useMemo } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { api, type Task, type Agent, type ActivityBucket, type RuntimeStatus } from '../api/client'
import { isActiveRuntimeSession } from './Runtime'
import { useAuth } from '../hooks/useAuth'
import { useAttentionItems } from '../hooks/useAttentionItems'
import { BarChart, Bar, XAxis, YAxis, Tooltip, ResponsiveContainer } from 'recharts'
import TaskCard from '../components/TaskCard'
import PageLayout from '../components/layout/PageLayout'
export default function Overview() {
  const { features } = useAuth()
  const runtimeActivityUI = !!features?.runtime_activity
  const liveSessionsUI = !!features?.agent_live_sessions
  const { attentionCount } = useAttentionItems()

  const { data: overview } = useQuery({
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

  const activeTasks = overview?.active_tasks ?? []
  const activity = overview?.activity ?? []
  const activeRuntimeSessions = useMemo(
    () => (liveSessionsUI && runtimeStatus?.enabled
      ? (runtimeSessions?.entries ?? []).filter(isActiveRuntimeSession)
      : []),
    [runtimeSessions, liveSessionsUI, runtimeStatus?.enabled],
  )

  const prevActiveRef = useRef<Map<string, Task>>(new Map())
  const [recentlyCompleted, setRecentlyCompleted] = useState<Task[]>([])
  const timersRef = useRef<Map<string, ReturnType<typeof setTimeout>>>(new Map())

  const removeCompleted = useCallback((id: string) => {
    setRecentlyCompleted(prev => prev.filter(t => t.id !== id))
    timersRef.current.delete(id)
  }, [])

  useEffect(() => {
    const prevMap = prevActiveRef.current
    const currentIds = new Set(activeTasks.map(t => t.id))

    for (const [id, task] of prevMap) {
      if (!currentIds.has(id) && !timersRef.current.has(id)) {
        const completed = { ...task, status: 'completed' as const }
        setRecentlyCompleted(prev => [...prev, completed])
        timersRef.current.set(id, setTimeout(() => removeCompleted(id), 60_000))
      }
    }

    const nextMap = new Map<string, Task>()
    for (const t of activeTasks) nextMap.set(t.id, t)
    prevActiveRef.current = nextMap
  }, [activeTasks, removeCompleted])

  useEffect(() => {
    const timers = timersRef.current
    return () => { for (const t of timers.values()) clearTimeout(t) }
  }, [])

  return (
    <PageLayout title="Home">
      <section>
        <Link
          to="/dashboard/activity"
          className={`block dev-panel px-5 py-4 transition-colors hover:border-brand/40 ${
            attentionCount > 0
              ? 'border-warning/40 bg-warning/10'
              : 'border-success/30 bg-success/10'
          }`}
        >
          <div className="flex items-center justify-between gap-3">
            <div className="flex items-center gap-3 min-w-0">
              {attentionCount > 0 ? (
                <span className="dev-badge--count shrink-0">
                  {attentionCount}
                </span>
              ) : (
                <span className="dev-badge--success shrink-0">ok</span>
              )}
              <span className={`text-sm ${attentionCount > 0 ? 'text-text-primary' : 'text-success'}`}>
                {attentionCount > 0
                  ? `${attentionCount} item${attentionCount === 1 ? '' : 's'} need attention`
                  : 'queue clear'}
              </span>
            </div>
            <span className="ds-link text-xs shrink-0">open activity →</span>
          </div>
        </Link>
      </section>

      {runtimeActivityUI && runtimeStatus?.enabled && (
        <RuntimePolicyCard status={runtimeStatus} activeSessionCount={activeRuntimeSessions.length} />
      )}

      <section>
        <h2 className="page-section-title">Activity · last 60m</h2>
        {activity.length === 0 ? (
          <div className="dev-panel px-5 py-8 text-center ds-page-loading">
            no events in window
          </div>
        ) : (
          <ActivityChart data={activity} />
        )}
      </section>

      {activeTasks.length > 0 && (
        <section>
          <div className="flex items-center justify-between mb-3">
            <h2 className="page-section-title mb-0">
              Active tasks
              <span className="ml-2 text-text-secondary normal-case tracking-normal">{activeTasks.length}</span>
            </h2>
            <Link to="/dashboard/tasks" className="font-mono text-xs text-brand hover:underline">
              /tasks
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

      {recentlyCompleted.length > 0 && (
        <section>
          <h2 className="page-section-title">
            Recently completed
            <span className="ml-2 text-text-secondary normal-case tracking-normal">{recentlyCompleted.length}</span>
          </h2>
          <div className="space-y-3">
            {recentlyCompleted.map(task => (
              <TaskCard
                key={task.id}
                task={task}
                agentName={agentMap.get(task.agent_id) ?? task.agent_id.slice(0, 8)}
              />
            ))}
          </div>
        </section>
      )}
    </PageLayout>
  )
}

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
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [document.documentElement.classList.contains('dark')])
}

function ActivityChart({ data }: { data: ActivityBucket[] }) {
  const colors = useChartColors()
  const rows = useMemo(() => {
    const counts = new Map<number, ChartRow>()
    const successOutcomes = new Set([
      'executed', 'approved', 'observed', 'approval_released', 'pass_through', 'success',
      'shell_poll_pass_through', 'readonly_shell_pass_through', 'auto_approved_from_conversation',
      'inline_task_approved', 'inline_task_auto_approved',
    ])
    for (const b of data) {
      const ms = new Date(b.bucket).getTime()
      if (!counts.has(ms)) counts.set(ms, { time: '', executed: 0, blocked: 0, pending: 0 })
      const row = counts.get(ms)!
      if (successOutcomes.has(b.outcome)) row.executed += b.count
      else if (b.outcome === 'blocked' || b.outcome === 'restricted') row.blocked += b.count
      else row.pending += b.count
    }

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
    <div className="dev-panel p-4">
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

function RuntimePolicyCard({ status, activeSessionCount }: { status: RuntimeStatus; activeSessionCount: number }) {
  if (!status.enabled && activeSessionCount === 0) return null

  return (
    <section>
      <div className="dev-panel p-5 space-y-3">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <div>
            <h2 className="page-section-title mb-1">Runtime policy</h2>
            <p className="text-sm text-text-tertiary">
              Local enforcement and approval settings for proxy-backed runs.
            </p>
          </div>
          <div className="flex flex-wrap items-center gap-2">
            <span className={`dev-chip ${status.enabled ? 'text-success border-success/30' : ''}`}>
              <span className={`w-1.5 h-1.5 rounded-full ${status.enabled ? 'bg-success' : 'bg-text-tertiary'}`} />
              {status.enabled ? 'proxy:on' : 'proxy:off'}
            </span>
            <span className="dev-chip">
              sessions:{activeSessionCount}
            </span>
          </div>
        </div>
        <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4 text-sm">
          <div className="dev-inset p-3">
            <div className="font-mono text-2xs uppercase tracking-widest text-text-tertiary">observation</div>
            <div className="mt-1 text-text-primary font-mono text-sm">{status.observation_mode_default ? 'observe' : 'enforce'}</div>
          </div>
          <div className="dev-inset p-3">
            <div className="font-mono text-2xs uppercase tracking-widest text-text-tertiary">inline approvals</div>
            <div className="mt-1 text-text-primary font-mono text-sm">{status.inline_approval_enabled ? 'enabled' : 'disabled'}</div>
          </div>
          <div className="dev-inset p-3">
            <div className="font-mono text-2xs uppercase tracking-widest text-text-tertiary">lease timeout</div>
            <div className="mt-1 text-text-primary font-mono text-sm">{status.tool_lease_timeout_seconds}s</div>
          </div>
          <div className="dev-inset p-3">
            <div className="font-mono text-2xs uppercase tracking-widest text-text-tertiary">retry ttl</div>
            <div className="mt-1 text-text-primary font-mono text-sm">{status.one_off_ttl_seconds}s</div>
          </div>
        </div>
        {status.proxy_url && (
          <div className="dev-inset p-3">
            <div className="font-mono text-2xs uppercase tracking-widest text-text-tertiary">proxy endpoint</div>
            <code className="mt-1 block text-xs text-brand break-all">{status.proxy_url}</code>
          </div>
        )}
      </div>
    </section>
  )
}
