import { useState, useEffect, useRef } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { Link, useSearchParams } from 'react-router-dom'
import { api, type Task, type Agent, type NotificationConfig } from '../api/client'
import { serviceName, actionName } from '../lib/services'
import Onboarding from './Onboarding'

interface Props {
  pendingCount: number
  actionableTaskCount?: number
}

function summarizeTaskActions(actions: Task['authorized_actions']): string {
  const groups = new Map<string, { auto: string[]; manual: string[] }>()
  for (const a of actions) {
    const svc = serviceName(a.service)
    if (!groups.has(svc)) groups.set(svc, { auto: [], manual: [] })
    const g = groups.get(svc)!
    if (a.auto_execute) {
      g.auto.push(actionName(a.action))
    } else {
      g.manual.push(actionName(a.action))
    }
  }

  const parts: string[] = []
  for (const [svc, g] of groups) {
    if (g.auto.length > 0) parts.push(`${g.auto.join(', ')} on ${svc}`)
    if (g.manual.length > 0) parts.push(`${g.manual.join(', ')} on ${svc} with approval`)
  }
  return parts.join(' · ') || 'No actions'
}

function PendingTaskCard({ task, agentName }: { task: Task; agentName: string }) {
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
  const isExpansion = task.status === 'pending_scope_expansion'

  if (result) {
    return (
      <div className="border rounded-lg p-4 bg-gray-50 text-sm text-gray-500">{result}</div>
    )
  }

  return (
    <div className="border border-orange-200 rounded-lg p-4 bg-white space-y-2">
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0 flex-1">
          <p className="text-sm font-semibold text-gray-900">{task.purpose}</p>
          <p className="text-xs text-gray-500 mt-0.5">
            {agentName} · {summarizeTaskActions(task.authorized_actions)}
          </p>
        </div>
        <span className="text-xs px-2 py-0.5 rounded-full bg-orange-100 text-orange-800 font-medium shrink-0">
          {isExpansion ? 'Scope expansion' : 'New task'}
        </span>
      </div>

      {isExpansion && task.pending_action && (
        <div className="text-xs text-orange-700 bg-orange-50 rounded p-2">
          Wants to add: {serviceName(task.pending_action.service)}: {actionName(task.pending_action.action)}
          {task.pending_reason && <span className="italic ml-1">— "{task.pending_reason}"</span>}
        </div>
      )}

      <div className="flex gap-2">
        <button
          onClick={() => isExpansion ? expandApproveMut.mutate() : approveMut.mutate()}
          disabled={isPending}
          className="flex-1 py-1.5 text-sm rounded bg-green-600 text-white hover:bg-green-700 disabled:opacity-50"
        >
          Approve
        </button>
        <button
          onClick={() => isExpansion ? expandDenyMut.mutate() : denyMut.mutate()}
          disabled={isPending}
          className="flex-1 py-1.5 text-sm rounded bg-red-100 text-red-700 hover:bg-red-200 disabled:opacity-50"
        >
          Deny
        </button>
      </div>
    </div>
  )
}

export default function Overview({ pendingCount }: Props) {
  const [searchParams, setSearchParams] = useSearchParams()
  const qc = useQueryClient()
  const [deepLinkResult, setDeepLinkResult] = useState<string | null>(null)

  const deepApprove = useMutation({
    mutationFn: (requestId: string) => api.approvals.approve(requestId),
    onSuccess: (_data, requestId) => {
      setDeepLinkResult(`Request ${requestId.slice(0, 8)}... approved.`)
      qc.invalidateQueries({ queryKey: ['approvals'] })
    },
    onError: (err: Error) => setDeepLinkResult(`Approve failed: ${err.message}`),
  })

  const deepDeny = useMutation({
    mutationFn: (requestId: string) => api.approvals.deny(requestId),
    onSuccess: (_data, requestId) => {
      setDeepLinkResult(`Request ${requestId.slice(0, 8)}... denied.`)
      qc.invalidateQueries({ queryKey: ['approvals'] })
    },
    onError: (err: Error) => setDeepLinkResult(`Deny failed: ${err.message}`),
  })

  // Handle deep link actions from Telegram buttons
  useEffect(() => {
    const action = searchParams.get('action')
    const requestId = searchParams.get('request_id')
    if (!action || !requestId) return

    // Clear params immediately to prevent re-triggering
    setSearchParams({}, { replace: true })

    if (action === 'approve') {
      deepApprove.mutate(requestId)
    } else if (action === 'deny') {
      deepDeny.mutate(requestId)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const { data: services } = useQuery({
    queryKey: ['services'],
    queryFn: () => api.services.list(),
  })
  const { data: auditData } = useQuery({
    queryKey: ['audit', { limit: 10 }],
    queryFn: () => api.audit.list({ limit: 10 }),
  })
  const { data: tasksData } = useQuery({
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

  const { data: notificationsData } = useQuery({
    queryKey: ['notifications'],
    queryFn: (): Promise<NotificationConfig[]> => api.notifications.list(),
  })

  const allServices = services?.services ?? []

  // Latch onboarding visibility once on first data load.
  // During the session, only user actions (dismiss / complete all steps) hide it.
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

  const activatedCount = allServices.filter(s => s.status === 'activated').length
  const allTasks = tasksData?.tasks ?? []
  const actionableTasks = allTasks.filter(
    (t: Task) => t.status === 'pending_approval' || t.status === 'pending_scope_expansion'
  )
  const activeTasks = allTasks.filter((t: Task) => t.status === 'active')
  const hasAnythingPending = actionableTasks.length > 0 || pendingCount > 0

  return (
    <div className="p-8 space-y-8">
      <h1 className="text-2xl font-bold text-gray-900">Overview</h1>

      {/* Onboarding */}
      {showOnboarding && (
        <Onboarding
          allServices={allServices}
          initialCompleted={onboardingInitial}
          onDismiss={() => setShowOnboarding(false)}
        />
      )}

      {/* Deep link result banner */}
      {deepLinkResult && (
        <div className="rounded-lg border border-blue-200 bg-blue-50 px-5 py-3 flex items-center justify-between">
          <span className="text-blue-800 text-sm">{deepLinkResult}</span>
          <button onClick={() => setDeepLinkResult(null)} className="text-blue-500 text-xs hover:underline">Dismiss</button>
        </div>
      )}

      {/* Attention area */}
      {hasAnythingPending ? (
        <section className="space-y-3">
          {/* Pending task approvals — inline cards */}
          {actionableTasks.map(task => (
            <PendingTaskCard
              key={task.id}
              task={task}
              agentName={agentMap.get(task.agent_id) ?? task.agent_id.slice(0, 8)}
            />
          ))}

          {/* Pending request approvals banner */}
          {pendingCount > 0 && (
            <div className="rounded-lg border border-orange-200 bg-orange-50 px-5 py-4 flex items-center justify-between">
              <span className="text-orange-800 font-medium">
                {pendingCount} request approval{pendingCount > 1 ? 's' : ''} awaiting your decision
              </span>
              <span className="text-orange-600 text-sm">See the panel →</span>
            </div>
          )}
        </section>
      ) : (
        <div className="rounded-lg border border-green-200 bg-green-50 px-5 py-4 flex items-center gap-3">
          <svg className="w-5 h-5 text-green-600 shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
            <path d="M22 11.08V12a10 10 0 1 1-5.93-9.14" />
            <polyline points="22 4 12 14.01 9 11.01" />
          </svg>
          <span className="text-green-800 font-medium">All clear — nothing needs your attention</span>
        </div>
      )}

      {/* Active tasks summary */}
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
          <div className="space-y-2">
            {activeTasks.slice(0, 5).map(task => (
              <div key={task.id} className="bg-white border rounded-lg px-4 py-3 flex items-center justify-between">
                <div className="min-w-0">
                  <p className="text-sm font-medium text-gray-900 truncate">{task.purpose}</p>
                  <p className="text-xs text-gray-400">{agentMap.get(task.agent_id) ?? task.agent_id.slice(0, 8)}</p>
                </div>
                {task.lifetime === 'standing' && (
                  <span className="text-xs px-2 py-0.5 rounded-full bg-purple-100 text-purple-700 font-medium shrink-0">
                    Ongoing
                  </span>
                )}
              </div>
            ))}
            {activeTasks.length > 5 && (
              <Link to="/dashboard/tasks" className="block text-center text-sm text-blue-600 hover:underline py-1">
                +{activeTasks.length - 5} more
              </Link>
            )}
          </div>
        </section>
      )}

      {/* Quick stats */}
      <div className="grid grid-cols-2 gap-4">
        <StatCard
          label="Services connected"
          value={activatedCount}
          href="/dashboard/services"
          color="blue"
        />
        <StatCard
          label="Audit entries"
          value={auditData?.total ?? 0}
          href="/dashboard/audit"
          color="gray"
        />
      </div>
    </div>
  )
}

function StatCard({
  label, value, href, color,
}: {
  label: string
  value: number
  href: string
  color: 'blue' | 'orange' | 'gray'
}) {
  const ring: Record<string, string> = {
    blue: 'border-blue-200',
    orange: 'border-orange-200',
    gray: 'border-gray-200',
  }
  const text: Record<string, string> = {
    blue: 'text-blue-700',
    orange: 'text-orange-600',
    gray: 'text-gray-800',
  }
  return (
    <Link
      to={href}
      className={`bg-white border rounded-lg p-5 hover:shadow-sm transition-shadow ${ring[color]}`}
    >
      <div className={`text-3xl font-bold ${text[color]}`}>{value}</div>
      <div className="text-sm text-gray-500 mt-1">{label}</div>
    </Link>
  )
}
