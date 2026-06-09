import { Fragment, useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { Link, useNavigate, useParams, useSearchParams } from 'react-router-dom'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api, type Agent, type ApprovalRecord, type AgentRuntimeSettings, type AuditEntry, type RuntimePolicyRule, type RuntimeSession, type Task } from '../api/client'
import type { ConnectionRequest, InstallContext } from '../api/client'
import { useAuth } from '../hooks/useAuth'
import { formatDistanceToNow } from 'date-fns'
import CountdownTimer from '../components/CountdownTimer'
import TaskCard from '../components/TaskCard'
import { RuntimeApprovalsPanel, RuntimeSessionsPanel, filterLiveRuntimeApprovals, isActiveRuntimeSession } from './Runtime'
import { ActiveServiceRow, openOAuthUrl } from './Services'
import { AgentHarnessIcon, AgentPickerContent } from '../components/ConnectAgentPicker'
import { AgentListCard } from '../components/AgentListCard'
import AgentSetupTryItPanel from '../components/AgentSetupTryItPanel'
import AgentUpstreamKeySetupPanel, { apiKeyContinueHint, useUpstreamKeyReadiness } from '../components/AgentUpstreamKeySetupPanel'
import InstallHelperStepPanel from '../components/InstallHelperStepPanel'
import { buildHelperCommand, INSTALLER_HELPERS, type InstallerHelper } from '../utils/installerHelpers'
import VaultKeyStep from '../components/VaultKeyStep'
import QuestionToggleGroup from '../components/QuestionToggleGroup'
import {
  type CredentialScope,
  type LLMProvider,
  hasAnyUpstreamKey,
  hasProviderUpstreamKey,
  providerLabel,
} from '../utils/llmCredentials'
import {
  AGENT_META,
  LEGACY_AGENT_TABS,
  PROXY_LITE_AGENT_TABS,
  agentSetupPath,
  type AgentTab,
} from '../constants/agentTabs'
import { PageHeader } from '../components/layout/PageLayout'

export default function Agents() {
  const { currentOrg, features } = useAuth()
  const { agentId, harness } = useParams()
  const navigate = useNavigate()
  const orgId = currentOrg?.id
  const qc = useQueryClient()
  const liveSessionsUI = !orgId && !!features?.agent_live_sessions
  const runtimePolicyUI = !orgId && !!features?.runtime_policy_ui
  const proxyLiteUI = !orgId && !!features?.proxy_lite
  const { data: agents, isLoading } = useQuery({
    queryKey: ['agents', orgId ?? 'personal'],
    queryFn: () => orgId ? api.orgs.agents(orgId) : api.agents.list(),
  })

  const { data: connections } = useQuery({
    queryKey: ['connections'],
    queryFn: () => api.connections.list(),
    enabled: !orgId,
  })
  const { data: runtimeStatus } = useQuery({
    queryKey: ['runtime-status'],
    queryFn: () => api.runtime.status(),
    enabled: runtimePolicyUI || liveSessionsUI,
  })
  const fullRuntimeSessionsUI = liveSessionsUI && !!runtimeStatus?.enabled
  const { data: runtimeSessions } = useQuery({
    queryKey: ['runtime-sessions'],
    queryFn: () => api.runtime.listSessions(),
    enabled: fullRuntimeSessionsUI,
    refetchInterval: 15_000,
  })
  const { data: runtimeApprovals } = useQuery({
    queryKey: ['runtime-approvals'],
    queryFn: () => api.runtime.listApprovals(),
    enabled: fullRuntimeSessionsUI,
    refetchInterval: 10_000,
  })

  const pending = (!orgId ? connections : undefined) ?? []
  const sessionsByAgent = useMemo(() => {
    const grouped = new Map<string, RuntimeSession[]>()
    for (const session of runtimeSessions?.entries ?? []) {
      if (!isActiveRuntimeSession(session)) continue
      const list = grouped.get(session.agent_id) ?? []
      list.push(session)
      grouped.set(session.agent_id, list)
    }
    return grouped
  }, [runtimeSessions])
  const approvalsByAgent = useMemo(() => {
    const grouped = new Map<string, ApprovalRecord[]>()
    const liveApprovals = filterLiveRuntimeApprovals(runtimeApprovals?.entries ?? [], runtimeSessions?.entries ?? [])
    for (const approval of liveApprovals) {
      if (!approval.agent_id) continue
      const list = grouped.get(approval.agent_id) ?? []
      list.push(approval)
      grouped.set(approval.agent_id, list)
    }
    return grouped
  }, [runtimeApprovals, runtimeSessions])

  const { data: tasksData } = useQuery({
    queryKey: ['tasks', 'agent-cards', orgId ?? 'personal'],
    queryFn: () => orgId
      ? api.orgs.tasks(orgId, { limit: 200 })
      : api.tasks.list({ limit: 200 }),
    enabled: !!agents && agents.length > 0,
  })

  const taskStatsByAgent = useMemo(() => {
    const last = new Map<string, Task>()
    const counts = new Map<string, number>()
    for (const task of tasksData?.tasks ?? []) {
      counts.set(task.agent_id, (counts.get(task.agent_id) ?? 0) + 1)
      const prev = last.get(task.agent_id)
      if (!prev || new Date(task.created_at) > new Date(prev.created_at)) {
        last.set(task.agent_id, task)
      }
    }
    return { last, counts }
  }, [tasksData])

  const selectedAgent = useMemo(() => agents?.find(agent => agent.id === agentId), [agents, agentId])
  const agentTabs = proxyLiteUI ? PROXY_LITE_AGENT_TABS : LEGACY_AGENT_TABS
  const setupHarness = harness && agentTabs.includes(harness as AgentTab)
    ? (harness as AgentTab)
    : null

  if (harness) {
    return <AgentHarnessSetupView harness={setupHarness} />
  }

  if (agentId) {
    if (isLoading) {
      return <div className="page-shell text-sm text-text-tertiary">Loading…</div>
    }
    if (!selectedAgent) {
      return (
        <div className="page-shell">
          <Link to="/dashboard/agents" className="text-sm text-brand hover:underline">← Back to agents</Link>
          <div className="rounded-md border border-border-default bg-surface-1 p-6 text-sm text-text-tertiary">
            Agent not found.
          </div>
        </div>
      )
    }
    return (
      <AgentDetailView
        agent={selectedAgent}
        orgId={orgId}
        sessions={sessionsByAgent.get(selectedAgent.id) ?? []}
        approvals={approvalsByAgent.get(selectedAgent.id) ?? []}
        liveSessionsUI={fullRuntimeSessionsUI}
        runtimePolicyUI={runtimePolicyUI}
        proxyLiteUI={proxyLiteUI}
        onDeleted={() => {
          qc.invalidateQueries({ queryKey: ['agents'] })
          qc.invalidateQueries({ queryKey: ['tasks'] })
          qc.invalidateQueries({ queryKey: ['overview'] })
          qc.invalidateQueries({ queryKey: ['welcome'] })
        }}
      />
    )
  }

  return (
    <div className="page-shell">
      <PageHeader
        title="Agents"
        meta={
          <>
            An agent is any AI system (Claude, a custom bot, etc.) that you want to give controlled access to your services.
            Each agent gets a unique token — paste it into your agent&apos;s configuration to connect it to Clawvisor.
          </>
        }
      />

      {/* Connect an Agent guide (personal context only) */}
      {!orgId && <ConnectAgentGuide connectedAgents={agents ?? []} />}

      {/* Pending connection requests (personal context only).
          Hidden while the wizard is mid-flight — the wizard renders its
          own copy of these cards inline so the user can approve without
          scrolling. */}
      {!orgId && pending.length > 0 && (
        <section>
          <div className="flex items-center gap-3 mb-3">
            <h2 className="text-lg font-semibold text-text-primary">Pending Connections</h2>
            <span className="bg-warning text-surface-0 text-xs font-bold rounded px-2.5 py-0.5 font-mono">
              {pending.length}
            </span>
          </div>
          <div className="space-y-3">
            {pending.map(cr => (
              <ConnectionCard key={cr.id} request={cr} />
            ))}
          </div>
        </section>
      )}

      {/* Agent list */}
      <section>
        <h2 className="text-lg font-semibold text-text-primary mb-3">Your Agents</h2>

        {isLoading && <div className="text-sm text-text-tertiary">Loading…</div>}

        {!isLoading && (!agents || agents.length === 0) && (
          <div className="text-sm text-text-tertiary text-center py-8 bg-surface-1 border border-border-default rounded-md">
            No agents yet. Follow the setup guides above to connect your first agent.
          </div>
        )}

        <div className="grid grid-cols-1 md:grid-cols-2 gap-3 items-stretch">
          {agents?.map(agent => {
            const liveSessions = fullRuntimeSessionsUI ? (sessionsByAgent.get(agent.id) ?? []) : []
            const lastTask = taskStatsByAgent.last.get(agent.id)
            const taskCount = taskStatsByAgent.counts.get(agent.id) ?? agent.active_task_count
            const isLive = liveSessions.length > 0 || agent.active_task_count > 0
            return (
              <AgentListCard
                key={agent.id}
                agent={agent}
                live={isLive}
                taskCount={taskCount}
                lastTaskPurpose={lastTask?.purpose}
                onClick={() => navigate(`/dashboard/agents/${agent.id}`)}
              />
            )
          })}
        </div>
      </section>

    </div>
  )
}

function AgentDetailView({
  agent,
  orgId,
  sessions,
  approvals,
  liveSessionsUI,
  runtimePolicyUI,
  proxyLiteUI,
  onDeleted,
}: {
  agent: Agent
  orgId?: string
  sessions: RuntimeSession[]
  approvals: ApprovalRecord[]
  liveSessionsUI: boolean
  runtimePolicyUI: boolean
  proxyLiteUI: boolean
  onDeleted: () => void
}) {
  const qc = useQueryClient()
  const { data: allRuntimeSessions } = useQuery({
    queryKey: ['runtime-sessions'],
    queryFn: () => api.runtime.listSessions(),
    enabled: liveSessionsUI,
    refetchInterval: 15_000,
  })
  const { data: runtimeStatus } = useQuery({
    queryKey: ['runtime-status'],
    queryFn: () => api.runtime.status(),
    enabled: runtimePolicyUI || liveSessionsUI || proxyLiteUI,
  })
  const { data: recentActivity } = useQuery({
    queryKey: ['audit', 'agent', agent.id],
    queryFn: () => api.audit.list({ agent_id: agent.id, limit: 8 }),
    enabled: !orgId,
    refetchInterval: 20_000,
  })
  const { data: allEgressRules } = useQuery({
    queryKey: ['runtime-rules', 'egress', 'all'],
    queryFn: () => api.runtime.listRules({ kind: 'egress' }),
    enabled: runtimePolicyUI,
  })
  const { data: allToolRules } = useQuery({
    queryKey: ['runtime-rules', 'tool', 'all'],
    queryFn: () => api.runtime.listRules({ kind: 'tool' }),
    enabled: runtimePolicyUI,
  })
  const deleteMut = useMutation({
    mutationFn: (id: string) => orgId ? api.orgs.deleteAgent(orgId, id) : api.agents.delete(id),
    onSuccess: onDeleted,
  })
  const agentMap = useMemo(() => new Map([[agent.id, agent]]), [agent])
  const fullRuntimeActive = !!runtimeStatus?.enabled
  const recentSessions = useMemo(() => {
    return (allRuntimeSessions?.entries ?? [])
      .filter(session => session.agent_id === agent.id)
      .sort((a, b) => new Date(b.created_at).getTime() - new Date(a.created_at).getTime())
      .slice(0, 10)
  }, [agent.id, allRuntimeSessions])
  const agentRules = useMemo(() => {
    const rules = [...(allEgressRules?.entries ?? []), ...(allToolRules?.entries ?? [])]
    return rules.filter(rule => !rule.agent_id || rule.agent_id === agent.id)
  }, [agent.id, allEgressRules, allToolRules])
  const proxyLiteActive = proxyLiteUI && !!runtimeStatus?.proxy_lite_enabled
  const showRuntimeSettings = runtimePolicyUI && runtimeStatus?.enabled
  const showAgentSettings = showRuntimeSettings || proxyLiteActive

  return (
    <div className="page-shell">
      <div className="space-y-3">
        <Link to="/dashboard/agents" className="text-sm text-brand hover:underline">← Back to agents</Link>
        <div className="flex flex-wrap items-start justify-between gap-4">
          <div>
            <h1 className="page-title">{agent.name}</h1>
            {agent.description && <p className="text-sm text-text-secondary mt-2 max-w-3xl">{agent.description}</p>}
            <p className="text-xs text-text-tertiary mt-2">
              Created {formatDistanceToNow(new Date(agent.created_at), { addSuffix: true })} · {agent.id}
            </p>
          </div>
          <button
            onClick={() => {
              const taskWarning = agent.active_task_count > 0
                ? `\n\n${agent.active_task_count} active ${agent.active_task_count === 1 ? 'task' : 'tasks'} will be revoked.`
                : ''
              if (confirm(`Revoke agent "${agent.name}"? Running agents using this token will stop working.${taskWarning}`)) {
                deleteMut.mutate(agent.id)
              }
            }}
            disabled={deleteMut.isPending}
            className="text-sm px-4 py-2 rounded bg-danger/10 text-danger border border-danger/20 hover:bg-danger/20 disabled:opacity-50"
          >
            {deleteMut.isPending ? 'Revoking…' : 'Revoke agent'}
          </button>
        </div>
      </div>

      <div className={`grid gap-3 ${fullRuntimeActive && liveSessionsUI ? 'md:grid-cols-3' : 'md:grid-cols-1'}`}>
        {fullRuntimeActive && liveSessionsUI && <AgentMetric label="Live sessions" value={String(sessions.length)} />}
        {fullRuntimeActive && liveSessionsUI && <AgentMetric label="Pending approvals" value={String(approvals.length)} />}
        <AgentMetric label="Active tasks" value={String(agent.active_task_count)} />
      </div>

      <div className="flex flex-wrap gap-3">
        <Link to={`/dashboard/activity?agent_id=${encodeURIComponent(agent.id)}`} className="rounded border border-border-default px-4 py-2 text-sm text-text-secondary hover:bg-surface-2">
          Open Activity for Agent
        </Link>
        <Link to={`/dashboard/policy?agent_id=${encodeURIComponent(agent.id)}`} className="rounded border border-border-default px-4 py-2 text-sm text-text-secondary hover:bg-surface-2">
          Open Policy
        </Link>
      </div>

      {agent.install_context?.harness && (
        <AgentConnectionDetailsPanel agent={agent} />
      )}

      {showAgentSettings && <AgentRuntimePanel agentId={agent.id} defaultOpen showRuntimeControls={showRuntimeSettings} />}

      {proxyLiteActive && <AgentLiteProxyPanel agentId={agent.id} />}
      {proxyLiteActive && <AgentLLMCredentialsPanel agentId={agent.id} />}

      {runtimePolicyUI && (
        <AgentPolicyPanel
          agent={agent}
          rules={agentRules}
          recentActivity={recentActivity?.entries ?? []}
        />
      )}

      {fullRuntimeActive && liveSessionsUI && (
        <RecentSessionsPanel sessions={recentSessions} />
      )}

      {fullRuntimeActive && liveSessionsUI && (
        <RuntimeSessionsPanel
          sessions={sessions}
          agents={agentMap}
          onUpdated={() => {
            qc.invalidateQueries({ queryKey: ['runtime-sessions'] })
            qc.invalidateQueries({ queryKey: ['overview'] })
          }}
        />
      )}

      {fullRuntimeActive && liveSessionsUI && (
        <RuntimeApprovalsPanel
          approvals={approvals}
          onResolved={() => {
            qc.invalidateQueries({ queryKey: ['runtime-approvals'] })
            qc.invalidateQueries({ queryKey: ['runtime-sessions'] })
            qc.invalidateQueries({ queryKey: ['overview'] })
          }}
        />
      )}
    </div>
  )
}

function AgentPolicyPanel({
  agent,
  rules,
  recentActivity,
}: {
  agent: Agent
  rules: RuntimePolicyRule[]
  recentActivity: AuditEntry[]
}) {
  const starterProfile = agent.runtime_settings?.starter_profile ?? 'none'
  const systemRules = rules.filter(rule => rule.source === 'system')
  const manualRules = rules.filter(rule => rule.source !== 'system')
  const inferredPresets = new Set<string>()
  for (const rule of systemRules) {
    if (rule.host === 'api.telegram.org') inferredPresets.add('Telegram')
  }

  return (
    <section className="rounded border border-border-subtle bg-surface-1 p-5 space-y-4">
      <div>
        <h2 className="text-lg font-semibold text-text-primary">Applied Policy</h2>
        <p className="text-sm text-text-tertiary mt-1">Current starter profile, service presets, and effective runtime restrictions for this agent.</p>
      </div>
      <div className="grid gap-3 md:grid-cols-3">
        <AgentMetric label="Starter profile" value={starterProfile === 'none' ? 'None' : starterProfile} />
        <AgentMetric label="Service presets" value={String(inferredPresets.size)} />
        <AgentMetric label="Effective runtime rules" value={String(rules.length)} />
      </div>
      <div className="grid gap-4 md:grid-cols-2">
        <div className="rounded border border-border-subtle bg-surface-0 p-4">
          <div className="text-sm font-medium text-text-primary">Presets</div>
          <div className="mt-2 space-y-2 text-sm text-text-secondary">
            <div>Harness profile: <span className="text-text-primary">{starterProfile === 'none' ? 'None' : starterProfile}</span></div>
            <div>Service presets: <span className="text-text-primary">{inferredPresets.size === 0 ? 'None detected' : Array.from(inferredPresets).join(', ')}</span></div>
          </div>
        </div>
        <div className="rounded border border-border-subtle bg-surface-0 p-4">
          <div className="text-sm font-medium text-text-primary">Restrictions</div>
          <div className="mt-2 space-y-2 text-sm text-text-secondary">
            <div>Manual / event-derived rules: <span className="text-text-primary">{manualRules.length}</span></div>
            <div>Preset-installed rules: <span className="text-text-primary">{systemRules.length}</span></div>
          </div>
        </div>
      </div>
      <div className="rounded border border-border-subtle bg-surface-0 p-4">
        <div className="text-sm font-medium text-text-primary">Recent Activity Summary</div>
        <div className="mt-3 space-y-2">
          {recentActivity.length === 0 && (
            <div className="text-sm text-text-tertiary">No recent activity for this agent.</div>
          )}
          {recentActivity.map(entry => (
            <div key={entry.id} className="flex flex-wrap items-center justify-between gap-3 text-sm">
              <div className="text-text-primary">{entry.summary_text || `${entry.service} ${entry.action}`}</div>
              <div className="text-xs text-text-tertiary">
                {entry.outcome} · {formatDistanceToNow(new Date(entry.timestamp), { addSuffix: true })}
              </div>
            </div>
          ))}
        </div>
      </div>
    </section>
  )
}

function RecentSessionsPanel({ sessions }: { sessions: RuntimeSession[] }) {
  return (
    <section className="rounded border border-border-subtle bg-surface-1 p-5 space-y-4">
      <div>
        <h2 className="text-lg font-semibold text-text-primary">Recent Sessions</h2>
        <p className="text-sm text-text-tertiary mt-1">Most recent runtime sessions for this agent, including ended and revoked sessions.</p>
      </div>
      <div className="space-y-2">
        {sessions.length === 0 && (
          <div className="rounded border border-dashed border-border-default bg-surface-0 px-4 py-6 text-sm text-text-tertiary">
            No runtime sessions yet.
          </div>
        )}
        {sessions.map(session => {
          const status = session.revoked_at
            ? 'revoked'
            : new Date(session.expires_at).getTime() <= Date.now()
              ? 'expired'
              : 'live'
          return (
            <div key={session.id} className="flex flex-wrap items-center justify-between gap-3 rounded border border-border-subtle bg-surface-0 px-4 py-3">
              <div>
                <div className="text-sm font-medium text-text-primary">{session.id}</div>
                <div className="mt-1 text-xs text-text-tertiary">
                  {session.observation_mode ? 'observe' : 'enforce'} · started {formatDistanceToNow(new Date(session.created_at), { addSuffix: true })}
                </div>
              </div>
              <div className="text-xs text-text-tertiary">{status}</div>
            </div>
          )
        })}
      </div>
    </section>
  )
}

function AgentMetric({ label, value }: { label: string; value: string }) {
  return (
    <div className="rounded border border-border-subtle bg-surface-1 p-4">
      <div className="text-xs uppercase tracking-wider text-text-tertiary">{label}</div>
      <div className="mt-1 text-lg font-semibold text-text-primary">{value}</div>
    </div>
  )
}

// Shows the harness / install mode this agent came from, plus a "Reinstall"
// shortcut back to the wizard. Surfaced on the agent detail page so the user
// can recognize an OpenClaw vs. Claude Code install without remembering
// what they named it, and can rebuild the bootstrap from the agent.
function AgentConnectionDetailsPanel({ agent }: { agent: Agent }) {
  const ic = agent.install_context
  if (!ic?.harness) return null

  // Map server-side harness slugs back to the wizard's picker target so the
  // Reinstall link drops the user back into the right flow. Currently only
  // hermes/openclaw round-trip through the installer wizard; other targets
  // (claude-code, codex, claude-desktop) have separate flows that aren't
  // resumable by URL today.
  const wizardableHarnesses = new Set(['openclaw', 'hermes'])
  const reinstallTarget = wizardableHarnesses.has(ic.harness) ? ic.harness : null
  const label = ic.harness === 'openclaw'
    ? 'OpenClaw'
    : ic.harness === 'hermes'
      ? 'Hermes'
      : ic.harness === 'claude-code'
        ? 'Claude Code'
        : ic.harness === 'codex'
          ? 'Codex'
          : ic.harness === 'claude-desktop'
            ? 'Claude Desktop'
            : ic.harness

  return (
    <div className="rounded border border-border-default bg-surface-1 px-5 py-4">
      <div className="flex items-start justify-between gap-3 flex-wrap">
        <div>
          <h3 className="text-sm font-medium text-text-primary">Connection</h3>
          <p className="text-xs text-text-tertiary mt-0.5">How this agent was registered.</p>
        </div>
        {reinstallTarget && (
          <Link
            to={agentSetupPath(reinstallTarget as AgentTab)}
            className="text-xs rounded border border-border-default px-3 py-1.5 text-text-secondary hover:bg-surface-2"
          >
            Reinstall instructions →
          </Link>
        )}
      </div>
      <dl className="mt-3 grid grid-cols-1 sm:grid-cols-2 gap-x-6 gap-y-2 text-xs">
        <div>
          <dt className="text-text-tertiary">Harness</dt>
          <dd className="text-text-primary font-medium">{label}</dd>
        </div>
        {ic.install_mode && (
          <div>
            <dt className="text-text-tertiary">Install mode</dt>
            <dd className="text-text-primary font-medium">{ic.install_mode}</dd>
          </div>
        )}
        {ic.host_os && (
          <div>
            <dt className="text-text-tertiary">Host OS</dt>
            <dd className="text-text-primary font-medium">{ic.host_os}</dd>
          </div>
        )}
        {ic.harness_version && (
          <div>
            <dt className="text-text-tertiary">Harness version</dt>
            <dd className="text-text-primary font-mono">{ic.harness_version}</dd>
          </div>
        )}
      </dl>
    </div>
  )
}

function AgentRuntimePanel({
  agentId,
  defaultOpen = false,
  showRuntimeControls = true,
}: {
  agentId: string
  defaultOpen?: boolean
  showRuntimeControls?: boolean
}) {
  const qc = useQueryClient()
  const [open, setOpen] = useState(defaultOpen)
  const { data: settings } = useQuery({
    queryKey: ['agent-runtime-settings', agentId],
    queryFn: () => api.agents.getRuntimeSettings(agentId),
    enabled: open || defaultOpen,
  })
  const [draft, setDraft] = useState<AgentRuntimeSettings | null>(null)

  useEffect(() => {
    if (settings && draft == null) {
      setDraft(settings)
    }
  }, [settings, draft])

  const saveMut = useMutation({
    mutationFn: (next: AgentRuntimeSettings) => api.agents.updateRuntimeSettings(agentId, next),
    onSuccess: (saved) => {
      setDraft(saved)
      qc.invalidateQueries({ queryKey: ['agent-runtime-settings', agentId] })
      qc.invalidateQueries({ queryKey: ['agents'] })
      qc.invalidateQueries({ queryKey: ['runtime-status'] })
    },
  })

  const current = draft ?? settings
  const secretDetectionEnabled = current?.lite_proxy_secret_detection_disabled === false
  const secretDetectionSummary = `secret detection ${secretDetectionEnabled ? 'on' : 'off'}`

  return (
    <div className="mt-3 overflow-hidden rounded border border-border-subtle bg-surface-0">
      <button
        onClick={() => {
          setOpen(v => !v)
          if (!open && settings && !draft) setDraft(settings)
        }}
        className="flex w-full items-center justify-between px-4 py-3 text-left"
      >
        <div>
          <div className="text-sm font-medium text-text-primary">{showRuntimeControls ? 'Runtime settings' : 'Agent settings'}</div>
          <div className="text-xs text-text-tertiary">
            {current
              ? showRuntimeControls
                ? `${current.runtime_enabled ? 'enabled' : 'disabled'} · ${current.runtime_mode} · ${current.starter_profile || 'none'} · ${secretDetectionSummary}`
                : `secret detection ${secretDetectionEnabled ? 'enabled' : 'disabled'}`
              : showRuntimeControls
                ? 'Configure observe vs enforce defaults, starter profile, and outbound credential posture.'
                : 'Configure experimental agent controls.'}
          </div>
        </div>
        <span className="text-xs text-text-tertiary">{open ? 'Hide' : 'Edit'}</span>
      </button>
      {open && current && (
        <div className="border-t border-border-subtle px-4 py-4 space-y-3">
          {showRuntimeControls && (
            <>
              <div className="grid gap-3 md:grid-cols-2">
                <label className="space-y-1">
                  <span className="text-xs text-text-tertiary">Runtime enabled</span>
                  <select
                    value={current.runtime_enabled ? 'true' : 'false'}
                    onChange={e => setDraft({ ...current, runtime_enabled: e.target.value === 'true' })}
                    className="w-full rounded border border-border-default bg-surface-1 px-3 py-2 text-sm text-text-primary"
                  >
                    <option value="true">Enabled</option>
                    <option value="false">Disabled</option>
                  </select>
                </label>
                <label className="space-y-1">
                  <span className="text-xs text-text-tertiary">Runtime mode</span>
                  <select
                    value={current.runtime_mode}
                    onChange={e => setDraft({ ...current, runtime_mode: e.target.value as AgentRuntimeSettings['runtime_mode'] })}
                    className="w-full rounded border border-border-default bg-surface-1 px-3 py-2 text-sm text-text-primary"
                  >
                    <option value="observe">Observe</option>
                    <option value="enforce">Enforce</option>
                  </select>
                </label>
                <label className="space-y-1">
                  <span className="text-xs text-text-tertiary">Starter profile</span>
                  <select
                    value={current.starter_profile}
                    onChange={e => setDraft({ ...current, starter_profile: e.target.value })}
                    className="w-full rounded border border-border-default bg-surface-1 px-3 py-2 text-sm text-text-primary"
                  >
                    <option value="none">None</option>
                    <option value="claude_code">Claude Code</option>
                    <option value="codex">Codex</option>
                  </select>
                </label>
                <label className="space-y-1">
                  <span className="text-xs text-text-tertiary">Outbound credential mode</span>
                  <select
                    value={current.outbound_credential_mode}
                    onChange={e => setDraft({ ...current, outbound_credential_mode: e.target.value as AgentRuntimeSettings['outbound_credential_mode'] })}
                    className="w-full rounded border border-border-default bg-surface-1 px-3 py-2 text-sm text-text-primary"
                  >
                    <option value="inherit">Inherit</option>
                    <option value="observe">Observe</option>
                    <option value="strict">Strict</option>
                  </select>
                </label>
              </div>
              <label className="flex items-center gap-2 text-sm text-text-primary">
                <input
                  type="checkbox"
                  checked={current.inject_stored_bearer}
                  onChange={e => setDraft({ ...current, inject_stored_bearer: e.target.checked })}
                />
                Inject stored bearer credentials
              </label>
            </>
          )}
          <div className="flex flex-wrap items-center justify-between gap-3 rounded border border-border-subtle bg-surface-1 px-4 py-3">
            <div className="min-w-0">
              <div className="flex flex-wrap items-center gap-2">
                <div className="text-sm font-medium text-text-primary">Detect raw secrets</div>
                <span className="rounded border border-border-subtle px-2 py-0.5 text-sm uppercase tracking-wider text-text-tertiary">
                  Experimental
                </span>
              </div>
              <div className="mt-1 text-xs text-text-tertiary">
                Scans agent LLM requests for raw secrets and pauses them so you can vault, discard, allow once, or mark them safe.
              </div>
            </div>
            <SwitchControl
              checked={secretDetectionEnabled}
              onChange={checked => setDraft({ ...current, lite_proxy_secret_detection_disabled: !checked })}
              label="Detect raw secrets"
            />
          </div>
          <div className="rounded border border-border-subtle bg-surface-1 px-4 py-3 space-y-2">
            <div className="flex flex-wrap items-center gap-2">
              <div className="text-sm font-medium text-text-primary">Conversation-based auto-approval</div>
            </div>
            <p className="text-xs text-text-tertiary">
              When this agent asks to create a task in response to your message, skip the
              approval prompt if the conversation makes your intent clear and the task's
              risk is at or below this level. Higher levels are not selectable here.
            </p>
            <label className="block space-y-1">
              <span className="text-xs text-text-tertiary">Auto-approve up to</span>
              <select
                value={current.conversation_auto_approve_threshold ?? 'off'}
                onChange={e =>
                  setDraft({
                    ...current,
                    conversation_auto_approve_threshold: e.target.value as 'off' | 'low' | 'medium',
                  })
                }
                className="w-full rounded border border-border-default bg-surface-0 px-3 py-2 text-sm text-text-primary"
              >
                <option value="off">Off — always ask</option>
                <option value="low">Low risk only</option>
                <option value="medium">Low and medium risk</option>
              </select>
            </label>
          </div>
          <div className="flex justify-end">
            <button
              onClick={() => saveMut.mutate(current)}
              disabled={saveMut.isPending}
              className="rounded bg-brand px-4 py-2 text-sm font-medium text-surface-0 hover:bg-brand-strong disabled:opacity-50"
            >
              {saveMut.isPending ? 'Saving…' : 'Save runtime settings'}
            </button>
          </div>
        </div>
      )}
    </div>
  )
}

function SwitchControl({
  checked,
  onChange,
  label,
}: {
  checked: boolean
  onChange: (checked: boolean) => void
  label: string
}) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      aria-label={label}
      onClick={() => onChange(!checked)}
      className={`relative inline-flex h-5 w-9 shrink-0 cursor-pointer rounded-full transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand/30 focus-visible:ring-offset-2 ${
        checked ? 'bg-brand' : 'bg-border-strong'
      }`}
    >
      <span
        className={`pointer-events-none inline-block h-4 w-4 transform rounded-full bg-white shadow transition-transform mt-0.5 ${
          checked ? 'translate-x-[18px] ml-0' : 'translate-x-0.5'
        }`}
      />
    </button>
  )
}

// ── Connect an Agent wizard ──────────────────────────────────────────────────
//
// Step 1: pick the agent (card grid).
// Step 2: install — per-target instructions, with a "back to picker" affordance
//         and an inline copy of the pending connections card so the user can
//         approve without scrolling.
//
// Wizard step is derived from `?agent=<harness>` so it survives reloads, deep
// links land directly on step 2, and the browser back button rewinds the
// wizard naturally.


export function AgentSetupWizardPanel({
  picked,
  onBack,
  newToken = null,
  showBackLink = true,
  onSetupComplete,
}: {
  picked: AgentTab
  onBack: () => void
  newToken?: string | null
  showBackLink?: boolean
  onSetupComplete?: (tab: AgentTab) => void
}) {
  const [searchParams] = useSearchParams()
  const { user, features } = useAuth()
  const proxyLiteUI = !!features?.proxy_lite
  const showSkillDefault = !proxyLiteUI || searchParams.get('mode') === 'skill'
  const [copied, setCopied] = useState(false)

  const { data: pairInfo } = useQuery({
    queryKey: ['pairInfo'],
    queryFn: () => api.devices.pairInfo(),
  })
  const { data: publicConfig } = useQuery({
    queryKey: ['public-config'],
    queryFn: () => api.config.public(),
  })
  const { data: connections } = useQuery({
    queryKey: ['connections'],
    queryFn: () => api.connections.list(),
  })
  const isInstallerWizard = picked === 'hermes' || picked === 'openclaw'
  const pendingForWizard = (connections ?? []).filter(c => c.status === 'pending')
  const showOuterPending = !isInstallerWizard && pendingForWizard.length > 0

  const { data: claim } = useQuery({
    queryKey: ['connection-claim'],
    queryFn: () => api.connections.mintClaim(),
    enabled: proxyLiteUI,
    refetchInterval: 4 * 60 * 1000,
    staleTime: 0,
  })

  const isLocal = window.location.hostname === 'localhost' || window.location.hostname === '127.0.0.1'
  const hasRelay = !!(pairInfo?.daemon_id && pairInfo?.relay_host)

  const clawvisorURL = isLocal
    ? window.location.origin
    : hasRelay
      ? `https://${pairInfo!.relay_host}/d/${pairInfo!.daemon_id}`
      : window.location.origin
  const proxyLiteURL = !isLocal && proxyLiteUI
    ? normalizePublicURL(publicConfig?.proxy_lite_public_url) ?? clawvisorURL
    : clawvisorURL

  const userIdParam = user?.id ? `?user_id=${encodeURIComponent(user.id)}` : ''

  const setupURL = hasRelay
    ? `https://${pairInfo!.relay_host}/d/${pairInfo!.daemon_id}/skill/setup${userIdParam}`
    : `${window.location.origin}/skill/setup${userIdParam}`

  const copyText = (text: string) => {
    navigator.clipboard.writeText(text)
    setCopied(true)
    setTimeout(() => setCopied(false), 2000)
  }

  const handleBack = () => {
    if (picked === 'openclaw' || picked === 'hermes') {
      clearInstallerProgress(picked)
    }
    setCopied(false)
    onBack()
  }

  return (
    <>
      {showBackLink && (
        <button
          onClick={handleBack}
          className="text-xs text-text-tertiary hover:text-text-primary mb-4 inline-flex items-center gap-1"
        >
          ← Choose a different agent
        </button>
      )}

      {proxyLiteUI ? (
        <>
          {picked === 'openclaw' && <InstallerSkillGuide target="openclaw" installerBaseURL={clawvisorURL} claim={claim?.code} userIdParam={userIdParam} onCopy={copyText} />}
          {picked === 'hermes' && <InstallerSkillGuide target="hermes" installerBaseURL={clawvisorURL} claim={claim?.code} userIdParam={userIdParam} onCopy={copyText} />}
          {picked === 'claude-code' && <ManualProxyCLISetupGuide target="claude-code" clawvisorURL={clawvisorURL} llmBaseURL={proxyLiteURL} claim={claim?.code} onCopy={copyText} />}
          {picked === 'codex' && <ManualProxyCLISetupGuide target="codex" clawvisorURL={clawvisorURL} llmBaseURL={proxyLiteURL} claim={claim?.code} onCopy={copyText} />}
          {picked === 'claude-desktop' && <ClaudeDesktopProfileGuide />}
          {picked === 'gbrain' && <GBrainStreamlinedGuide clawvisorURL={clawvisorURL} onCopy={copyText} />}
          {picked === 'cloud-agent' && <CloudAgentPromptGuide setupURL={setupURL} clawvisorURL={clawvisorURL} copied={copied} onCopy={copyText} />}
          {picked === 'other' && <OtherAgentGuide setupURL={setupURL} clawvisorURL={clawvisorURL} llmBaseURL={proxyLiteURL} claim={claim?.code} newToken={newToken} copied={copied} onCopy={copyText} showSkillDefault={showSkillDefault} />}
        </>
      ) : (
        <>
          {picked === 'openclaw' && (
            <LegacyOpenClawGuide
              setupURL={setupURL}
              copied={copied}
              onCopy={copyText}
            />
          )}
          {picked === 'claude-code' && <LegacyClaudeCodeGuide clawvisorURL={clawvisorURL} userIdParam={userIdParam} onCopy={copyText} />}
          {picked === 'claude-desktop' && <LegacyClaudeDesktopGuide isLocal={isLocal} onCopy={copyText} />}
          {picked === 'other' && <LegacyOtherAgentGuide setupURL={setupURL} clawvisorURL={clawvisorURL} copied={copied} onCopy={copyText} />}
        </>
      )}

      {showOuterPending && (
        <div className="mt-6 pt-5 border-t border-border-subtle">
          <div className="flex items-center gap-2 mb-3">
            <h3 className="text-sm font-medium text-text-primary">Pending Connections</h3>
            <span className="bg-warning text-surface-0 text-xs font-bold rounded px-2 py-0.5 font-mono">
              {pendingForWizard.length}
            </span>
          </div>
          <div className="space-y-3">
            {pendingForWizard.map(cr => (
              <ConnectionCard key={cr.id} request={cr} />
            ))}
          </div>
        </div>
      )}
    </>
  )
}

function AgentSetupBreadcrumbs({ current }: { current: string }) {
  return (
    <nav aria-label="Breadcrumb" className="flex items-center gap-2 text-sm">
      <Link to="/dashboard/agents" className="text-brand hover:underline">
        Agents
      </Link>
      <span className="text-text-tertiary" aria-hidden>/</span>
      <span className="text-text-primary">{current}</span>
    </nav>
  )
}

const HARNESS_SETUP_INTRO: Partial<Record<AgentTab, string>> = {
  openclaw:
    'OpenClaw is an open-source agent framework built on Claude Code. This wizard registers your instance with Clawvisor, vaults an upstream API key, and installs a helper skill that routes LLM calls through the proxy — so every request is logged and gated by your policy.',
}

function AgentHarnessSetupView({ harness }: { harness: AgentTab | null }) {
  const navigate = useNavigate()

  if (!harness) {
    return (
      <div className="page-shell">
        <AgentSetupBreadcrumbs current="Setup" />
        <div className="rounded-md border border-border-default bg-surface-1 p-6 text-sm text-text-tertiary">
          Unknown agent type.{' '}
          <Link to="/dashboard/agents" className="text-brand hover:underline">
            Back to agents
          </Link>
        </div>
      </div>
    )
  }

  const meta = AGENT_META[harness]

  return (
    <div className="page-shell">
      <AgentSetupBreadcrumbs current={meta.label} />
      <PageHeader
        title={`Connect ${meta.label}`}
        meta={HARNESS_SETUP_INTRO[harness] ?? meta.tagline}
      />
      <section className="bg-surface-1 border border-border-default rounded-md p-5">
        <AgentSetupWizardPanel
          picked={harness}
          onBack={() => navigate('/dashboard/agents')}
          showBackLink={false}
        />
      </section>
    </div>
  )
}

function ConnectAgentGuide({ connectedAgents = [] }: { connectedAgents?: Agent[] }) {
  return (
    <section className="bg-surface-1 border border-border-default rounded-md overflow-hidden">
      <div className="px-5 pt-5 pb-4">
        <h2 className="text-lg font-semibold text-text-primary">Connect an Agent</h2>
      </div>
      <div className="p-5 pt-0">
        <AgentPickerContent connectedAgents={connectedAgents} />
      </div>
    </section>
  )
}

function StepNumber({ n }: { n: number }) {
  return (
    <span className="flex-shrink-0 w-6 h-6 rounded-full bg-brand/10 text-brand text-xs font-bold flex items-center justify-center">
      {n}
    </span>
  )
}

function SetupStepNumber({ n, variant }: { n: number; variant: 'active' | 'upcoming' | 'complete' }) {
  const className = variant === 'active'
    ? 'flex-shrink-0 w-6 h-6 rounded-full bg-brand text-surface-0 text-xs font-bold flex items-center justify-center ring-2 ring-brand/25'
    : variant === 'complete'
      ? 'flex-shrink-0 w-6 h-6 rounded-full bg-success/15 text-success border border-success/30 text-xs font-bold flex items-center justify-center'
      : 'flex-shrink-0 w-6 h-6 rounded-full bg-transparent text-text-tertiary border border-dashed border-border-default text-xs font-bold flex items-center justify-center'

  return (
    <span className={className}>
      {variant === 'complete' ? (
        <svg className="w-3.5 h-3.5" fill="none" stroke="currentColor" strokeWidth="2.5" viewBox="0 0 24 24">
          <path d="M5 13l4 4L19 7" />
        </svg>
      ) : (
        n
      )}
    </span>
  )
}

function SetupStepCopyHandle({ value, label }: { value: string; label: string }) {
  const [copied, setCopied] = useState(false)

  function copy(e: React.MouseEvent) {
    e.preventDefault()
    e.stopPropagation()
    navigator.clipboard.writeText(value).then(() => {
      setCopied(true)
      setTimeout(() => setCopied(false), 1500)
    })
  }

  return (
    <button
      type="button"
      onClick={copy}
      title={copied ? 'Copied' : `Copy ${label}`}
      aria-label={copied ? 'Copied' : `Copy ${label}`}
      className="dev-pick-copy shrink-0 self-start opacity-0 group-hover:opacity-100 group-focus-within:opacity-100 focus:opacity-100 transition-opacity"
    >
      {copied ? (
        <svg className="w-3.5 h-3.5 text-success" fill="none" stroke="currentColor" strokeWidth="2.5" viewBox="0 0 24 24" aria-hidden>
          <path d="M5 13l4 4L19 7" />
        </svg>
      ) : (
        <svg className="w-3.5 h-3.5" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24" aria-hidden>
          <rect x="9" y="9" width="13" height="13" rx="2" ry="2" />
          <path d="M5 15H4a2 2 0 01-2-2V4a2 2 0 012-2h9a2 2 0 012 2v1" />
        </svg>
      )}
    </button>
  )
}

function SetupGuideStep({
  step,
  title,
  description,
  completedSummary,
  variant,
  children,
  compactTop = false,
  copyValue,
  copyLabel,
}: {
  step: number
  title: string
  description?: ReactNode
  completedSummary?: ReactNode
  variant: 'active' | 'upcoming' | 'complete'
  children: ReactNode
  compactTop?: boolean
  copyValue?: string
  copyLabel?: string
}) {
  const [userToggledOpen, setUserToggledOpen] = useState(false)
  const canToggle = variant !== 'active'
  const isExpanded = variant === 'active' || userToggledOpen

  useEffect(() => {
    if (variant === 'active') setUserToggledOpen(true)
    else if (variant === 'complete') setUserToggledOpen(false)
  }, [variant])

  const paddingClass = compactTop ? 'px-4 pt-2 pb-4' : 'p-4'
  const shellClass = variant === 'active'
    ? `group rounded-md border border-brand/30 bg-surface-1 ${paddingClass}`
    : variant === 'complete'
      ? `group rounded-md border border-border-default bg-surface-2/40 ${isExpanded ? paddingClass : 'p-4'}`
      : `group rounded-md border border-border-default ${isExpanded ? paddingClass : 'p-4'}`

  const titleClass = variant === 'upcoming' ? 'text-text-secondary' : 'text-text-primary'

  const headerInner = (
    <>
      <div className="flex w-6 shrink-0 flex-col items-center pt-0.5">
        <SetupStepNumber n={step} variant={variant} />
      </div>
      <div className="min-w-0 flex-1 space-y-1">
        <p className={`text-sm font-medium ${titleClass}`}>{title}</p>
        {variant === 'complete' && !isExpanded && completedSummary ? (
          <div className="text-sm text-text-secondary leading-relaxed">{completedSummary}</div>
        ) : description && (!isExpanded || variant === 'active') && (
          <div className="text-sm text-text-secondary leading-relaxed">{description}</div>
        )}
      </div>
      <div className="shrink-0 self-start flex items-center gap-0.5">
        {copyValue && (
          <SetupStepCopyHandle value={copyValue} label={copyLabel ?? title} />
        )}
        {canToggle && (
          <span className="p-1 text-text-tertiary" aria-hidden>
            <svg
              className={`h-4 w-4 transition-transform ${isExpanded ? 'rotate-90' : ''}`}
              fill="none"
              stroke="currentColor"
              strokeWidth="2"
              viewBox="0 0 24 24"
            >
              <path d="M9 6l6 6-6 6" />
            </svg>
          </span>
        )}
      </div>
    </>
  )

  return (
    <div className={shellClass}>
      {canToggle ? (
        <button
          type="button"
          aria-expanded={isExpanded}
          onClick={() => setUserToggledOpen(open => !open)}
          className="flex w-full items-start gap-3 text-left hover:bg-surface-2/50 rounded-md -m-1 p-1 transition-colors"
        >
          {headerInner}
        </button>
      ) : (
        <div className="flex items-start gap-3">{headerInner}</div>
      )}
      {isExpanded && children && (
        <div className="mt-3 min-w-0 space-y-3 pl-9">
          {children}
        </div>
      )}
    </div>
  )
}

function CodeBlock({ children, onCopy }: { children: string; onCopy?: () => void }) {
  const [copied, setCopied] = useState(false)
  const handleCopy = () => {
    if (!onCopy) return
    onCopy()
    setCopied(true)
    window.setTimeout(() => setCopied(false), 1500)
  }
  return (
    <div className="bg-surface-0 border border-border-subtle rounded overflow-hidden">
      <pre className="px-3 py-2.5 text-xs font-mono text-text-primary overflow-x-auto whitespace-pre-wrap break-all">
        {children}
      </pre>
      {onCopy && (
        <div className="border-t border-border-subtle bg-surface-1/60 px-2 py-1.5 flex justify-end">
          <button
            onClick={handleCopy}
            className="inline-flex items-center gap-1.5 text-xs font-medium px-2.5 py-1 rounded border border-border-default text-text-secondary hover:text-text-primary hover:bg-surface-0"
          >
            <span aria-hidden="true">{copied ? '✓' : '⧉'}</span>
            {copied ? 'Copied' : 'Copy'}
          </button>
        </div>
      )}
    </div>
  )
}

// Renders a compact opt-in checkbox that toggles
// `--dangerously-skip-permissions` (Claude Code) or its Codex equivalent into
// the test-connection and alias commands above. The flag is dangerous on
// purpose — the label spells out what's being bypassed so users can't
// flip it accidentally and then forget. Kept as a thin wrapper around a
// native `<input type="checkbox">` so it inherits the form-control styling
// the dashboard already ships.
function SkipPermissionsCheckbox({
  checked,
  onChange,
  flag,
  label,
}: {
  checked: boolean
  onChange: (next: boolean) => void
  flag: string
  label: string
}) {
  return (
    <label className="flex items-start gap-2 text-xs text-text-secondary cursor-pointer select-none">
      <input
        type="checkbox"
        checked={checked}
        onChange={e => onChange(e.target.checked)}
        className="mt-0.5 h-3.5 w-3.5 rounded border-border-default text-brand focus:ring-brand/30"
      />
      <span>
        Skip permission prompts in {label} (
        <code className="font-mono text-text-secondary">{flag}</code>
        ). Dangerous — the agent will run shell commands without asking you first.
      </span>
    </label>
  )
}

function LegacyClaudeCodeGuide({ clawvisorURL, userIdParam, onCopy }: {
  clawvisorURL: string
  userIdParam: string
  onCopy: (text: string) => void
}) {
  const installCmd = `curl -sf "${clawvisorURL}/skill/clawvisor-setup.md${userIdParam}" \\\n  --create-dirs -o ~/.claude/commands/clawvisor-setup.md`

  return (
    <div className="space-y-5">
      <p className="text-sm text-text-secondary">
        Install a slash command, then run it in Claude Code. It handles agent registration,
        skill installation, environment setup, and a smoke test — all interactively.
      </p>

      <div className="flex items-start gap-3">
        <StepNumber n={1} />
        <div className="space-y-1.5 min-w-0 flex-1">
          <p className="text-sm font-medium text-text-primary">Install the setup command</p>
          <p className="text-xs text-text-tertiary">
            Run this in your terminal to install the{' '}
            <code className="font-mono text-text-secondary">/clawvisor-setup</code> slash command:
          </p>
          <CodeBlock onCopy={() => onCopy(installCmd)}>{installCmd}</CodeBlock>
        </div>
      </div>

      <div className="flex items-start gap-3">
        <StepNumber n={2} />
        <div className="space-y-1.5 min-w-0 flex-1">
          <p className="text-sm font-medium text-text-primary">Run /clawvisor-setup in Claude Code</p>
          <p className="text-xs text-text-tertiary">
            Open Claude Code and type{' '}
            <code className="font-mono text-text-secondary">/clawvisor-setup</code>.
            Claude will walk you through the setup — registering as an agent, configuring
            environment variables, and verifying the connection.
          </p>
        </div>
      </div>

      <div className="flex items-start gap-3">
        <StepNumber n={3} />
        <div className="space-y-1.5 min-w-0 flex-1">
          <p className="text-sm font-medium text-text-primary">Approve the connection</p>
          <p className="text-xs text-text-tertiary">
            During setup, Claude Code sends a connection request. Approve it in the{' '}
            <strong>Pending Connections</strong> section above. Once approved, Claude Code
            finishes setup automatically and runs a smoke test.
          </p>
        </div>
      </div>
    </div>
  )
}

function LegacyClaudeDesktopGuide({ isLocal, onCopy }: { isLocal: boolean; onCopy: (text: string) => void }) {
  const marketplaceSlug = 'clawvisor/cowork-plugins'
  const pluginLabel = isLocal ? 'Clawvisor Local' : 'Clawvisor'

  return (
    <div className="space-y-5">
      <p className="text-sm text-text-secondary">
        {isLocal
          ? 'Connect Claude Cowork to your local Clawvisor instance via the Cowork plugin.'
          : 'Connect Claude Cowork to your Clawvisor cloud account via the Cowork plugin.'}
      </p>

      <div className="flex items-start gap-3">
        <StepNumber n={1} />
        <div className="space-y-1.5 min-w-0 flex-1">
          <p className="text-sm font-medium text-text-primary">Open the plugin manager</p>
          <p className="text-xs text-text-tertiary">
            In Claude Desktop, navigate to <strong>Claude Cowork</strong>, click{' '}
            <strong>Customize</strong> in the sidebar, then press the <strong>+</strong> next to{' '}
            <strong>Personal plugins</strong>.
          </p>
        </div>
      </div>

      <div className="flex items-start gap-3">
        <StepNumber n={2} />
        <div className="space-y-1.5 min-w-0 flex-1">
          <p className="text-sm font-medium text-text-primary">Add the marketplace</p>
          <p className="text-xs text-text-tertiary">
            Under <strong>Create plugin</strong>, select <strong>Add marketplace</strong> and paste:
          </p>
          <CodeBlock onCopy={() => onCopy(marketplaceSlug)}>{marketplaceSlug}</CodeBlock>
        </div>
      </div>

      <div className="flex items-start gap-3">
        <StepNumber n={3} />
        <div className="space-y-1.5 min-w-0 flex-1">
          <p className="text-sm font-medium text-text-primary">Install the {pluginLabel} plugin</p>
          <p className="text-xs text-text-tertiary">
            Open the <strong>Personal</strong> tab, switch to the <strong>cowork-plugins</strong> tab,
            then select <strong>{pluginLabel}</strong> to install.
          </p>
        </div>
      </div>

      {!isLocal && (
        <div className="flex items-start gap-3">
          <StepNumber n={4} />
          <div className="space-y-1.5 min-w-0 flex-1">
            <p className="text-sm font-medium text-text-primary">Connect the Clawvisor connector</p>
            <p className="text-xs text-text-tertiary">
              Under the <strong>Clawvisor</strong> plugin, select <strong>Connectors</strong>, click the{' '}
              <strong>clawvisor</strong> connector, and connect. Authorize Claude Cowork in your browser
              when prompted.
            </p>
          </div>
        </div>
      )}

      <div className="flex items-start gap-3">
        <StepNumber n={isLocal ? 4 : 5} />
        <div className="space-y-1.5 min-w-0 flex-1">
          <p className="text-sm font-medium text-text-primary">Start using it</p>
          <p className="text-xs text-text-tertiary">
            Create a new Claude Cowork session and ask your agent to use a connected account via
            Clawvisor — e.g. "check my Gmail" or "list my GitHub issues." Claude will create a task,
            ask you to approve, and execute through Clawvisor.{' '}
            {isLocal &&
              <>Open the dashboard with <code className="font-mono text-text-secondary">clawvisor tui</code> or visit <code className="font-mono text-text-secondary">http://localhost:25297</code> to manage services, approvals, and restrictions.</>
            }
          </p>
        </div>
      </div>
    </div>
  )
}

function LegacyPromptBlock({ prompt, copied, onCopy }: { prompt: string; copied: boolean; onCopy: (text: string) => void }) {
  return (
    <div className="relative group bg-surface-0 border border-brand/20 rounded overflow-hidden">
      <pre className="px-3 py-2.5 sm:pr-16 text-xs font-mono text-text-primary overflow-x-auto whitespace-pre-wrap break-words">
        {prompt}
      </pre>
      <button
        onClick={() => onCopy(prompt)}
        className="hidden sm:block absolute top-2 right-2 text-xs px-2 py-1 rounded border border-border-subtle bg-surface-1 text-text-secondary hover:text-text-primary hover:bg-surface-2"
      >
        {copied ? 'Copied' : 'Copy'}
      </button>
      <div className="sm:hidden border-t border-brand/20 px-3 py-1.5 flex justify-end">
        <button
          onClick={() => onCopy(prompt)}
          className="text-xs px-2.5 py-1 rounded border border-border-subtle bg-surface-1 text-text-secondary hover:text-text-primary hover:bg-surface-2"
        >
          {copied ? 'Copied' : 'Copy'}
        </button>
      </div>
    </div>
  )
}

const PASTE_STEP_ADVANCE_MS = 5_000

function AgentNameInput({
  agentName,
  onChange,
  disabled,
  hint,
}: {
  agentName: string
  onChange: (n: string) => void
  disabled?: boolean
  hint?: React.ReactNode
}) {
  return (
    <div>
      <label htmlFor="agent-setup-name" className="text-xs uppercase tracking-wider text-text-tertiary">
        Name this agent
      </label>
      <input
        id="agent-setup-name"
        type="text"
        value={agentName}
        onChange={e => onChange(sanitizeAgentName(e.target.value))}
        disabled={disabled}
        className="mt-1 block w-full text-sm font-mono rounded border border-border-default bg-surface-0 text-text-primary px-3 py-1.5 focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand disabled:opacity-60"
      />
      {hint && <p className="text-xs text-text-tertiary mt-1">{hint}</p>}
    </div>
  )
}

const OPENCLAW_DEPLOYMENT_LABELS = {
  host: 'On this machine',
  docker: 'In Docker on this machine',
  remote: 'On another machine',
} as const

const CREDENTIAL_SCOPE_SUMMARY = {
  user: 'User-level',
  agent: 'Agent-specific',
} as const

function LegacyOpenClawGuide({ setupURL, copied, onCopy }: {
  setupURL: string
  copied: boolean
  onCopy: (text: string) => void
}) {
  const helperCopyTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null)
  const [helperCopyWatching, setHelperCopyWatching] = useState(false)
  const [prototypeTrafficDetected, setPrototypeTrafficDetected] = useState(false)
  const [answers, setAnswers] = useState<InstallerAnswers>(() => defaultInstallerAnswers('openclaw'))
  const [setupQuestionsDone, setSetupQuestionsDone] = useState(false)
  const [credentialScope, setCredentialScope] = useState<CredentialScope>('user')
  const [apiKeyAcknowledged, setApiKeyAcknowledged] = useState(false)
  const [keySavedForContinue, setKeySavedForContinue] = useState(false)
  const [helper, setHelper] = useState<InstallerHelper>('claude')
  const helperStartedAtRef = useRef(Date.now())

  const installerBaseURL = useMemo(
    () => setupURL.split('/skill/setup')[0],
    [setupURL],
  )

  const { data: agents } = useQuery({
    queryKey: ['agents', 'personal'],
    queryFn: () => api.agents.list(),
  })
  const [agentName, setAgentName] = useSequencedAgentName('openclaw-1', agents)
  const approvedAgent = useMemo(
    () => agents?.find(a => a.name === agentName),
    [agents, agentName],
  )

  const nameTaken = !!agents?.find(a => a.name === agentName)
  useEffect(() => () => {
    if (helperCopyTimerRef.current) clearTimeout(helperCopyTimerRef.current)
  }, [])

  useEffect(() => {
    setApiKeyAcknowledged(false)
    setKeySavedForContinue(false)
  }, [answers.llmProvider, credentialScope])

  const { apiKeyReady } = useUpstreamKeyReadiness(
    answers.llmProvider,
    credentialScope,
    approvedAgent?.id,
  )
  const canContinueFromKeyStep = apiKeyReady || keySavedForContinue
  const apiKeyStepDone = canContinueFromKeyStep && apiKeyAcknowledged

  const skillURL = useMemo(() => {
    const params = new URLSearchParams(setupURL.includes('?') ? setupURL.split('?')[1] : '')
    applyInstallerAnswerParams(params, 'openclaw', answers)
    if (agentName && agentName !== 'openclaw-1') params.set('agent_name', agentName)
    const qs = params.toString()
    return `${installerBaseURL}/skill/install/openclaw.md${qs ? `?${qs}` : ''}`
  }, [installerBaseURL, setupURL, answers, agentName])

  const helperCommand = useMemo(
    () => buildHelperCommand({ skillURL, helper }),
    [skillURL, helper],
  )

  useEffect(() => {
    if (setupQuestionsDone) helperStartedAtRef.current = Date.now()
  }, [setupQuestionsDone])

  const pollingHelper = setupQuestionsDone
  const { data: helperSessions } = useQuery({
    queryKey: ['runtime-sessions', 'legacy-openclaw-helper', approvedAgent?.id ?? 'none'],
    queryFn: () => api.runtime.listSessions(),
    enabled: pollingHelper,
    refetchInterval: pollingHelper ? 3000 : false,
    retry: false,
  })
  const helperLiveSession = useMemo(
    () => (helperSessions?.entries ?? []).find(
      s => s.agent_id === approvedAgent?.id && isActiveRuntimeSession(s),
    ),
    [approvedAgent?.id, helperSessions],
  )
  const helperStartActivity = useAgentStartActivity(
    approvedAgent?.id,
    helperStartedAtRef.current,
    pollingHelper,
  )
  const trafficDetected = !!helperLiveSession || !!helperStartActivity || prototypeTrafficDetected
  const helperStepDone = trafficDetected || !!approvedAgent?.id
  const verifyReady = apiKeyStepDone

  const handleHelperCopy = (text: string) => {
    onCopy(text)
    setPrototypeTrafficDetected(false)
    setHelperCopyWatching(true)
    if (helperCopyTimerRef.current) clearTimeout(helperCopyTimerRef.current)
    helperCopyTimerRef.current = setTimeout(() => {
      setHelperCopyWatching(false)
      setPrototypeTrafficDetected(true)
      helperCopyTimerRef.current = null
    }, PASTE_STEP_ADVANCE_MS)
  }

  const setupQuestionsSatisfied = setupQuestionsDone
  const step1Variant = setupQuestionsSatisfied ? 'complete' : 'active'
  const step2Variant = helperStepDone
    ? 'complete'
    : setupQuestionsSatisfied
      ? 'active'
      : 'upcoming'
  const step3Variant = apiKeyStepDone
    ? 'complete'
    : helperStepDone
      ? 'active'
      : 'upcoming'
  const step4Variant = verifyReady ? 'active' : 'upcoming'

  return (
    <div className="relative space-y-5">
      <AgentNameInput
        agentName={agentName}
        onChange={setAgentName}
        hint={
          nameTaken
            ? <>An agent named <strong className="text-text-secondary">{agentName}</strong> already exists — pick a different name for a new connection.</>
            : undefined
        }
      />

      <div className="space-y-3">
        <SetupGuideStep
          step={1}
          title="Where is this agent running?"
          description="These answers are baked into the installer skill URL, so the helper follows your preferences instead of asking again."
          completedSummary={
            <>
              {providerLabel(answers.llmProvider)} · {OPENCLAW_DEPLOYMENT_LABELS[answers.openclawMode]}
            </>
          }
          variant={step1Variant}
        >
          <InstallerSetupQuestions
            target="openclaw"
            answers={answers}
            onChange={setAnswers}
            showTitle={false}
          />
          {!setupQuestionsSatisfied ? (
            <WizardNav
              canBack={false}
              canNext
              onBack={() => {}}
              onNext={() => setSetupQuestionsDone(true)}
              nextLabel="Continue"
              showTopBorder={false}
              alignNext="left"
            />
          ) : (
            <button
              type="button"
              onClick={() => setSetupQuestionsDone(false)}
              className="text-xs text-brand hover:underline"
            >
              Change answers
            </button>
          )}
        </SetupGuideStep>

        <SetupGuideStep
          step={2}
          title="Install and run the helper"
          description="Run the installer skill in Claude Code or Codex so it reads the token from disk and finishes configuring OpenClaw."
          completedSummary={
            <>
              {INSTALLER_HELPERS[helper].pillLabel} · routed activity detected
            </>
          }
          variant={step2Variant}
          copyValue={setupQuestionsSatisfied ? helperCommand : undefined}
          copyLabel="helper command"
        >
          {setupQuestionsSatisfied ? (
            <InstallHelperStepPanel
              helper={helper}
              onHelperChange={setHelper}
              helperCommand={helperCommand}
              skillPreviewUrl={skillURL}
              frameworkLabel="OpenClaw"
              onCopy={handleHelperCopy}
              copied={copied}
              agentName={approvedAgent?.name ?? agentName}
              liveSession={helperLiveSession}
              startActivity={helperStartActivity}
              watching={helperCopyWatching}
              prototypeDetected={prototypeTrafficDetected}
            />
          ) : (
            <p className="text-xs text-text-tertiary">
              Complete the setup questions above first — this step unlocks once you continue.
            </p>
          )}
        </SetupGuideStep>

        <SetupGuideStep
          step={3}
          title={`Set the ${providerLabel(answers.llmProvider)} key`}
          description={<>Vault an upstream {providerLabel(answers.llmProvider)} key so Clawvisor can swap it in when {agentName} calls through the proxy.</>}
          completedSummary={
            <>
              {CREDENTIAL_SCOPE_SUMMARY[credentialScope]} {providerLabel(answers.llmProvider)} key
            </>
          }
          variant={step3Variant}
        >
          {helperStepDone ? (
            <>
              <AgentUpstreamKeySetupPanel
                agentName={approvedAgent?.name ?? agentName}
                agentId={approvedAgent?.id}
                provider={answers.llmProvider}
                credentialScope={credentialScope}
                onCredentialScopeChange={setCredentialScope}
                prototypeSaveUnlock
                onKeySaved={() => setKeySavedForContinue(true)}
              />
              <WizardNav
                canBack={false}
                canNext={canContinueFromKeyStep}
                onBack={() => {}}
                onNext={() => setApiKeyAcknowledged(true)}
                nextLabel="Continue"
                showTopBorder={false}
                alignNext="left"
                nextDisabledHint={canContinueFromKeyStep
                  ? undefined
                  : apiKeyContinueHint(answers.llmProvider, credentialScope, false)}
              />
            </>
          ) : (
            <p className="text-xs text-text-tertiary">
              Run the installer helper above first — this step unlocks once your agent is registered.
            </p>
          )}
        </SetupGuideStep>

        <SetupGuideStep
          step={4}
          title="Test out your agent!"
          variant={step4Variant}
        >
          <AgentSetupTryItPanel preview={!verifyReady} />
        </SetupGuideStep>
      </div>
    </div>
  )
}

function LegacyOtherAgentGuide({ setupURL, clawvisorURL, copied, onCopy }: {
  setupURL: string
  clawvisorURL: string
  copied: boolean
  onCopy: (text: string) => void
}) {
  const [credentialScope, setCredentialScope] = useState<CredentialScope>('user')
  const [llmProvider, setLlmProvider] = useState<LLMProvider>('openai')
  const [promptAcknowledged, setPromptAcknowledged] = useState(false)
  const { data: agents } = useQuery({
    queryKey: ['agents', 'personal'],
    queryFn: () => api.agents.list(),
    refetchInterval: 3000,
  })
  const [agentName] = useSequencedAgentName('my-agent', agents)
  const connectedAgent = agents?.find(a => a.name === agentName)

  const prompt = `Please install Clawvisor. It's a security gateway between you and external services like Gmail, Slack, and GitHub. You don't hold any API keys directly; instead, you make requests through Clawvisor and I approve which actions you can take. Every call is logged, and I can revoke access at any time.\n\nSetup is just registering an agent token and installing a skill that teaches you how to use it. I'll review each step before it happens.\n\nInstructions: ${setupURL}`

  const step1Variant = promptAcknowledged ? 'complete' : 'active'
  const step2Variant = connectedAgent
    ? 'complete'
    : promptAcknowledged
      ? 'active'
      : 'upcoming'
  const step3Variant = connectedAgent ? 'active' : 'upcoming'

  return (
    <div className="space-y-5">
      <p className="text-sm text-text-secondary">
        Any agent that can make HTTP requests can connect to Clawvisor. The fastest way is to paste the setup
        prompt below directly into your agent's chat — it will self-register and wait for your approval.
      </p>

      <div className="space-y-3">
        <SetupGuideStep
          step={1}
          title="Paste this into your agent"
          description="The agent will follow the setup instructions at that URL — it registers itself, sets up E2E encryption, and installs the Clawvisor skill."
          completedSummary="Setup prompt copied"
          variant={step1Variant}
          copyValue={prompt}
          copyLabel="setup prompt"
        >
          <LegacyPromptBlock prompt={prompt} copied={copied} onCopy={onCopy} />
          {!promptAcknowledged && (
            <WizardNav
              canBack={false}
              canNext
              onBack={() => {}}
              onNext={() => setPromptAcknowledged(true)}
              nextLabel="Continue"
              showTopBorder={false}
              alignNext="left"
            />
          )}
        </SetupGuideStep>

        <SetupGuideStep
          step={2}
          title="Approve the connection"
          description={<>A connection request will appear in the <strong>Pending Connections</strong> section above. Click <strong>Approve</strong> to grant the agent a token.</>}
          completedSummary={connectedAgent ? <>Registered as {connectedAgent.name}</> : undefined}
          variant={step2Variant}
        >
          {promptAcknowledged ? (
            <p className="text-xs text-text-tertiary">
              {connectedAgent
                ? <>Agent <strong className="text-text-secondary">{connectedAgent.name}</strong> is connected — continue to vault your API key.</>
                : 'Waiting for your agent to register. Approve the pending connection when it appears above.'}
            </p>
          ) : (
            <p className="text-xs text-text-tertiary">
              Paste the setup prompt into your agent first — this step unlocks once you continue.
            </p>
          )}
        </SetupGuideStep>

        <SetupGuideStep
          step={3}
          title="Set the API key"
          description="Vault an upstream key so Clawvisor can swap it in when your agent calls through the proxy."
          variant={step3Variant}
        >
          {connectedAgent ? (
            <>
              <QuestionToggleGroup
                label="Which LLM provider does this agent use?"
                value={llmProvider}
                onChange={value => setLlmProvider(value as LLMProvider)}
                options={[
                  ['openai', 'OpenAI'],
                  ['anthropic', 'Anthropic'],
                ]}
              />
              <AgentUpstreamKeySetupPanel
                agentName={connectedAgent.name}
                agentId={connectedAgent.id}
                provider={llmProvider}
                credentialScope={credentialScope}
                onCredentialScopeChange={setCredentialScope}
              />
            </>
          ) : (
            <p className="text-xs text-text-tertiary">
              Approve the connection first — this step unlocks once your agent is registered
              {agentName !== 'my-agent' ? ` as ${agentName}` : ''}.
            </p>
          )}
        </SetupGuideStep>
      </div>

      <details className="group">
        <summary className="text-sm font-medium text-text-secondary cursor-pointer hover:text-text-primary select-none">
          Manual setup (token + environment variables)
        </summary>
        <div className="mt-4 space-y-4 pl-0">
          <div className="flex items-start gap-3">
            <StepNumber n={1} />
            <div className="space-y-1.5 min-w-0 flex-1">
              <p className="text-sm font-medium text-text-primary">Create an agent token</p>
              <p className="text-xs text-text-tertiary">
                Use the <strong>Create Agent</strong> form above. Copy the token — it's shown only once.
              </p>
            </div>
          </div>

          <div className="flex items-start gap-3">
            <StepNumber n={2} />
            <div className="space-y-1.5 min-w-0 flex-1">
              <p className="text-sm font-medium text-text-primary">Configure environment variables</p>
              <p className="text-xs text-text-tertiary">
                Set these in your agent's environment (<code className="font-mono text-text-secondary">.env</code>, shell profile, container config, etc.):
              </p>
              <CodeBlock>{`CLAWVISOR_URL=${clawvisorURL}\nCLAWVISOR_AGENT_TOKEN=<your token>`}</CodeBlock>
            </div>
          </div>

          <div className="flex items-start gap-3">
            <StepNumber n={3} />
            <div className="space-y-1.5 min-w-0 flex-1">
              <p className="text-sm font-medium text-text-primary">Verify</p>
              <CodeBlock>{`curl -sf -H "X-Clawvisor-Agent-Token: $CLAWVISOR_AGENT_TOKEN" \\\n  "$CLAWVISOR_URL/api/skill/catalog" | head -20`}</CodeBlock>
              <p className="text-xs text-text-tertiary">
                Should return a JSON catalog of available services. See{' '}
                <code className="font-mono text-text-secondary">{clawvisorURL}/skill/SKILL.md</code>{' '}
                for the full protocol reference.
              </p>
            </div>
          </div>
        </div>
      </details>
    </div>
  )
}

// Restrict agent names to characters that round-trip cleanly through a
// filesystem path, a shell single-quoted JSON body, and a URL. Spaces
// become dashes; other characters drop. Matches the daemon's collision
// check by exact-string equality, so what the user types is what the
// daemon stores.
function sanitizeAgentName(input: string): string {
  return input
    .replace(/\s+/g, '-')
    .replace(/[^a-zA-Z0-9_.-]/g, '')
    .slice(0, 64)
}

// Resolve a collision-free version of base by trying base, base-0,
// base-1, … against the agents list. Returns base itself when no
// existing agent matches.
function nextAvailableName(base: string, agents: Agent[] | undefined): string {
  if (!agents) return base
  const taken = new Set(agents.map(a => a.name))
  if (!taken.has(base)) return base
  for (let i = 0; i < 1000; i++) {
    const candidate = `${base}-${i}`
    if (!taken.has(candidate)) return candidate
  }
  // Fallback for the absurd case of 1000 agents with the same base. The
  // dashboard would have other problems by this point.
  return `${base}-${Date.now()}`
}

// useSequencedAgentName initializes agentName to a collision-free variant
// of base. The auto-rename runs at most once and only if the user hasn't
// already typed something; otherwise we'd clobber their input when
// `agents` resolves async (mount → effect early-returns because agents is
// undefined → user types "my-name" → agents resolves → effect fires → name
// overwritten back to "codex-0").
function useSequencedAgentName(base: string, agents: Agent[] | undefined): [string, (n: string) => void] {
  const [name, setName] = useState(base)
  const sequenced = useRef(false)
  const touched = useRef(false)
  useEffect(() => {
    if (sequenced.current || touched.current || !agents) return
    sequenced.current = true
    const next = nextAvailableName(base, agents)
    if (next !== base) setName(next)
  }, [agents, base])
  const setAndMarkTouched = (next: string) => {
    touched.current = true
    setName(next)
  }
  return [name, setAndMarkTouched]
}

function normalizePublicURL(url: string | null | undefined): string | null {
  const trimmed = url?.trim().replace(/\/+$/, '')
  return trimmed || null
}

function buildBootstrapCommand(clawvisorURL: string, claim: string | undefined, agentName: string, harness?: string): string {
  // Name and claim ride on the URL so the curl is body-less — no -H, no -d.
  // The claim code (minted by an authenticated dashboard session) attributes
  // this curl to the user without leaking user_id into the URL. mkdir + chmod
  // bracket the curl so the file lands with tight perms; -sf makes curl exit
  // non-zero on a 4xx (duplicate-name 409, expired-claim 401, etc.) and
  // --remove-on-error guarantees the partial/error body never lands on disk.
  // Without --remove-on-error, a failed retry would silently overwrite the
  // previous good JSON with the error response.
  //
  // `URLSearchParams` handles URL-encoding so a future claim format that
  // contains `&` / `=` / `#` / space doesn't silently break the curl. The
  // newer `buildConnectCommand` uses the same pattern.
  //
  // `harness` is the install-context tag the server stamps onto the resulting
  // agent (see connections.go); the gateway-only guides set it to identify
  // GBrain / cloud-agent connections distinctly from the generic "other"
  // path. Omitted means no tag — preserves prior behavior for callers that
  // don't care.
  const qs = new URLSearchParams({ wait: 'true', name: agentName })
  if (claim) qs.set('claim', claim)
  if (harness) qs.set('harness', harness)
  return `mkdir -p ~/.clawvisor/agents && printf '\\nApprove the connection request on your Clawvisor dashboard...\\n\\n' && curl -sf --remove-on-error -X POST \\
  "${clawvisorURL}/api/agents/connect?${qs.toString()}" \\
  -o ~/.clawvisor/agents/${agentName}.json \\
  && chmod 600 ~/.clawvisor/agents/${agentName}.json`
}

// ── Wizard primitives ────────────────────────────────────────────────────────
//
// Each per-harness guide renders a small wizard with 2-3 steps. The shared
// scaffolding (StepBar, WizardNav) keeps the per-guide implementations short
// and consistent. Steps are tracked by integer index; completion of an earlier
// step is observable (agent exists, key vaulted) so the bar reflects real
// progress rather than just clicks.

type WizardStepDef = { id: string; title: string; done: boolean }

function StepBar({ steps, activeIndex }: { steps: WizardStepDef[]; activeIndex: number }) {
  return (
    <ol className="inline-flex items-center gap-2 text-xs">
      {steps.map((s, i) => {
        const isActive = i === activeIndex
        const isDone = s.done
        // Active always gets a ring so the "you are here" marker survives
        // even when the step is also done (criteria met before reaching it).
        const baseClass = isDone
          ? 'bg-brand text-surface-0 border-brand'
          : isActive
            ? 'bg-surface-0 text-brand border-brand'
            : 'bg-surface-0 text-text-tertiary border-border-default'
        const ringClass = isActive ? ' ring-2 ring-brand/30' : ''
        const labelClass = isActive ? 'text-text-primary font-medium' : 'text-text-tertiary'
        return (
          <Fragment key={s.id}>
            {i > 0 && (
              <div className={`h-px w-6 ${steps[i - 1].done ? 'bg-brand' : 'bg-border-default'}`} />
            )}
            <li className="flex items-center gap-2 whitespace-nowrap">
              <div className={`w-5 h-5 rounded-full flex items-center justify-center text-sm font-bold border transition-colors ${baseClass}${ringClass}`}>
                {i + 1}
              </div>
              <span className={labelClass}>{s.title}</span>
            </li>
          </Fragment>
        )
      })}
    </ol>
  )
}

function WizardNav({
  canBack, canNext, onBack, onNext, onSkip,
  nextLabel = 'Next', skipLabel = 'Skip', nextDisabledHint,
  showTopBorder = true,
  alignNext = 'right',
}: {
  canBack: boolean
  canNext: boolean
  onBack: () => void
  onNext: () => void
  onSkip?: () => void
  nextLabel?: string
  skipLabel?: string
  nextDisabledHint?: string
  showTopBorder?: boolean
  alignNext?: 'left' | 'right'
}) {
  const alignLeft = alignNext === 'left' && !canBack

  const nextButton = (
    <button
      onClick={onNext}
      disabled={!canNext}
      className="bg-brand text-surface-0 font-medium rounded px-4 py-1.5 text-sm hover:bg-brand-strong disabled:opacity-50 disabled:cursor-not-allowed"
    >
      {nextLabel}
    </button>
  )

  const hint = !canNext && nextDisabledHint ? (
    <span className="text-xs text-text-tertiary">{nextDisabledHint}</span>
  ) : null

  const skipButton = onSkip ? (
    <button
      onClick={onSkip}
      className="text-sm text-text-secondary hover:text-text-primary"
    >
      {skipLabel}
    </button>
  ) : null

  const nextActions = alignLeft ? (
    <>
      {nextButton}
      {hint}
      {skipButton}
    </>
  ) : (
    <>
      {hint}
      {skipButton}
      {nextButton}
    </>
  )

  return (
    <div className={`flex items-center gap-3 pt-4 mt-4${showTopBorder ? ' border-t border-border-subtle' : ''} ${
      alignLeft ? 'justify-start' : 'justify-between'
    }`}>
      {alignLeft ? (
        <div className="flex w-full items-center gap-4">{nextActions}</div>
      ) : (
        <>
          <div>
            {canBack && (
              <button
                onClick={onBack}
                className="text-sm text-text-secondary hover:text-text-primary"
              >
                ← Back
              </button>
            )}
          </div>
          <div className="flex items-center gap-4">{nextActions}</div>
        </>
      )}
    </div>
  )
}


function OtherAgentGuide({ setupURL, clawvisorURL, llmBaseURL, claim, newToken, copied, onCopy, showSkillDefault }: {
  setupURL: string
  clawvisorURL: string
  llmBaseURL: string
  claim: string | undefined
  newToken: string | null
  copied: boolean
  onCopy: (text: string) => void
  showSkillDefault: boolean
}) {
  const [keyAcknowledged, setKeyAcknowledged] = useState(false)
  const [useDone, setUseDone] = useState(false)
  const { data: agents } = useQuery({
    queryKey: ['agents', 'personal'],
    queryFn: () => api.agents.list(),
    refetchInterval: 3000,
  })
  const [agentName, setAgentName] = useSequencedAgentName('my-agent', agents)
  const myAgent = agents?.find(a => a.name === agentName)
  const connected = !!myAgent
  const { data: creds } = useQuery({
    queryKey: ['llm-credentials', myAgent?.id ?? ''],
    queryFn: () => api.llmCredentials.list(myAgent!.id),
    enabled: !!myAgent,
  })
  const keyReady = hasAnyUpstreamKey(creds)

  const jsonPath = `~/.clawvisor/agents/${agentName}.json`
  const anthropicSDK = `import anthropic, json, os
data = json.load(open(os.path.expanduser("${jsonPath}")))
client = anthropic.Anthropic(
    base_url="${llmBaseURL}/api",
    api_key=data["token"],
)`
  const openaiSDK = `from openai import OpenAI
import json, os
data = json.load(open(os.path.expanduser("${jsonPath}")))
client = OpenAI(
    base_url="${llmBaseURL}/api/v1",
    api_key=data["token"],
)`
  const curlCmd = `curl -X POST "${llmBaseURL}/api/v1/messages" \\
  -H "Authorization: Bearer $(jq -r .token ${jsonPath})" \\
  -H "anthropic-version: 2023-06-01" \\
  -H "Content-Type: application/json" \\
  -d '{"model":"claude-sonnet-4-6","max_tokens":256,"messages":[{"role":"user","content":"hi"}]}'`
  const tokenValue = newToken ?? 'cvis_<your-token>'
  const manualAnthropicSDK = `import anthropic
client = anthropic.Anthropic(
    base_url="${llmBaseURL}/api",
    api_key="${tokenValue}",
)`
  const manualOpenaiSDK = `from openai import OpenAI
client = OpenAI(
    base_url="${llmBaseURL}/api/v1",
    api_key="${tokenValue}",
)`
  const prompt = `Please install Clawvisor. It's a security gateway between you and external services like Gmail, Slack, and GitHub. You don't hold any API keys directly; instead, you make requests through Clawvisor and I approve which actions you can take. Every call is logged, and I can revoke access at any time.\n\nSetup is just registering an agent token and installing a skill that teaches you how to use it. I'll review each step before it happens.\n\nInstructions: ${setupURL}`
  const bootstrapCmd = buildBootstrapCommand(clawvisorURL, claim, agentName)
  const keyStepDone = keyAcknowledged || keyReady

  const step1Variant = connected ? 'complete' : 'active'
  const step2Variant = keyStepDone
    ? 'complete'
    : connected
      ? 'active'
      : 'upcoming'
  const step3Variant = useDone
    ? 'complete'
    : keyStepDone
      ? 'active'
      : 'upcoming'

  return (
    <div className="space-y-5">
      <p className="text-sm text-text-secondary">
        If the agent lets you change its Anthropic or OpenAI-compatible LLM
        gateway URL, it can use Clawvisor. Clawvisor swaps your{' '}
        <code className="font-mono text-text-secondary">cvis_…</code> token for
        a vaulted upstream key on each call. Three steps — bootstrap, vault, use.
      </p>

      <div className="space-y-3">
        <SetupGuideStep
          step={1}
          title="Bootstrap agent"
          description="Run the registration command in your terminal, then approve the connection inline."
          completedSummary={myAgent ? <>Registered as {myAgent.name}</> : undefined}
          variant={step1Variant}
          copyValue={bootstrapCmd}
          copyLabel="bootstrap command"
        >
          <BootstrapApproveStep
            clawvisorURL={clawvisorURL}
            claim={claim}
            agentName={agentName}
            setAgentName={setAgentName}
            onCopy={onCopy}
            onAdvance={() => {}}
          />
        </SetupGuideStep>

        <SetupGuideStep
          step={2}
          title="Vault upstream key"
          description="Store your Anthropic or OpenAI key so Clawvisor can swap it in on each proxied call."
          completedSummary={keyReady ? 'Upstream key configured' : 'Skipped'}
          variant={step2Variant}
        >
          {myAgent ? (
            <>
              <VaultKeyStep agentId={myAgent.id} />
              <WizardNav
                canBack={false}
                canNext={keyReady}
                onBack={() => {}}
                onNext={() => setKeyAcknowledged(true)}
                onSkip={() => setKeyAcknowledged(true)}
                skipLabel="Skip — I'll vault one elsewhere"
                nextLabel="Continue"
                showTopBorder={false}
                alignNext="left"
                nextDisabledHint={keyReady ? undefined : 'Vault at least one provider key to continue'}
              />
            </>
          ) : (
            <p className="text-xs text-text-tertiary">
              Bootstrap your agent first — this step unlocks once the connection is approved.
            </p>
          )}
        </SetupGuideStep>

        <SetupGuideStep
          step={3}
          title="Use it"
          description="Point your agent's SDK or HTTP client at Clawvisor using the on-disk token."
          completedSummary="SDK snippets configured"
          variant={step3Variant}
        >
          {keyStepDone ? (
            <>
              <div className="space-y-3">
                <div className="space-y-1.5">
                  <p className="text-sm font-medium text-text-primary">Anthropic SDK (Python)</p>
                  <CodeBlock onCopy={() => onCopy(anthropicSDK)}>{anthropicSDK}</CodeBlock>
                </div>
                <div className="space-y-1.5">
                  <p className="text-sm font-medium text-text-primary">OpenAI SDK (Python)</p>
                  <CodeBlock onCopy={() => onCopy(openaiSDK)}>{openaiSDK}</CodeBlock>
                </div>
                <div className="space-y-1.5">
                  <p className="text-sm font-medium text-text-primary">curl / direct HTTP</p>
                  <CodeBlock onCopy={() => onCopy(curlCmd)}>{curlCmd}</CodeBlock>
                  <p className="text-xs text-text-tertiary">
                    Needs <code className="font-mono text-text-secondary">jq</code> (<code className="font-mono text-text-secondary">brew install jq</code> on macOS).
                  </p>
                </div>
              </div>
              <WizardNav
                canBack={false}
                canNext
                onBack={() => {}}
                onNext={() => setUseDone(true)}
                nextLabel="Done"
                showTopBorder={false}
                alignNext="left"
              />
            </>
          ) : (
            <p className="text-xs text-text-tertiary">
              Vault your upstream key first — this step unlocks once you continue.
            </p>
          )}
        </SetupGuideStep>
      </div>

      <details className="group">
        <summary className="text-sm font-medium text-text-secondary cursor-pointer hover:text-text-primary select-none">
          Manual setup (inline a token created via the dashboard)
        </summary>
        <div className="mt-3 space-y-3">
          <p className="text-xs text-text-tertiary">
            If you don't want a JSON file on disk, create an agent in <strong>Your Agents</strong>{' '}
            below and inline the token directly. The placeholder fills in automatically after creation.
          </p>
          <CodeBlock onCopy={() => onCopy(manualAnthropicSDK)}>{manualAnthropicSDK}</CodeBlock>
          <CodeBlock onCopy={() => onCopy(manualOpenaiSDK)}>{manualOpenaiSDK}</CodeBlock>
        </div>
      </details>

      <details className="group" open={showSkillDefault}>
        <summary className="text-sm font-medium text-text-secondary cursor-pointer hover:text-text-primary select-none">
          Skill-based setup (use Clawvisor's native skill protocol instead)
        </summary>
        <div className="mt-4 space-y-5">
          <p className="text-sm text-text-secondary">
            Any agent that can make HTTP requests can speak Clawvisor's skill protocol directly.
            The fastest way is to paste the setup prompt below into your agent's chat — it will
            self-register and wait for your approval.
          </p>

          <div className="space-y-4">
            <div className="flex items-start gap-3">
              <StepNumber n={1} />
              <div className="space-y-1.5 min-w-0 flex-1">
                <p className="text-sm font-medium text-text-primary">Paste this into your agent</p>
                <div className="relative group bg-surface-0 border border-brand/20 rounded overflow-hidden">
                  <pre className="px-3 py-2.5 sm:pr-16 text-xs font-mono text-text-primary overflow-x-auto whitespace-pre-wrap break-words">
                    {prompt}
                  </pre>
                  <button
                    onClick={() => onCopy(prompt)}
                    className="hidden sm:block absolute top-2 right-2 text-xs px-2 py-1 rounded border border-border-subtle text-text-tertiary hover:text-text-primary hover:bg-surface-1"
                  >
                    {copied ? 'Copied' : 'Copy'}
                  </button>
                  <div className="sm:hidden border-t border-brand/20 px-3 py-1.5 flex justify-end">
                    <button
                      onClick={() => onCopy(prompt)}
                      className="text-xs px-2.5 py-1 rounded border border-border-subtle text-text-tertiary hover:text-text-primary hover:bg-surface-1"
                    >
                      {copied ? 'Copied' : 'Copy'}
                    </button>
                  </div>
                </div>
                <p className="text-xs text-text-tertiary">
                  The agent will follow the setup instructions at that URL — it registers itself,
                  sets up E2E encryption, and installs the Clawvisor skill.
                </p>
              </div>
            </div>

            <div className="flex items-start gap-3">
              <StepNumber n={2} />
              <div className="space-y-1.5 min-w-0 flex-1">
                <p className="text-sm font-medium text-text-primary">Approve the connection</p>
                <p className="text-xs text-text-tertiary">
                  A connection request will appear in the <strong>Pending Connections</strong> section above.
                  Click <strong>Approve</strong> to grant the agent a token. It receives the token automatically
                  and is ready to go.
                </p>
              </div>
            </div>
          </div>

          <details className="group">
            <summary className="text-sm font-medium text-text-secondary cursor-pointer hover:text-text-primary select-none">
              Manual setup (token + environment variables)
            </summary>
            <div className="mt-4 space-y-4 pl-0">
              <div className="flex items-start gap-3">
                <StepNumber n={1} />
                <div className="space-y-1.5 min-w-0 flex-1">
                  <p className="text-sm font-medium text-text-primary">Create an agent token</p>
                  <p className="text-xs text-text-tertiary">
                    Use the <strong>Create Agent</strong> form above. Copy the token — it's shown only once.
                  </p>
                </div>
              </div>

              <div className="flex items-start gap-3">
                <StepNumber n={2} />
                <div className="space-y-1.5 min-w-0 flex-1">
                  <p className="text-sm font-medium text-text-primary">Configure environment variables</p>
                  <p className="text-xs text-text-tertiary">
                    Set these in your agent's environment (<code className="font-mono text-text-secondary">.env</code>, shell profile, container config, etc.):
                  </p>
                  <CodeBlock>{`CLAWVISOR_URL=${clawvisorURL}\nCLAWVISOR_AGENT_TOKEN=<your token>`}</CodeBlock>
                </div>
              </div>

              <div className="flex items-start gap-3">
                <StepNumber n={3} />
                <div className="space-y-1.5 min-w-0 flex-1">
                  <p className="text-sm font-medium text-text-primary">Verify</p>
                  <CodeBlock>{`curl -sf -H "X-Clawvisor-Agent-Token: $CLAWVISOR_AGENT_TOKEN" \\\n  "$CLAWVISOR_URL/api/skill/catalog" | head -20`}</CodeBlock>
                  <p className="text-xs text-text-tertiary">
                    Should return a JSON catalog of available services. See{' '}
                    <code className="font-mono text-text-secondary">{clawvisorURL}/skill/SKILL.md</code>{' '}
                    for the full protocol reference.
                  </p>
                </div>
              </div>
            </div>
          </details>
        </div>
      </details>
    </div>
  )
}

// ── Wizard helpers shared by the fallback OtherAgentGuide ────────────────────
//
// BootstrapApproveStep, VaultKeyStep, and hasAnyUpstreamKey were once used by
// every per-harness guide. The new installer-skill flow handles minting
// inside the agent, so they only survive for OtherAgentGuide — the fallback
// path for agents that can't redirect their LLM endpoint.

// BootstrapApproveStep handles step 1 for every harness: name input, the
// bootstrap curl, and (when the curl runs) inline Approve / Deny buttons for
// the pending connection request — so the user never has to scroll up to the
// Pending Connections card. Completion is detected via the existing
// ['agents'] query: the step becomes done when an agent matching the chosen
// name exists.
function BootstrapApproveStep({
  clawvisorURL, claim, agentName, setAgentName, onCopy, onAdvance, harness,
}: {
  clawvisorURL: string
  claim: string | undefined
  agentName: string
  setAgentName: (n: string) => void
  onCopy: (text: string) => void
  onAdvance: (agentId: string) => void
  // `harness` is stamped onto the connection request's install_context so the
  // resulting agent record carries which gateway-only path created it
  // (gbrain / cloud-agent). Existing callers omit this and get an untagged
  // connection, same as before.
  harness?: string
}) {
  const qc = useQueryClient()
  const { data: connections } = useQuery({
    queryKey: ['connections'],
    queryFn: () => api.connections.list(),
    refetchInterval: 3000,
  })
  const { data: agents } = useQuery({
    queryKey: ['agents', 'personal'],
    queryFn: () => api.agents.list(),
    refetchInterval: 3000,
  })

  const myAgent = useMemo(
    () => agents?.find(a => a.name === agentName),
    [agents, agentName],
  )
  const myPending = useMemo(
    () => connections?.find(c => c.name === agentName && c.status === 'pending'),
    [connections, agentName],
  )

  // Any time a previously-tracked pending request disappears (approved,
  // denied via the inline buttons, or server-expired after a wait-timeout)
  // the claim that produced it has been burned. Mint a fresh one so the
  // visible curl in the UI is immediately retry-able. The mutation
  // onSuccess handlers also invalidate, but this effect is the only thing
  // that catches the server-expired case where the dashboard wasn't the
  // driver of the resolution.
  const hadPendingRef = useRef(false)
  useEffect(() => {
    if (hadPendingRef.current && !myPending) {
      qc.invalidateQueries({ queryKey: ['connection-claim'] })
    }
    hadPendingRef.current = !!myPending
  }, [myPending, qc])

  const [actionError, setActionError] = useState<string | null>(null)
  const approveMut = useMutation({
    mutationFn: (id: string) => api.connections.approve(id),
    onSuccess: (data) => {
      qc.invalidateQueries({ queryKey: ['connections'] })
      qc.invalidateQueries({ queryKey: ['agents', 'personal'] })
      qc.invalidateQueries({ queryKey: ['agents'] })
      qc.invalidateQueries({ queryKey: ['welcome'] })
      qc.invalidateQueries({ queryKey: ['overview'] })
      // Claim is consumed once the curl POSTs; re-mint so a follow-up
      // bootstrap in this session always has a fresh code.
      qc.invalidateQueries({ queryKey: ['connection-claim'] })
      if (data.agent_id) onAdvance(data.agent_id)
    },
    onError: (err: Error) => setActionError(err.message),
  })
  const denyMut = useMutation({
    mutationFn: (id: string) => api.connections.deny(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['connections'] })
      qc.invalidateQueries({ queryKey: ['overview'] })
      // The claim was burned by the bootstrap curl that produced this
      // request; pasting the same command again would 401. Mint a fresh
      // one so the visible curl is immediately retry-able.
      qc.invalidateQueries({ queryKey: ['connection-claim'] })
    },
    onError: (err: Error) => setActionError(err.message),
  })

  const bootstrapCmd = buildBootstrapCommand(clawvisorURL, claim, agentName, harness)
  const filePath = `~/.clawvisor/agents/${agentName}.json`

  return (
    <div className="space-y-4">
      <div>
        <label className="text-xs uppercase tracking-wider text-text-tertiary">Name this agent</label>
        <input
          type="text"
          value={agentName}
          onChange={e => setAgentName(sanitizeAgentName(e.target.value))}
          disabled={!!myPending}
          className="mt-1 block w-full max-w-xs text-sm font-mono rounded border border-border-default bg-surface-0 text-text-primary px-3 py-1.5 focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand disabled:opacity-60"
        />
        <p className="text-xs text-text-tertiary mt-1">
          Determines both the agent's name in Clawvisor and the on-disk file:{' '}
          <code className="font-mono text-text-secondary">{filePath}</code>
          {myAgent && !myPending && (
            <span className="ml-1 text-warning">An agent with this name already exists; pick a different name to create a fresh connection.</span>
          )}
        </p>
      </div>

      <div className="space-y-1.5">
        <p className="text-sm font-medium text-text-primary">Run this in your terminal</p>
        <CodeBlock onCopy={() => onCopy(bootstrapCmd)}>{bootstrapCmd}</CodeBlock>
      </div>

      {myPending ? (
        <div className="rounded border border-brand/30 bg-brand/5 px-4 py-3 space-y-2">
          <div>
            <p className="text-sm font-medium text-text-primary">Connection request received.</p>
            <p className="text-xs text-text-secondary mt-1">
              From <code className="font-mono">{myPending.ip_address}</code> ·{' '}
              requested {formatDistanceToNow(new Date(myPending.created_at), { addSuffix: true })}.
              Approve to release the curl with a fresh token.
            </p>
          </div>
          {actionError && <p className="text-xs text-danger">{actionError}</p>}
          <div className="flex items-center gap-2">
            <button
              onClick={() => { setActionError(null); approveMut.mutate(myPending.id) }}
              disabled={approveMut.isPending || denyMut.isPending}
              className="bg-brand text-surface-0 font-medium rounded px-4 py-1.5 text-sm hover:bg-brand-strong disabled:opacity-50"
            >
              {approveMut.isPending ? 'Approving…' : 'Approve'}
            </button>
            <button
              onClick={() => { setActionError(null); denyMut.mutate(myPending.id) }}
              disabled={approveMut.isPending || denyMut.isPending}
              className="rounded px-4 py-1.5 text-sm font-medium bg-danger/10 text-danger border border-danger/20 hover:bg-danger/20 disabled:opacity-50"
            >
              Deny
            </button>
          </div>
        </div>
      ) : myAgent ? (
        <div className="rounded border border-border-default bg-surface-0 px-4 py-3 space-y-2">
          <p className="text-sm text-text-secondary">
            Pick a different name to create a fresh connection request. Clawvisor will issue a new token after you approve it.
          </p>
        </div>
      ) : (
        <p className="text-xs text-text-tertiary">
          Waiting for you to run the curl above. Once it lands, an Approve button shows up right here.
        </p>
      )}
    </div>
  )
}

function isWizardStartActivity(entry: AuditEntry, startedAtMs: number): boolean {
  const ts = new Date(entry.timestamp).getTime()
  if (!Number.isFinite(ts) || ts < startedAtMs - 5000) return false
  if (entry.activity_kind === 'runtime') return true
  if (entry.action?.startsWith('lite_proxy.')) return true
  return false
}

function isUpstreamCredentialIssue(entry: AuditEntry): boolean {
  return entry.outcome === 'upstream_auth_missing_for_passthrough' ||
    entry.outcome === 'upstream_key_missing' ||
    entry.error_msg?.includes('API key configured') === true
}

function useAgentStartActivity(agentId: string | null | undefined, startedAtMs: number, polling: boolean = true) {
  const { data } = useQuery({
    queryKey: ['audit', 'install-wizard-start', agentId ?? 'none', startedAtMs],
    queryFn: () => api.audit.list({ agent_id: agentId ?? undefined, limit: 8 }),
    enabled: !!agentId && polling,
    refetchInterval: polling ? 3000 : false,
    retry: false,
  })
  return useMemo(
    () => (data?.entries ?? []).find(entry => isWizardStartActivity(entry, startedAtMs)),
    [data, startedAtMs],
  )
}

function AgentStartStatus({
  liveSession,
  startActivity,
  waitingText,
}: {
  liveSession?: RuntimeSession
  startActivity?: AuditEntry
  waitingText: string
}) {
  const detected = !!liveSession || !!startActivity
  if (detected) {
    return (
      <div className="rounded-md border border-success/30 bg-success/10 px-3 py-3">
        <div className="flex items-start gap-2.5">
          <span className="mt-1 h-2.5 w-2.5 rounded-full bg-success shadow-[0_0_0_3px_rgba(34,197,94,0.16)]" />
          <div>
            <p className="text-sm font-medium text-success">
              {liveSession ? 'Live session detected' : 'Routed activity detected'}
            </p>
            <p className="text-xs text-text-secondary mt-1">
              Clawvisor is seeing traffic for this agent. Continue when you're ready to finish setup.
            </p>
          </div>
        </div>
      </div>
    )
  }
  return (
    <div className="rounded-md border border-border-subtle bg-surface-0 px-3 py-3">
      <div className="flex items-start gap-2.5">
        <span className="mt-1 h-2.5 w-2.5 rounded-full bg-text-tertiary" />
        <div>
          <p className="text-sm font-medium text-text-primary">Waiting for Clawvisor traffic</p>
          <p className="text-xs text-text-tertiary mt-1">{waitingText}</p>
        </div>
      </div>
    </div>
  )
}

// ── Manual proxy setup path (Claude Code / Codex) ────────────────────────────

type ManualProxyTarget = 'claude-code' | 'codex'

const MANUAL_PROXY_META: Record<ManualProxyTarget, { label: string; baseName: string }> = {
  'claude-code': { label: 'Claude Code', baseName: 'claude-code' },
  codex: { label: 'Codex', baseName: 'codex' },
}

function ManualProxyCLISetupGuide({
  target,
  clawvisorURL,
  llmBaseURL,
  claim,
  onCopy,
}: {
  target: ManualProxyTarget
  clawvisorURL: string
  llmBaseURL: string
  claim: string | undefined
  onCopy: (text: string) => void
}) {
  const meta = MANUAL_PROXY_META[target]
  const [step, setStep] = useState(0)
  const startedAtRef = useRef(Date.now())
  const { data: agents } = useQuery({
    queryKey: ['agents', 'personal'],
    queryFn: () => api.agents.list(),
    refetchInterval: 3000,
  })
  const [agentName, setAgentName] = useSequencedAgentName(meta.baseName, agents)
  const [agentId, setAgentId] = useState<string | null>(null)
  const selectedAgent = useMemo(
    () => agentId ? agents?.find(a => a.id === agentId) : agents?.find(a => a.name === agentName),
    [agentId, agentName, agents],
  )
  useEffect(() => {
    if (selectedAgent && selectedAgent.id !== agentId) setAgentId(selectedAgent.id)
  }, [agentId, selectedAgent])

  const { data: sessions } = useQuery({
    queryKey: ['runtime-sessions', 'install-wizard', agentId ?? 'none'],
    queryFn: () => api.runtime.listSessions(),
    enabled: !!agentId,
    refetchInterval: 3000,
    retry: false,
  })
  const liveSession = useMemo(() => {
    return (sessions?.entries ?? []).find(session => session.agent_id === agentId && isActiveRuntimeSession(session))
  }, [agentId, sessions])
  const startActivity = useAgentStartActivity(agentId, startedAtRef.current)
  const agentStarted = !!liveSession || !!startActivity
  const upstreamProvider: LLMProvider = target === 'codex' ? 'openai' : 'anthropic'
  const authIssueActivity = startActivity && isUpstreamCredentialIssue(startActivity) ? startActivity : undefined
  const { data: userCreds } = useQuery({
    queryKey: ['llm-credentials', 'user'],
    queryFn: () => api.llmCredentials.list(),
    enabled: !!authIssueActivity,
  })
  const authIssueKeyReady = hasProviderUpstreamKey(userCreds, upstreamProvider)

  const tokenPath = `~/.clawvisor/agents/${agentName}.json`
  // Opt-in flag that disables the harness's interactive permission prompts.
  // For Claude Code: `--dangerously-skip-permissions`. For Codex: the
  // equivalent `--dangerously-bypass-approvals-and-sandbox`. Applied to BOTH
  // the test-connection (`startCommand`) and the alias (`aliasCommand`) so
  // the user's everyday `claude-cv` / `codex-cv` invocations stay consistent
  // with what they verified in the wizard.
  const [skipPermissions, setSkipPermissions] = useState(false)
  const skipPermsFlag = target === 'codex'
    ? '--dangerously-bypass-approvals-and-sandbox'
    : '--dangerously-skip-permissions'
  const skipPermsClaude = skipPermissions ? ` ${skipPermsFlag}` : ''
  const skipPermsCodex = skipPermissions ? ` ${skipPermsFlag}` : ''

  const configureCommand = `mkdir -p ~/.codex
grep -q '^\\[model_providers\\.clawvisor\\]' ~/.codex/config.toml 2>/dev/null || cat >> ~/.codex/config.toml <<'EOF'

[model_providers.clawvisor]
name = "Clawvisor"
base_url = "${llmBaseURL}/api/v1"
wire_api = "responses"
requires_openai_auth = true

[model_providers.clawvisor.env_http_headers]
X-Clawvisor-Agent-Token = "CLAWVISOR_AGENT_TOKEN"
EOF`
  const startCommand = target === 'codex'
    ? `CLAWVISOR_AGENT_TOKEN=$(jq -r .token ${tokenPath}) codex${skipPermsCodex} -c model_provider=clawvisor`
    : `ANTHROPIC_BASE_URL=${llmBaseURL}/api \\
ANTHROPIC_CUSTOM_HEADERS="X-Clawvisor-Agent-Token: $(jq -r .token ${tokenPath})" \\
ANTHROPIC_AUTH_TOKEN= ANTHROPIC_API_KEY= \\
claude${skipPermsClaude}`
  const aliasCommand = target === 'codex'
    ? `cat >> ~/.zshrc <<'EOF'
codex-cv() {
  CLAWVISOR_AGENT_TOKEN=$(jq -r .token ${tokenPath}) codex${skipPermsCodex} -c model_provider=clawvisor "$@"
}
EOF`
    : `cat >> ~/.zshrc <<'EOF'
claude-cv() {
  ANTHROPIC_BASE_URL=${llmBaseURL}/api \\
  ANTHROPIC_CUSTOM_HEADERS="X-Clawvisor-Agent-Token: $(jq -r .token ${tokenPath})" \\
  ANTHROPIC_AUTH_TOKEN= ANTHROPIC_API_KEY= \\
  claude${skipPermsClaude} "$@"
}
EOF`
  const settingsLink = selectedAgent ? `/dashboard/agents/${encodeURIComponent(selectedAgent.id)}` : '/dashboard/agents'

  const wizardSteps: WizardStepDef[] = target === 'codex'
    ? [
        { id: 'token', title: 'Token', done: !!agentId },
        { id: 'configure', title: 'Configure', done: step > 1 },
        { id: 'session', title: 'Start session', done: agentStarted || step > 2 },
        { id: 'alias', title: 'Alias & settings', done: step > 3 },
      ]
    : [
        { id: 'token', title: 'Token', done: !!agentId },
        { id: 'session', title: 'Start session', done: agentStarted || step > 2 },
        { id: 'alias', title: 'Alias & settings', done: step > 3 },
      ]
  const activeStepIndex = target === 'codex' ? step : Math.max(0, step - 1)

  return (
    <div className="space-y-5">
      <p className="text-sm text-text-secondary">
        Set up {meta.label} manually for the local machine. First mint an agent
        token, then start one Clawvisor-routed session. When Clawvisor sees
        traffic, the wizard will show that you're ready for alias and settings.
      </p>
      <div className="rounded-md border border-border-default bg-surface-1 px-4 py-5 space-y-4">
        <div className="overflow-x-auto pb-1">
          <StepBar steps={wizardSteps} activeIndex={activeStepIndex} />
        </div>

        {step === 0 && (
          <>
            <BootstrapApproveStep
              clawvisorURL={clawvisorURL}
              claim={claim}
              agentName={agentName}
              setAgentName={setAgentName}
              onCopy={onCopy}
              onAdvance={(id) => {
                setAgentId(id)
                setStep(target === 'codex' ? 1 : 2)
              }}
            />
          </>
        )}

        {target === 'codex' && step === 1 && (
          <>
            <div className="space-y-3">
              <p className="text-sm font-medium text-text-primary">Configure {meta.label}</p>
              <p className="text-xs text-text-tertiary">
                This uses the token file created in the previous step:{' '}
                <code className="font-mono text-text-secondary">{tokenPath}</code>.
              </p>
              <CodeBlock onCopy={() => onCopy(configureCommand)}>{configureCommand}</CodeBlock>
            </div>
            <WizardNav
              canBack
              canNext
              onBack={() => setStep(0)}
              onNext={() => setStep(2)}
              nextLabel="Continue"
            />
          </>
        )}

        {step === 2 && (
          <>
            <div className="space-y-3">
              <p className="text-sm font-medium text-text-primary">Start a Clawvisor-routed session</p>
              <p className="text-xs text-text-tertiary">
                Run this and send a short test message. This step updates when
                Clawvisor sees routed activity from this agent.
              </p>
              <SkipPermissionsCheckbox
                checked={skipPermissions}
                onChange={setSkipPermissions}
                flag={skipPermsFlag}
                label={meta.label}
              />
              <CodeBlock onCopy={() => onCopy(startCommand)}>{startCommand}</CodeBlock>
              <AgentStartStatus
                liveSession={liveSession}
                startActivity={startActivity}
                waitingText="Run the command above and send a short message. This updates automatically once Clawvisor sees the request."
              />
              {authIssueActivity && (
                <div className="rounded-md border border-warning/30 bg-warning/10 px-3 py-3">
                  <p className="text-sm font-medium text-text-primary">API key needed</p>
                  <p className="text-xs text-text-secondary mt-1">
                    {meta.label} reached Clawvisor, but the first model request did not include usable upstream auth.
                    Add a {providerLabel(upstreamProvider)} API key, then run the session command again.
                  </p>
                  <div className="mt-3">
                    <VaultKeyStep
                      provider={upstreamProvider}
                      title={`Add ${providerLabel(upstreamProvider)} API key`}
                      description={`Clawvisor will use this user-level ${providerLabel(upstreamProvider)} key only when ${meta.label}'s own upstream auth is unavailable.`}
                    />
                  </div>
                </div>
              )}
            </div>
            <WizardNav
              canBack
              canNext={!authIssueActivity || authIssueKeyReady}
              onBack={() => setStep(target === 'codex' ? 1 : 0)}
              onNext={() => setStep(3)}
              nextLabel={agentStarted ? 'Continue to alias & settings' : "I've started it"}
              nextDisabledHint={authIssueActivity && !authIssueKeyReady ? `Add a ${providerLabel(upstreamProvider)} API key to continue` : undefined}
            />
          </>
        )}

        {step === 3 && (
          <>
            <div className="space-y-4">
              <div>
                <p className="text-sm font-medium text-text-primary">Create an alias</p>
                <p className="text-xs text-text-tertiary mt-1">
                  Add this to zsh. Use the same shape in bash/fish if needed.
                </p>
                <div className="mt-2 space-y-2">
                  <SkipPermissionsCheckbox
                    checked={skipPermissions}
                    onChange={setSkipPermissions}
                    flag={skipPermsFlag}
                    label={meta.label}
                  />
                  <CodeBlock onCopy={() => onCopy(aliasCommand)}>{aliasCommand}</CodeBlock>
                </div>
              </div>
              <div className="rounded border border-border-subtle bg-surface-0 px-3 py-2.5">
                <p className="text-xs font-medium text-text-primary">Configure settings</p>
                <p className="text-xs text-text-tertiary mt-1">
                  Open this agent’s settings to tune runtime mode, restrictions,
                  secret detection, and task auto-approval.
                </p>
                <Link to={settingsLink} className="mt-2 inline-block text-xs font-medium text-brand hover:underline">
                  Open agent settings
                </Link>
              </div>
            </div>
            <WizardNav
              canBack
              canNext
              onBack={() => setStep(2)}
              onNext={() => setStep(4)}
              nextLabel="Done"
            />
          </>
        )}

        {step >= 4 && (
          <div className="rounded border border-success/30 bg-success/10 px-4 py-3">
            <p className="text-sm font-medium text-success">{meta.label} setup complete.</p>
            <button
              onClick={() => setStep(3)}
              className="mt-2 text-xs text-brand hover:underline"
            >
              Show alias and settings again
            </button>
          </div>
        )}
      </div>
    </div>
  )
}

// ── Installer-skill driven path (Hermes / OpenClaw) ─────────────────────────
//
// The dashboard hands the actual install off to an agent-side skill. Each
// target is what will be connected to Clawvisor. The helper is the agent that
// walks the user through the setup. Claude Code and Codex can both run the
// same target-specific installer markdown.

type InstallerTarget = 'hermes' | 'openclaw'

interface InstallerSpec {
  label: string
  baseName: string
  defaultProvider: LLMProvider
}

const INSTALLER_SPECS: Record<InstallerTarget, InstallerSpec> = {
  hermes: {
    label: 'Hermes',
    baseName: 'hermes',
    defaultProvider: 'openai',
  },
  openclaw: {
    label: 'OpenClaw',
    baseName: 'openclaw',
    defaultProvider: 'anthropic',
  },
}

// Bootstrap is two distinct commands the user pastes in order:
//
//   1. buildConnectCommand: POST /api/agents/connect with wait=true. Curl
//      blocks until the user approves the request in the wizard, then writes
//      the JSON response (which includes the token) directly to disk via
//      `-o`. `--remove-on-error` keeps a denied/expired response from
//      leaving a partial file behind.
//   2. buildHelperCommand: download the installer skill markdown and invoke
//      the helper agent. The helper reads the token from disk.
//
// We keep them as separate copy-paste blocks rather than chaining with `&&`
// so each step's purpose is obvious and the long-polling curl is its own
// concern. The user sees "this one blocks until I approve in the dashboard"
// distinct from "this one runs the helper".
function buildConnectCommand(opts: {
  name: string
  baseURL: string
  claim: string | undefined
  // Install context the server stamps onto the connection request (and, on
  // approval, onto the resulting agent). Passed as query params so the
  // bootstrap curl stays body-less.
  harness?: string
  mode?: string
}): string {
  const { name, baseURL, claim, harness, mode } = opts
  const qs = new URLSearchParams({ wait: 'true', name })
  if (claim) qs.set('claim', claim)
  if (harness) qs.set('harness', harness)
  if (mode) qs.set('mode', mode)
  const connectURL = `${baseURL}/api/agents/connect?${qs.toString()}`
  return [
    `mkdir -p ~/.clawvisor/agents && printf '\\nApprove the connection request on your Clawvisor dashboard...\\n\\n' && curl -sf --remove-on-error -X POST \\`,
    `  "${connectURL}" \\`,
    `  -o ~/.clawvisor/agents/${name}.json \\`,
    `  && chmod 600 ~/.clawvisor/agents/${name}.json`,
  ].join('\n')
}

interface InstallerAnswers {
  hermesConfig: 'env' | 'file'
  hermesMode: 'host' | 'docker' | 'remote'
  openclawMode: 'host' | 'docker' | 'remote'
  llmProvider: LLMProvider
}

type InstallerCredentialScope = CredentialScope

function defaultInstallerAnswers(target: InstallerTarget): InstallerAnswers {
  return {
    hermesConfig: 'env',
    hermesMode: 'host',
    openclawMode: 'host',
    llmProvider: INSTALLER_SPECS[target].defaultProvider,
  }
}

function applyInstallerAnswerParams(params: URLSearchParams, target: InstallerTarget, answers: InstallerAnswers) {
  params.set('llm_provider', answers.llmProvider)
  if (target === 'hermes') {
    params.set('hermes_config', answers.hermesConfig)
    params.set('hermes_mode', answers.hermesMode)
  }
  if (target === 'openclaw') params.set('openclaw_mode', answers.openclawMode)
}

// State model for the installer wizard.
//
// Two phases, two sets of rules:
//
//   Phase 1 (`'configure' | 'apiKey'`) is purely user-driven. Local React
//   state advanced by Next clicks.
//
//   Phase 1 = `'past'` means the user has clicked through to the connection
//   stage. From here on, the active screen is **derived from server state**:
//
//     • If an agent matching the target was created since the user entered
//       phase 'past' → `verify` (or `success` once we see routed activity).
//     • Else if there's a pending matching connection request → `approve`.
//     • Else → `watching` (waiting for the helper to ask Clawvisor for a
//       token).
//
//   Agent existence is checked because the connections list endpoint only
//   exposes pending requests — once approved, the connection drops out and
//   the only durable record is the new agent.
//
//   `pastSinceMs` is the timestamp the user entered phase 'past'; we only
//   match agents created after that, so stale ones from prior installs
//   don't trigger a false success.
//
//   Phase and pastSinceMs are persisted to sessionStorage so reloading
//   mid-install resumes where the user left off.

type InstallerPhase = 'configure' | 'past'

type InstallerScreen =
  | { kind: 'configure' }
  | { kind: 'register' }                       // user pastes command 1
  | { kind: 'approve'; req: ConnectionRequest }
  | { kind: 'apiKey'; agent: Agent }            // post-approval; the key is written against the real agent id
  | { kind: 'runHelper'; agent: Agent }        // user pastes command 2
  | { kind: 'success'; agent: Agent }

function deriveInstallerScreen(
  phase: InstallerPhase,
  pendingReq: ConnectionRequest | undefined,
  approvedAgent: Agent | undefined,
  apiKeyReady: boolean,
  apiKeyAcknowledged: boolean,
  verifyReady: boolean,
): InstallerScreen {
  if (phase === 'configure') return { kind: 'configure' }
  if (approvedAgent) {
    if (verifyReady) return { kind: 'success', agent: approvedAgent }
    if (apiKeyReady && apiKeyAcknowledged) return { kind: 'runHelper', agent: approvedAgent }
    return { kind: 'apiKey', agent: approvedAgent }
  }
  if (pendingReq) return { kind: 'approve', req: pendingReq }
  return { kind: 'register' }
}

function InstallerSkillGuide({
  target,
  installerBaseURL,
  claim,
  userIdParam,
  onCopy,
}: {
  target: InstallerTarget
  // installerBaseURL is the **management** host the dashboard talks to —
  // where /skill/install/<harness>.md and /api/agents/connect live. In split
  // deployments this is *not* the same as `proxy_lite_public_url`, which
  // serves only model traffic (no /skill, no /api/agents/*). Passing the
  // proxy URL here would 404 the skill fetch and the connect POST before
  // setup even starts. The proxy URL the helper uses for model traffic is
  // embedded by the server-side installer renderer (`resolveURL`), not from
  // this prop, so a configured public_url still propagates into the skill
  // markdown end to end without the dashboard knowing about it.
  installerBaseURL: string
  claim: string | undefined
  userIdParam: string
  onCopy: (text: string) => void
}) {
  const qc = useQueryClient()
  const spec = INSTALLER_SPECS[target]

  // ─── Persisted state (phase + phase-2 entry timestamp) ──────────────────
  // Persisted across reloads so an in-flight install resumes where the user
  // left off. The escape hatch is "← Choose a different agent" in the parent
  // wizard: that path clears these keys so re-picking the same target starts
  // fresh at Configure. `pastSinceMs` bounds the "approved agent created
  // during this install" check so we don't false-match a pre-existing agent.
  const phaseKey = `installer:${target}:phase`
  const sinceKey = `installer:${target}:pastSinceMs`
  const [phase, setPhaseState] = useState<InstallerPhase>(() => readInstallerPhase(target))
  const [pastSinceMs, setPastSinceMsState] = useState<number | null>(() => readInstallerSince(target))

  const setPhase = (next: InstallerPhase) => {
    setPhaseState(next)
    try { sessionStorage.setItem(phaseKey, next) } catch {}
    if (next === 'past' && pastSinceMs == null) {
      const ts = Date.now()
      setPastSinceMsState(ts)
      try { sessionStorage.setItem(sinceKey, String(ts)) } catch {}
    } else if (next !== 'past') {
      setPastSinceMsState(null)
      try { sessionStorage.removeItem(sinceKey) } catch {}
    }
  }

  // Restore on target change (different harness, different stored state).
  useEffect(() => {
    setPhaseState(readInstallerPhase(target))
    setPastSinceMsState(readInstallerSince(target))
  }, [target])

  // ─── Form state (not part of the state machine) ─────────────────────────
  const [helper, setHelper] = useState<InstallerHelper>('claude')
  const [answers, setAnswers] = useState<InstallerAnswers>(() => defaultInstallerAnswers(target))
  const [credentialScope, setCredentialScope] = useState<InstallerCredentialScope>('user')
  // apiKeyAcknowledged is the local "user clicked Continue on the API-key
  // screen" flag that lets the wizard advance to Run Helper. It exists so
  // the user can deliberately walk past the API-key screen rather than
  // having the wizard skip it the moment `userKeyReady` first becomes true.
  const [apiKeyAcknowledged, setApiKeyAcknowledged] = useState(false)
  const startedAtRef = useRef(Date.now())
  useEffect(() => {
    setAnswers(defaultInstallerAnswers(target))
    setCredentialScope('user')
    setApiKeyAcknowledged(false)
  }, [target])
  useEffect(() => {
    // Provider change invalidates a previous "I confirmed my key" click —
    // the relevant `userKeyReady` / `agentKeyReady` flips on the new provider.
    setApiKeyAcknowledged(false)
  }, [answers.llmProvider])

  // ─── Polling gate ───────────────────────────────────────────────────────
  // The wizard's four polling queries (agents, connections, sessions, audit)
  // are only useful while we're actively waiting on a server signal. Phase 1
  // doesn't depend on any of them changing during the wizard — agents is
  // needed once for the name picker, but a 3s refetch buys nothing — and
  // once the install reaches `success`, the wizard is terminal until the
  // user clicks "Connect another" (which resets phase). `installComplete`
  // is sticky on purpose: a single useEffect flips it when verifyReady
  // fires, after which background polling stops cold instead of cycling
  // four endpoints at 3s forever.
  const [installComplete, setInstallComplete] = useState(false)
  const pollingActive = phase === 'past' && !installComplete

  // ─── Server state ────────────────────────────────────────────────────────
  const { data: agents } = useQuery({
    queryKey: ['agents', 'personal'],
    queryFn: () => api.agents.list(),
    refetchInterval: pollingActive ? 3000 : false,
  })
  const { data: connections } = useQuery({
    queryKey: ['connections'],
    queryFn: () => api.connections.list(),
    refetchInterval: pollingActive ? 3000 : false,
  })

  // The server's /api/agents/connections endpoint only returns *pending*
  // requests — approved/denied/expired drop out of the list. So we use two
  // different signals for the two phase-2 states:
  //
  //   • `pendingInstallRequest`: a pending request matching this target.
  //   • `approvedAgent`: an agent matching this target that was created
  //     after the user entered phase 'past'. This is how we detect that the
  //     pending request was approved (the agent gets minted on approval).
  // Two matching predicates. The strong signal is `install_context.harness
  // === target`, denormalized onto both connection requests and agents by
  // the bootstrap-curl change. Name-prefix matching is a *bootstrap-only*
  // fallback for pending connection requests that came in without
  // install_context (e.g. a hand-rolled curl from an older harness). It is
  // deliberately NOT applied to the agents list: a pre-existing
  // `openclaw-prod` agent shares the `openclaw-` prefix but is not the
  // install we're tracking, and matching on prefix would jump the wizard to
  // "success" the moment that agent's `created_at` floats past
  // `pastSinceMs-5000` (e.g. clock skew, or a refetch landing right at the
  // edge).
  const matchesTargetPending = (name: string, harness?: string) =>
    harness === target ||
    (!harness && (name === spec.baseName || name.startsWith(`${spec.baseName}-`)))

  const pendingInstallRequest = useMemo(() => {
    if (!connections) return undefined
    return connections
      .filter(cr => matchesTargetPending(cr.name, cr.install_context?.harness))
      .sort((a, b) => new Date(b.created_at).getTime() - new Date(a.created_at).getTime())[0]
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [connections, spec.baseName, target])

  const approvedAgent = useMemo(() => {
    if (!agents || pastSinceMs == null) return undefined
    const cutoff = pastSinceMs - 5000
    return agents
      .filter(a => a.install_context?.harness === target)
      .filter(a => new Date(a.created_at).getTime() >= cutoff)
      .sort((a, b) => new Date(b.created_at).getTime() - new Date(a.created_at).getTime())[0]
  }, [agents, pastSinceMs, target])

  // Whenever a previously-tracked pending request disappears (approved or
  // denied via ConnectionCard, or server-expired after 5 minutes without
  // action), the claim that produced it has been burned. ConnectionCard's
  // mutations cover the in-session approve/deny path; this effect is the
  // only thing that catches the expiry case where the dashboard wasn't the
  // driver of the resolution. Without it, a "Connect another" / retry in
  // the same session would render the cached (now-consumed) claim until
  // the 4-minute mintClaim refetch and the next POST would 401 with
  // INVALID_CLAIM.
  const hadPendingInstallRef = useRef(false)
  useEffect(() => {
    if (hadPendingInstallRef.current && !pendingInstallRequest) {
      qc.invalidateQueries({ queryKey: ['connection-claim'] })
    }
    hadPendingInstallRef.current = !!pendingInstallRequest
  }, [pendingInstallRequest, qc])

  const candidateAgent = approvedAgent

  // Pick the agent name the wizard uses in the curl and skill URL. Order
  // matters:
  //
  //   1. If we already have an approved agent for this install, lock to
  //      its name. Otherwise the helper command (rendered on the Run
  //      Helper screen) would point at the next free slot — e.g. user
  //      registered openclaw-8, approved it, and the helper would then
  //      look up openclaw-9.json because the next-available bumped one
  //      slot after the new agent appeared.
  //   2. If we've already minted a pending request, lock to its name.
  //      Same logic for the in-flight phase.
  //   3. Otherwise, suggest the next-available variant of the base name.
  const agentName = useMemo(() => {
    if (approvedAgent) return approvedAgent.name
    if (pendingInstallRequest) return pendingInstallRequest.name
    return nextAvailableName(spec.baseName, agents)
  }, [approvedAgent, pendingInstallRequest, spec.baseName, agents])

  const { data: sessions } = useQuery({
    queryKey: ['runtime-sessions', 'install-wizard', candidateAgent?.id ?? 'none'],
    queryFn: () => api.runtime.listSessions(),
    enabled: !!candidateAgent?.id && pollingActive,
    refetchInterval: pollingActive ? 3000 : false,
    retry: false,
  })
  const liveSession = useMemo(() =>
    (sessions?.entries ?? []).find(s => s.agent_id === candidateAgent?.id && isActiveRuntimeSession(s)),
  [candidateAgent?.id, sessions])
  const startActivity = useAgentStartActivity(candidateAgent?.id, startedAtRef.current, pollingActive)
  const verifyReady = !!liveSession || !!startActivity
  // Flip the sticky `installComplete` flag the moment we see the first
  // routed call. Subsequent renders read `pollingActive = false` and the
  // four polling queries above unwind to a no-op.
  useEffect(() => {
    if (verifyReady && !installComplete) setInstallComplete(true)
  }, [verifyReady, installComplete])
  // Reset on a "Connect another" / target switch / explicit start-over so the
  // next install actually polls and the user re-confirms the API key step.
  useEffect(() => {
    if (phase !== 'past') {
      if (installComplete) setInstallComplete(false)
      if (apiKeyAcknowledged) setApiKeyAcknowledged(false)
    }
  }, [phase, installComplete, apiKeyAcknowledged])

  const { apiKeyReady } = useUpstreamKeyReadiness(
    answers.llmProvider,
    credentialScope,
    candidateAgent?.id,
  )

  // ─── URL params for the installer skill ──────────────────────────────────
  // Claim takes precedence over user_id; passing both is harmless but the
  // server prefers the claim path and burns the code on consumption.
  const reqUrlParams = new URLSearchParams()
  if (claim) reqUrlParams.set('claim', claim)
  else if (userIdParam) {
    new URLSearchParams(userIdParam.replace(/^\?/, '')).forEach((value, key) => reqUrlParams.set(key, value))
  }
  applyInstallerAnswerParams(reqUrlParams, target, answers)
  if (agentName && agentName !== spec.baseName) {
    reqUrlParams.set('agent_name', agentName)
  }
  const skillURL = `${installerBaseURL}/skill/install/${target}.md${reqUrlParams.toString() ? `?${reqUrlParams.toString()}` : ''}`
  const connectCommand = buildConnectCommand({
    name: agentName,
    baseURL: installerBaseURL,
    claim,
    harness: target,
    mode: target === 'openclaw' ? answers.openclawMode
        : target === 'hermes' ? answers.hermesMode
        : undefined,
  })
  const helperCommand = buildHelperCommand({ skillURL, helper })
  const dashboardIsLocalhost = /^(https?:\/\/)?(localhost|127\.0\.0\.1)([:/]|$)/i.test(installerBaseURL)
  const targetMode = target === 'openclaw' ? answers.openclawMode
                   : target === 'hermes' ? answers.hermesMode
                   : undefined
  const remoteWithLocalhost = targetMode === 'remote' && dashboardIsLocalhost

  // ─── Derived screen + StepBar ───────────────────────────────────────────
  const screen = deriveInstallerScreen(phase, pendingInstallRequest, approvedAgent, apiKeyReady, apiKeyAcknowledged, verifyReady)
  const activeIndex = screenToStepIndex(screen)
  const isPastApiKey = screen.kind === 'runHelper' || screen.kind === 'success'
  const wizardSteps: WizardStepDef[] = [
    { id: 'configure', title: 'Configure', done: phase !== 'configure' },
    { id: 'register', title: 'Register', done: phase === 'past' && screen.kind !== 'register' },
    { id: 'approve', title: 'Approve', done: phase === 'past' && screen.kind !== 'register' && screen.kind !== 'approve' },
    { id: 'apiKey', title: 'API key', done: phase === 'past' && isPastApiKey },
    { id: 'runHelper', title: 'Run helper', done: phase === 'past' && screen.kind === 'success' },
    { id: 'success', title: 'Done', done: phase === 'past' && screen.kind === 'success' },
  ]

  const startOver = () => setPhase('configure')

  return (
    <div className="space-y-5">
      <p className="text-sm text-text-secondary">
        Walk Claude Code or Codex through connecting {spec.label} to Clawvisor.
        Answer a couple of questions, run one command, approve, and you're done.
      </p>

      <div className="rounded-md border border-border-default bg-surface-1 px-4 py-5">
        <div className="overflow-x-auto pb-1">
          <StepBar steps={wizardSteps} activeIndex={activeIndex} />
        </div>

        {screen.kind === 'configure' && (
          <div className="mt-5">
            <InstallerSetupQuestions target={target} answers={answers} onChange={setAnswers} />
            {remoteWithLocalhost && (
              <div className="mt-4 rounded border border-warning/30 bg-warning/10 px-3 py-2.5">
                <p className="text-xs font-medium text-warning">Dashboard URL isn't reachable from another machine</p>
                <p className="text-xs text-text-secondary mt-1 leading-relaxed">
                  This dashboard is on <code className="font-mono">{installerBaseURL}</code>, which the remote
                  OpenClaw host can't reach. Set <code className="font-mono">Server.PublicURL</code> (or a
                  relay) in Clawvisor settings before running the installer, otherwise the curl below will
                  hit the wrong host.
                </p>
              </div>
            )}
            <WizardNav
              canBack={false}
              canNext
              onBack={() => {}}
              onNext={() => setPhase('past')}
            />
          </div>
        )}

        {screen.kind === 'apiKey' && (
          <div className="mt-5 space-y-4">
            <AgentUpstreamKeySetupPanel
              agentName={screen.agent.name}
              agentId={screen.agent.id}
              provider={answers.llmProvider}
              credentialScope={credentialScope}
              onCredentialScopeChange={setCredentialScope}
            />
            <WizardNav
              canBack={false}
              canNext={apiKeyReady}
              onBack={() => {}}
              onNext={() => setApiKeyAcknowledged(true)}
              nextLabel="Continue"
              nextDisabledHint={apiKeyContinueHint(answers.llmProvider, credentialScope, apiKeyReady)}
            />
          </div>
        )}

        {screen.kind === 'register' && (
          <div className="mt-5 space-y-4">
            <div>
              <p className="text-sm font-medium text-text-primary">Register the agent</p>
              <p className="text-xs text-text-tertiary mt-1">
                Paste this into your terminal. It blocks until you approve the
                request in the wizard, then saves the token to
                {' '}<code className="font-mono text-text-secondary">~/.clawvisor/agents/{agentName}.json</code>.
                {agentName !== spec.baseName && (
                  <>
                    {' '}<span className="text-warning">A previous {spec.label} install used <code className="font-mono">{spec.baseName}</code>; this one will register as <code className="font-mono">{agentName}</code>.</span>
                  </>
                )}
              </p>
            </div>
            {remoteWithLocalhost && (
              <div className="rounded border border-warning/30 bg-warning/10 px-3 py-2.5">
                <p className="text-xs font-medium text-warning">Localhost URL won't work on the remote host</p>
                <p className="text-xs text-text-secondary mt-1 leading-relaxed">
                  The command below points at <code className="font-mono">{installerBaseURL}</code>. The remote
                  {' '}{spec.label} host can't reach that. Configure a public/relay URL in Clawvisor settings, then
                  reload this page so the URL updates.
                </p>
              </div>
            )}
            <CodeBlock onCopy={() => onCopy(connectCommand)}>{connectCommand}</CodeBlock>
            <div className="rounded border border-border-subtle bg-surface-0 px-3 py-2.5 flex items-start gap-2.5">
              <span className="mt-1 h-2.5 w-2.5 rounded-full bg-text-tertiary animate-pulse" />
              <p className="text-xs text-text-tertiary">
                Watching for the connection request — this screen updates the moment Clawvisor sees it.
              </p>
            </div>
            <div className="flex items-center pt-3 border-t border-border-subtle">
              <button
                onClick={() => setPhase('configure')}
                className="text-sm text-text-secondary hover:text-text-primary"
              >
                ← Back
              </button>
            </div>
          </div>
        )}

        {screen.kind === 'approve' && (
          <div className="mt-5 space-y-4">
            <div>
              <p className="text-sm font-medium text-text-primary">Approve the connection</p>
              <p className="text-xs text-text-tertiary mt-1 leading-relaxed">
                Your terminal is blocked on the registration curl. Approving here
                unblocks it and writes the token to disk so the helper agent can
                pick it up in the next step.
              </p>
            </div>
            <ConnectionCard request={screen.req} />
          </div>
        )}

        {screen.kind === 'runHelper' && (
          <div className="mt-5">
            <InstallHelperStepPanel
              helper={helper}
              onHelperChange={setHelper}
              helperCommand={helperCommand}
              skillPreviewUrl={skillURL}
              frameworkLabel={spec.label}
              onCopy={onCopy}
              agentName={candidateAgent?.name}
              liveSession={liveSession}
              startActivity={startActivity}
            />
          </div>
        )}

        {screen.kind === 'success' && (
          <div className="mt-5 space-y-4">
            <div className="rounded border border-success/30 bg-success/10 px-4 py-3">
              <p className="text-sm font-medium text-success">{spec.label} is connected to Clawvisor.</p>
              <p className="text-xs text-text-secondary mt-1">
                {candidateAgent
                  ? <>It registered as <code className="font-mono">{candidateAgent.name}</code>. Every model call from this agent now flows through Clawvisor.</>
                  : <>Every model call from this agent now flows through Clawvisor.</>}
              </p>
            </div>
            <div className="rounded border border-border-subtle bg-surface-0 px-3 py-3">
              <p className="text-xs font-medium text-text-primary mb-2">What you can do next</p>
              <ul className="space-y-1.5 text-xs text-text-secondary">
                {candidateAgent && (
                  <li>
                    <Link to={`/dashboard/agents/${encodeURIComponent(candidateAgent.id)}`} className="text-brand hover:underline">
                      Open {candidateAgent.name} settings
                    </Link>
                    {' '}— runtime mode, restrictions, secret detection, task auto-approval.
                  </li>
                )}
                <li>
                  <Link
                    to={candidateAgent ? `/dashboard/activity?agent_id=${encodeURIComponent(candidateAgent.id)}` : '/dashboard/activity'}
                    className="text-brand hover:underline"
                  >
                    View activity
                  </Link>
                  {' '}— see Clawvisor-routed calls and policy decisions for this agent.
                </li>
                <li>
                  <Link
                    to={candidateAgent ? `/dashboard/policy?agent_id=${encodeURIComponent(candidateAgent.id)}` : '/dashboard/policy'}
                    className="text-brand hover:underline"
                  >
                    Edit policy
                  </Link>
                  {' '}— set defaults for what {spec.label} can do without asking.
                </li>
                <li className="text-text-tertiary">
                  Uninstall reference saved at <code className="font-mono">~/.clawvisor/uninstall-{target}.md</code> on the helper's machine.
                </li>
              </ul>
            </div>
            <div className="flex items-center justify-end pt-2">
              <button
                onClick={startOver}
                className="text-xs text-text-tertiary hover:text-text-primary"
              >
                Connect another {spec.label} →
              </button>
            </div>
          </div>
        )}

      </div>

      <details className="group">
        <summary className="text-sm font-medium text-text-secondary cursor-pointer hover:text-text-primary select-none">
          Preview what the skill will do
        </summary>
        <div className="mt-3">
          <InstallerSkillPreview url={skillURL} />
        </div>
      </details>
    </div>
  )
}

function readInstallerPhase(target: InstallerTarget): InstallerPhase {
  try {
    const v = sessionStorage.getItem(`installer:${target}:phase`)
    if (v === 'past') return 'past'
    // 'apiKey' is a legacy phase value from the pre-this-refactor wizard;
    // map it to 'past' so a reload during an in-flight install resumes at
    // Register rather than restarting at Configure.
    if (v === 'apiKey') return 'past'
  } catch {}
  return 'configure'
}

function readInstallerSince(target: InstallerTarget): number | null {
  try {
    const v = sessionStorage.getItem(`installer:${target}:pastSinceMs`)
    if (!v) return null
    const n = parseInt(v, 10)
    return Number.isFinite(n) ? n : null
  } catch { return null }
}

function clearInstallerProgress(target: InstallerTarget) {
  try {
    sessionStorage.removeItem(`installer:${target}:phase`)
    sessionStorage.removeItem(`installer:${target}:pastSinceMs`)
  } catch {}
}

function screenToStepIndex(screen: InstallerScreen): number {
  switch (screen.kind) {
    case 'configure': return 0
    case 'register': return 1
    case 'approve': return 2
    case 'apiKey': return 3
    case 'runHelper': return 4
    case 'success': return 5
  }
}

function InstallerSetupQuestions({
  target,
  answers,
  onChange,
  showTitle = true,
}: {
  target: InstallerTarget
  answers: InstallerAnswers
  onChange: (answers: InstallerAnswers) => void
  showTitle?: boolean
}) {
  const targetLabel = INSTALLER_SPECS[target].label
  const set = <K extends keyof InstallerAnswers>(key: K, value: InstallerAnswers[K]) => {
    onChange({ ...answers, [key]: value })
  }
  return (
    <div className="space-y-4">
      <div>
        {showTitle && (
          <p className="text-sm font-medium text-text-primary">Answer setup questions</p>
        )}
        <p className={`text-xs text-text-tertiary${showTitle ? ' mt-1' : ''}`}>
          These answers are baked into the installer skill URL, so the helper
          follows your preferences instead of asking again.
        </p>
      </div>

      <QuestionToggleGroup
        label={`Which upstream LLM provider should ${targetLabel} use?`}
        value={answers.llmProvider}
        onChange={value => set('llmProvider', value as InstallerAnswers['llmProvider'])}
        options={[
          ['anthropic', 'Anthropic'],
          ['openai', 'OpenAI'],
        ]}
      />

      {target === 'hermes' && (
        <>
          <QuestionToggleGroup
            label="Where is Hermes running?"
            value={answers.hermesMode}
            onChange={value => set('hermesMode', value as InstallerAnswers['hermesMode'])}
            options={[
              ['host', 'On this machine'],
              ['docker', 'In Docker on this machine'],
              ['remote', 'On another machine'],
            ]}
          />
          <QuestionToggleGroup
            label="How should Hermes store its Clawvisor settings?"
            value={answers.hermesConfig}
            onChange={value => set('hermesConfig', value as InstallerAnswers['hermesConfig'])}
            options={[
              ['env', 'Environment-variable launch command'],
              ['file', 'Persistent ~/.hermes/config.yaml'],
            ]}
          />
        </>
      )}

      {target === 'openclaw' && (
        <QuestionToggleGroup
          label="Where is OpenClaw running?"
          value={answers.openclawMode}
          onChange={value => set('openclawMode', value as InstallerAnswers['openclawMode'])}
          options={[
            ['host', 'On this machine'],
            ['docker', 'In Docker on this machine'],
            ['remote', 'On another machine'],
          ]}
        />
      )}
    </div>
  )
}

function InstallerSkillPreview({ url }: { url: string }) {
  const { data, isLoading, error } = useQuery({
    queryKey: ['installer-skill-preview', url],
    queryFn: async () => {
      const r = await fetch(url)
      if (!r.ok) throw new Error(`HTTP ${r.status}`)
      return r.text()
    },
    staleTime: 5 * 60 * 1000,
  })
  if (isLoading) return <p className="text-xs text-text-tertiary">Loading preview…</p>
  if (error) return <p className="text-xs text-danger">Couldn't load preview.</p>
  return (
    <pre className="text-xs font-mono whitespace-pre-wrap text-text-secondary bg-surface-0 border border-border-subtle rounded p-3 max-h-96 overflow-y-auto">
      {data}
    </pre>
  )
}

// ── Claude Desktop configuration-profile path ────────────────────────────────
//
// Claude Desktop reads a macOS managed configuration profile rather than env
// vars or a skill — Anthropic ships com.anthropic.claudefordesktop payloads
// with inferenceProvider/inferenceGatewayBaseUrl/inferenceGatewayApiKey keys.
// The dashboard download endpoint mints a fresh agent and bakes its token
// into the plist at request time; the user double-clicks the file to install.

function ClaudeDesktopProfileGuide() {
  const qc = useQueryClient()
  const [isDownloading, setIsDownloading] = useState(false)
  const [downloadError, setDownloadError] = useState<string | null>(null)
  const { data: userCreds } = useQuery({
    queryKey: ['llm-credentials', 'user'],
    queryFn: () => api.llmCredentials.list(),
  })
  const keyReady = hasProviderUpstreamKey(userCreds, 'anthropic')
  const downloadProfile = async () => {
    if (!keyReady) {
      setDownloadError('Add an Anthropic API key before downloading the profile.')
      return
    }
    setIsDownloading(true)
    setDownloadError(null)
    try {
      const { blob, filename } = await api.installer.downloadClaudeDesktopProfile()
      const url = URL.createObjectURL(blob)
      const a = document.createElement('a')
      a.href = url
      a.download = filename ?? 'claude-desktop.mobileconfig'
      document.body.appendChild(a)
      a.click()
      a.remove()
      // Defer the revoke a tick: Safari (and historically Firefox) dispatch
      // the download asynchronously, and revoking the blob URL on the same
      // tick as `.click()` can cancel the download or land an empty file.
      window.setTimeout(() => URL.revokeObjectURL(url), 0)
      qc.invalidateQueries({ queryKey: ['agents', 'personal'] })
    } catch (err) {
      const message = err instanceof Error ? err.message : 'Could not download configuration profile'
      setDownloadError(message)
    } finally {
      setIsDownloading(false)
    }
  }

  return (
    <div className="space-y-5">
      <p className="text-sm text-text-secondary">
        Claude Desktop reads a macOS configuration profile to discover its
        inference gateway. Download the profile below, open it, and macOS
        installs it via System Settings → Profiles. The download itself mints
        the agent and bakes the token into the file.
      </p>

      <div className="rounded-md border border-border-default bg-surface-1 px-4 py-5 space-y-5">
        <div className="flex items-start gap-3">
          <StepNumber n={1} />
          <div className="space-y-1.5 min-w-0 flex-1">
            <VaultKeyStep
              provider="anthropic"
              title="Add Anthropic API key"
              description="Claude Desktop uses a configuration profile and sends model requests to Clawvisor with a cvis_ token. Clawvisor needs your upstream Anthropic API key vaulted before the profile is installed."
            />
          </div>
        </div>

        <div className="flex items-start gap-3">
          <StepNumber n={2} />
          <div className="space-y-1.5 min-w-0 flex-1">
            <p className="text-sm font-medium text-text-primary">Download the profile</p>
            <button
              type="button"
              onClick={downloadProfile}
              disabled={isDownloading || !keyReady}
              className="inline-block bg-brand text-surface-0 font-medium rounded px-5 py-2 text-sm hover:bg-brand-strong disabled:opacity-50 disabled:cursor-not-allowed"
            >
              {isDownloading ? 'Preparing Profile…' : 'Download Configuration Profile'}
            </button>
            {downloadError && (
              <p className="text-xs text-danger mt-1">{downloadError}</p>
            )}
            <p className="text-xs text-text-tertiary">
              Each download mints a fresh agent. Re-downloading creates a
              sequenced agent (claude-desktop-2, …) — older installs keep
              working until you revoke them under <strong>Your Agents</strong>.
            </p>
          </div>
        </div>

        <div className="flex items-start gap-3">
          <StepNumber n={3} />
          <div className="space-y-1.5 min-w-0 flex-1">
            <p className="text-sm font-medium text-text-primary">Open the file</p>
            <p className="text-xs text-text-tertiary">
              Double-click{' '}
              <code className="font-mono text-text-secondary">claude-desktop.mobileconfig</code>{' '}
              in your Downloads folder. macOS opens <strong>System Settings → Profiles</strong>;
              click <strong>Install</strong> and enter your password.
            </p>
          </div>
        </div>

        <div className="flex items-start gap-3">
          <StepNumber n={4} />
          <div className="space-y-1.5 min-w-0 flex-1">
            <p className="text-sm font-medium text-text-primary">Restart Claude Desktop</p>
            <p className="text-xs text-text-tertiary">
              Quit Claude Desktop fully (⌘Q, not just close the window) and
              reopen. Your next message routes through Clawvisor.
            </p>
          </div>
        </div>
      </div>

      <details className="group">
        <summary className="text-sm font-medium text-text-secondary cursor-pointer hover:text-text-primary select-none">
          How do I remove it later?
        </summary>
        <div className="mt-3 text-xs text-text-tertiary space-y-2">
          <p>
            macOS: System Settings → Privacy & Security → Profiles → select
            "Claude Desktop Third-Party Inference" → Remove. Then revoke the
            agent under <strong>Your Agents</strong> in this dashboard.
          </p>
        </div>
      </details>
    </div>
  )
}

// ── Cloud-agent connect path ────────────────────────────────────────────────
//
// Escape hatch from the LLM-proxy default for vendor-locked chat agents
// (Perplexity Computer, hosted ChatGPT, etc.) where the user can't redirect
// the model endpoint *and* doesn't have a terminal in the conversation.
// What they do have is a chat box — so we follow the same primitive the
// legacy non-proxy OpenClaw / Other-agent flow uses: hand the user a prompt
// to paste in, the agent fetches /skill/setup.md, self-registers via the
// standard Clawvisor skill, the user approves the resulting connection in
// the Pending Connections section below the wizard.
//
// Differs from LegacyOpenClawGuide only in framing (cloud-specific copy +
// LLM-proxy-stays-with-vendor explainer) so the experience is otherwise
// the same battle-tested path that's been working for non-proxy users.

function CloudAgentPromptGuide({
  setupURL,
  clawvisorURL,
  copied,
  onCopy,
}: {
  setupURL: string
  clawvisorURL: string
  copied: boolean
  onCopy: (text: string) => void
}) {
  const [promptAcknowledged, setPromptAcknowledged] = useState(false)
  const { data: agents } = useQuery({
    queryKey: ['agents', 'personal'],
    queryFn: () => api.agents.list(),
    refetchInterval: 3000,
  })
  const connectedAgent = agents?.find(a => a.install_context?.harness === 'cloud-agent')

  const prompt = `Please install Clawvisor. It's a security gateway between you and external services like Gmail, Slack, and GitHub. You don't hold any API keys directly; instead, you make requests through Clawvisor and I approve which actions you can take. Every call is logged, and I can revoke access at any time.\n\nSetup is just registering an agent token and installing a skill that teaches you how to use it. I'll review each step before it happens.\n\nInstructions: ${setupURL}`

  const step1Variant = promptAcknowledged ? 'complete' : 'active'
  const step2Variant = connectedAgent
    ? 'complete'
    : promptAcknowledged
      ? 'active'
      : 'upcoming'

  return (
    <div className="space-y-5">
      <p className="text-sm text-text-secondary">
        For hosted agents (Perplexity Computer, hosted ChatGPT, any harness
        where you can't change the LLM endpoint). Paste the prompt below into
        your agent — it will fetch the setup instructions, register itself,
        and wait for your approval. Your LLM traffic stays with the vendor;
        Clawvisor only intermediates external service calls.
      </p>

      <div className="space-y-3">
        <SetupGuideStep
          step={1}
          title="Paste this into your agent"
          description="The agent follows the setup instructions at that URL — registers itself, sets up E2E encryption, and installs the Clawvisor skill."
          completedSummary="Setup prompt copied"
          variant={step1Variant}
          copyValue={prompt}
          copyLabel="setup prompt"
        >
          <LegacyPromptBlock prompt={prompt} copied={copied} onCopy={onCopy} />
          {!promptAcknowledged && (
            <WizardNav
              canBack={false}
              canNext
              onBack={() => {}}
              onNext={() => setPromptAcknowledged(true)}
              nextLabel="Continue"
              showTopBorder={false}
              alignNext="left"
            />
          )}
        </SetupGuideStep>

        <SetupGuideStep
          step={2}
          title="Approve the connection"
          description={<>A connection request will appear in the <strong>Pending Connections</strong> section below. Click <strong>Approve</strong> to grant the agent a token.</>}
          completedSummary={connectedAgent ? <>Connected as {connectedAgent.name}</> : undefined}
          variant={step2Variant}
        >
          {promptAcknowledged ? (
            <p className="text-xs text-text-tertiary">
              {connectedAgent
                ? <>Agent <strong className="text-text-secondary">{connectedAgent.name}</strong> is connected and ready to go.</>
                : 'Waiting for your agent to register. Approve the pending connection when it appears below.'}
            </p>
          ) : (
            <p className="text-xs text-text-tertiary">
              Paste the setup prompt into your agent first — this step unlocks once you continue.
            </p>
          )}
        </SetupGuideStep>
      </div>

      <details className="group">
        <summary className="text-sm font-medium text-text-secondary cursor-pointer hover:text-text-primary select-none">
          Manual setup (token + environment variables)
        </summary>
        <div className="mt-4 space-y-4 pl-0">
          <div className="flex items-start gap-3">
            <StepNumber n={1} />
            <div className="space-y-1.5 min-w-0 flex-1">
              <p className="text-sm font-medium text-text-primary">Create an agent token</p>
              <p className="text-xs text-text-tertiary">
                Use the <strong>Create Agent</strong> form below. Copy the token — it's shown only once.
              </p>
            </div>
          </div>

          <div className="flex items-start gap-3">
            <StepNumber n={2} />
            <div className="space-y-1.5 min-w-0 flex-1">
              <p className="text-sm font-medium text-text-primary">Configure the agent's environment</p>
              <p className="text-xs text-text-tertiary">
                Paste these into wherever the agent reads credentials (vendor settings UI,
                integration token slot, etc.):
              </p>
              <CodeBlock>{`CLAWVISOR_URL=${clawvisorURL}\nCLAWVISOR_AGENT_TOKEN=<your token>`}</CodeBlock>
            </div>
          </div>
        </div>
      </details>
    </div>
  )
}

// ── GBrain streamlined connect path ─────────────────────────────────────────
//
// The fully-inline GBrain wizard: mint an agent (token returned to the
// dashboard), activate Gmail / Calendar / Contacts via OAuth popups, create
// a standing task with the recipe-canonical expansive purpose + lenient
// strictness, approve it inline, then hand the user three env vars
// (CLAWVISOR_URL / CLAWVISOR_AGENT_TOKEN / CLAWVISOR_TASK_ID) ready to paste
// to a downstream agent that will write them into GBrain's environment.
//
// Why not the bootstrap-curl path? GBrain is already installed on the user's
// machine — the dashboard creating the agent directly is one fewer
// terminal-paste, and we need the token in browser memory anyway to call
// POST /api/tasks as the agent.

const GBRAIN_AGENT_NAME = 'gbrain'
const GBRAIN_AGENT_DESCRIPTION = 'GBrain personal-brain data pipeline'
const GBRAIN_PURPOSE = 'Full executive assistant access to Gmail, Calendar, and Contacts including inbox triage, event listing, contact lookup, and historical data access for all connected Google accounts.'
// Service IDs match the adapter slugs in internal/adapters/google/*/adapter.go.
// Wildcard action with auto_execute + lenient verification mirrors the
// "expansive access" posture the credential-gateway recipe expects — GBrain
// reads a lot of methods across each service, and per-call intent
// verification would block the pipeline.
const GBRAIN_SERVICES: { id: string; label: string }[] = [
  { id: 'google.gmail', label: 'Gmail' },
  { id: 'google.calendar', label: 'Google Calendar' },
  { id: 'google.contacts', label: 'Google Contacts' },
]

type GBrainStep = 'mint' | 'services' | 'task' | 'env'

function GBrainStreamlinedGuide({
  clawvisorURL,
  onCopy,
}: {
  clawvisorURL: string
  onCopy: (text: string) => void
}) {
  const qc = useQueryClient()
  const [step, setStep] = useState<GBrainStep>('mint')
  // agentToken is held in component state only — never persisted. If the
  // user reloads mid-flow they have to delete the orphan agent and start
  // over; that's the right trade for not leaking the token into storage.
  const [agent, setAgent] = useState<Agent | null>(null)
  const [taskId, setTaskId] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)

  const { data: agents } = useQuery({
    queryKey: ['agents', 'personal'],
    queryFn: () => api.agents.list(),
  })

  // Poll services while we're on the activate step so the cards flip to
  // "activated" the moment the OAuth popup completes. Refetch-on-focus
  // would also work but is racier — explicit 2s polling is cheap and lands
  // the UI within a second of the user returning to the dashboard tab.
  const { data: servicesData } = useQuery({
    queryKey: ['services'],
    queryFn: () => api.services.list(),
    refetchInterval: step === 'services' ? 2000 : false,
  })

  const services = servicesData?.services ?? []
  // The services list is flattened per (service_id, alias) connection — one
  // entry per activated account. So Gmail with two accounts shows as two
  // ServiceInfo entries with the same id but different alias. Group them
  // back into (service → list of activated aliases) so we can render
  // multi-account rows + qualify task scopes correctly.
  //
  // Bare entries (alias undefined/"") are returned for the canonical
  // "default" connection — the gateway error message normalizes those to
  // alias "default" in its ALIAS_NOT_FOUND response, so we mirror that
  // normalization here so the task POST sends the same identifier the
  // gateway will accept.
  const connectedAliases = useMemo(() => {
    const map = new Map<string, string[]>()
    for (const s of services) {
      if (s.status !== 'activated') continue
      const target = GBRAIN_SERVICES.find(g => g.id === s.id)
      if (!target) continue
      const alias = (s.alias && s.alias.trim()) || 'default'
      const list = map.get(s.id) ?? []
      if (!list.includes(alias)) list.push(alias)
      map.set(s.id, list)
    }
    return map
  }, [services])
  const aliasesFor = (id: string) => connectedAliases.get(id) ?? []
  const allServicesHaveAccount = GBRAIN_SERVICES.every(s => aliasesFor(s.id).length > 0)

  const mintMutation = useMutation({
    mutationFn: async () => {
      const name = nextAvailableName(GBRAIN_AGENT_NAME, agents)
      return api.agents.create(name, GBRAIN_AGENT_DESCRIPTION)
    },
    onSuccess: (a) => {
      setAgent(a)
      qc.invalidateQueries({ queryKey: ['agents', 'personal'] })
      qc.invalidateQueries({ queryKey: ['agents'] })
      setStep('services')
    },
    onError: (e: Error) => setError(e.message),
  })

  // Expand (service, alias) pairs into qualified `service:alias` strings the
  // gateway accepts. The gateway requires an alias-qualified service field
  // whenever any aliases beyond a single bare connection exist for the
  // service (ALIAS_NOT_FOUND otherwise). Sending the qualified form
  // unconditionally is safe — it's also the disambiguation form the gateway
  // error message itself recommends.
  const qualifiedAuthorizedActions = useMemo(() => {
    const out: { service: string; action: string; auto_execute: boolean; verification: string }[] = []
    for (const target of GBRAIN_SERVICES) {
      for (const alias of aliasesFor(target.id)) {
        out.push({
          service: `${target.id}:${alias}`,
          action: '*',
          auto_execute: true,
          verification: 'lenient',
        })
      }
    }
    return out
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [connectedAliases])

  const createTaskMutation = useMutation({
    mutationFn: async () => {
      if (!agent?.token) throw new Error('Agent token missing — restart the wizard')
      if (qualifiedAuthorizedActions.length === 0) {
        throw new Error('No connected accounts found — go back and authorize at least one account per service')
      }
      const body = {
        purpose: GBRAIN_PURPOSE,
        authorized_actions: qualifiedAuthorizedActions,
        intent_verification_mode: 'lenient',
        lifetime: 'standing',
        schema_version: 2,
      }
      // POST /api/tasks requires an agent bearer token (tasks.go Create
      // pulls the agent from request context via middleware.AgentFromContext).
      // The dashboard's normal `api` client uses session auth, so this is a
      // one-off fetch with the in-memory cvis_… token.
      const res = await fetch(`${clawvisorURL}/api/tasks`, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'Authorization': `Bearer ${agent.token}`,
        },
        body: JSON.stringify(body),
      })
      if (!res.ok) {
        const txt = await res.text()
        throw new Error(`Task creation failed (${res.status}): ${txt}`)
      }
      return await res.json() as { task_id: string; status: string }
    },
    onSuccess: (data) => {
      setTaskId(data.task_id)
      qc.invalidateQueries({ queryKey: ['tasks'] })
    },
    onError: (e: Error) => setError(e.message),
  })

  // Two callers: first-time activation (newAccount undefined) and "Add
  // another account" on an already-activated service (newAccount=true).
  // The newAccount=true variant tells the OAuth handler to bypass the
  // already_authorized shortcut and force a fresh consent so a different
  // Google account can be selected — same flag the Services page uses.
  // Delegates to Services.openOAuthUrl so mobile redirect / popup fallback
  // matches the canonical flow.
  const openServiceOAuth = async (serviceId: string, newAccount = false) => {
    setError(null)
    try {
      const resp = await api.services.oauthGetUrl(serviceId, undefined, undefined, newAccount)
      if (resp.already_authorized) {
        qc.invalidateQueries({ queryKey: ['services'] })
        return
      }
      if (resp.url) openOAuthUrl(resp.url)
    } catch (e) {
      setError((e as Error).message ?? 'Failed to start OAuth flow')
    }
  }

  // Poll the tasks list while we're on the approve step so the wizard
  // auto-advances when the user clicks Approve inside TaskCard. We don't
  // own the approve mutation here — TaskCard does — so the status change
  // is the only signal we have that approval landed. List is filtered to
  // the agent_id so we don't refetch the whole user's task history at 2s.
  const { data: tasksData } = useQuery({
    queryKey: ['tasks', 'gbrain-wizard', agent?.id ?? 'none'],
    queryFn: () => api.tasks.list({ limit: 100 }),
    enabled: !!taskId && step === 'task',
    refetchInterval: step === 'task' ? 2000 : false,
  })
  const currentTask = useMemo(
    () => (taskId ? tasksData?.tasks.find(t => t.id === taskId) : undefined),
    [tasksData, taskId],
  )

  // TaskCard owns the approve mutation; we watch for the status to flip to
  // active and advance the wizard. `denied` is a terminal failure mode the
  // user has to recover from by going back to authorize step (which leaves
  // the orphan denied task in their list — they can revoke it from /tasks).
  useEffect(() => {
    if (step !== 'task') return
    if (currentTask?.status === 'active') setStep('env')
  }, [currentTask?.status, step])

  const wizardSteps: WizardStepDef[] = [
    { id: 'mint', title: 'Create agent', done: !!agent },
    { id: 'services', title: 'Authorize Google', done: allServicesHaveAccount && !!agent },
    { id: 'task', title: 'Approve task', done: step === 'env' },
    { id: 'env', title: 'Env vars', done: step === 'env' },
  ]
  const activeIndex = step === 'mint' ? 0 : step === 'services' ? 1 : step === 'task' ? 2 : 3

  return (
    <div className="space-y-5">
      <p className="text-sm text-text-secondary">
        Set up GBrain end to end without leaving this page. We'll mint a
        Clawvisor agent, authorize Gmail / Calendar / Contacts, approve a
        standing task with expansive access, and hand you three environment
        variables to paste to GBrain. None of GBrain's own LLM keys are touched.
      </p>

      <div className="rounded-md border border-border-default bg-surface-1 px-4 py-5 space-y-4">
        <div className="overflow-x-auto pb-1">
          <StepBar steps={wizardSteps} activeIndex={activeIndex} />
        </div>

        {error && (
          <div className="rounded border border-danger/30 bg-danger/10 px-3 py-2 text-xs text-danger">
            {error}
          </div>
        )}

        {step === 'mint' && (
          <div className="space-y-3">
            <div>
              <p className="text-sm font-medium text-text-primary">Create a Clawvisor agent for GBrain</p>
              <p className="text-xs text-text-tertiary mt-1">
                We'll register a new agent named <code className="font-mono">{nextAvailableName(GBRAIN_AGENT_NAME, agents)}</code>{' '}
                with a fresh token. The token stays in this browser tab — if you reload, you'll
                start over.
              </p>
            </div>
            <button
              onClick={() => { setError(null); mintMutation.mutate() }}
              disabled={mintMutation.isPending}
              className="bg-brand text-surface-0 font-medium rounded px-4 py-1.5 text-sm hover:bg-brand-strong disabled:opacity-50"
            >
              {mintMutation.isPending ? 'Creating…' : 'Create GBrain agent'}
            </button>
          </div>
        )}

        {step === 'services' && agent && (
          <div className="space-y-3">
            <div>
              <p className="text-sm font-medium text-text-primary">Authorize Google services</p>
              <p className="text-xs text-text-tertiary mt-1">
                Connect at least one Google account for each service. You can connect multiple
                accounts to the same service — every account you add will be in scope for the
                standing task on the next step. The rows below are the same connection cards
                as the Services page; rename, re-authorize, or deactivate from here.
              </p>
            </div>
            {GBRAIN_SERVICES.map(target => {
              const activated = services.filter(s => s.id === target.id && s.status === 'activated')
              const hasAny = activated.length > 0
              return (
                <div key={target.id} className="rounded border border-border-default bg-surface-0 overflow-hidden">
                  <div className="flex items-center justify-between px-5 py-3 bg-surface-1 border-b border-border-subtle">
                    <div className="flex items-center gap-2.5">
                      <span className={`h-2 w-2 rounded-full ${hasAny ? 'bg-success' : 'bg-text-tertiary'}`} />
                      <div>
                        <p className="text-sm font-medium text-text-primary">{target.label}</p>
                        <p className="text-xs text-text-tertiary font-mono">{target.id}</p>
                      </div>
                    </div>
                    <button
                      onClick={() => openServiceOAuth(target.id, hasAny)}
                      className="text-xs font-medium px-3 py-1.5 rounded border border-border-default text-text-primary bg-surface-1 hover:bg-surface-2"
                    >
                      {hasAny ? '+ Add account' : 'Authorize →'}
                    </button>
                  </div>
                  {hasAny ? (
                    <div className="divide-y divide-border-subtle">
                      {activated.map(svc => (
                        <ActiveServiceRow key={`${svc.id}:${svc.alias ?? 'default'}`} svc={svc} />
                      ))}
                    </div>
                  ) : (
                    <p className="px-5 py-3 text-xs text-text-tertiary">
                      No accounts connected yet. Click <strong>Authorize →</strong> to connect one.
                    </p>
                  )}
                </div>
              )
            })}
            <WizardNav
              canBack={false}
              canNext={allServicesHaveAccount}
              onBack={() => {}}
              onNext={() => { setError(null); setStep('task'); createTaskMutation.mutate() }}
              nextLabel="Continue"
              nextDisabledHint={allServicesHaveAccount ? undefined : 'Connect at least one account for each service to continue'}
            />
          </div>
        )}

        {step === 'task' && agent && (
          <div className="space-y-3">
            <div>
              <p className="text-sm font-medium text-text-primary">Approve the standing task</p>
              <p className="text-xs text-text-tertiary mt-1">
                This is the standard task approval card — the same one you'd see on the
                overview page if GBrain were already calling. We've prefilled the task with
                the credential-gateway recipe's purpose and posture; review the scope and
                approve when ready.
              </p>
            </div>

            <div className="rounded border border-warning/30 bg-warning/10 px-3 py-2.5">
              <p className="text-xs font-medium text-warning">Read what you're approving</p>
              <p className="text-xs text-text-secondary mt-1 leading-relaxed">
                GBrain will have standing access to read every message in your Gmail, every
                event on your Calendar, and every contact in your address book, across all
                connected Google accounts. Intent verification is set to <strong>lenient</strong>,
                which means GBrain's reads won't be challenged on a per-call basis. This is
                appropriate for a personal-brain pipeline that ingests everything; it would not
                be appropriate for an interactive agent. You can revoke at any time from the
                task's page.
              </p>
            </div>

            {createTaskMutation.isPending && (
              <p className="text-xs text-text-tertiary">Creating task…</p>
            )}

            {/* createTaskMutation failed: no taskId, not pending. Without an
                inline recovery the user is stranded — the outer error banner
                explains what went wrong, but the only escape is the
                "Choose a different agent" link which loses the minted agent
                and connected accounts. Offer Retry (re-run with the same
                qualified scopes) and Back (return to authorize step where
                they can adjust accounts before retrying). */}
            {!taskId && !createTaskMutation.isPending && createTaskMutation.isError && (
              <div className="flex items-center gap-2">
                <button
                  onClick={() => { setError(null); createTaskMutation.mutate() }}
                  className="bg-brand text-surface-0 font-medium rounded px-4 py-1.5 text-sm hover:bg-brand-strong"
                >
                  Retry
                </button>
                <button
                  onClick={() => { setError(null); setStep('services') }}
                  className="text-sm text-text-secondary hover:text-text-primary"
                >
                  ← Back to authorize
                </button>
              </div>
            )}

            {currentTask && currentTask.status !== 'denied' && currentTask.status !== 'revoked' && (
              <TaskCard task={currentTask} agentName={agent.name} />
            )}

            {currentTask?.status === 'denied' && (
              <div className="rounded border border-danger/30 bg-danger/10 px-3 py-2.5 space-y-2">
                <p className="text-xs font-medium text-danger">Task denied.</p>
                <p className="text-xs text-text-secondary">
                  Go back to the authorize step to adjust accounts, or revoke the denied task
                  from <Link to="/dashboard/tasks" className="text-brand hover:underline">Tasks</Link>{' '}
                  and re-run the wizard.
                </p>
                <button
                  onClick={() => { setError(null); setStep('services'); setTaskId(null) }}
                  className="text-xs font-medium px-3 py-1.5 rounded border border-border-default text-text-primary bg-surface-1 hover:bg-surface-2"
                >
                  ← Back to authorize
                </button>
              </div>
            )}

            {taskId && !currentTask && !createTaskMutation.isPending && (
              <p className="text-xs text-text-tertiary">Fetching task…</p>
            )}
          </div>
        )}

        {step === 'env' && agent && agent.token && taskId && (
          <div className="space-y-3">
            <div>
              <p className="text-sm font-medium text-text-primary">Hand these to GBrain</p>
              <p className="text-xs text-text-tertiary mt-1">
                Three env vars. Paste them to the agent that will write them into GBrain's
                environment — the gateway requires <code className="font-mono">CLAWVISOR_TASK_ID</code>{' '}
                on every call, so all three are needed.
              </p>
            </div>
            <GBrainEnvExport
              clawvisorURL={clawvisorURL}
              token={agent.token}
              taskId={taskId}
              onCopy={onCopy}
            />
            <div className="rounded border border-border-subtle bg-surface-0 px-3 py-3">
              <p className="text-xs font-medium text-text-primary mb-2">What you can do next</p>
              <ul className="space-y-1.5 text-xs text-text-secondary">
                <li>
                  <Link to={`/dashboard/agents/${encodeURIComponent(agent.id)}`} className="text-brand hover:underline">
                    Open {agent.name} settings
                  </Link>
                  {' '}— restrictions, secret detection, runtime mode.
                </li>
                <li>
                  <Link to={`/dashboard/tasks/${encodeURIComponent(taskId)}`} className="text-brand hover:underline">
                    Open the task
                  </Link>
                  {' '}— revoke, edit scope, or watch activity.
                </li>
                <li>
                  <Link to={`/dashboard/activity?agent_id=${encodeURIComponent(agent.id)}`} className="text-brand hover:underline">
                    View activity
                  </Link>
                  {' '}— gateway calls GBrain has made.
                </li>
              </ul>
            </div>
          </div>
        )}
      </div>
    </div>
  )
}

function GBrainEnvExport({
  clawvisorURL,
  token,
  taskId,
  onCopy,
}: {
  clawvisorURL: string
  token: string
  taskId: string
  onCopy: (text: string) => void
}) {
  const block = `export CLAWVISOR_URL="${clawvisorURL}"
export CLAWVISOR_AGENT_TOKEN="${token}"
export CLAWVISOR_TASK_ID="${taskId}"`
  return <CodeBlock onCopy={() => onCopy(block)}>{block}</CodeBlock>
}

// ── Connection request card ──────────────────────────────────────────────────

function InstallContextSummary({ ctx }: { ctx: InstallContext }) {
  const pieces: string[] = []
  if (ctx.harness) pieces.push(ctx.harness)
  if (ctx.harness_version) pieces.push(`v${ctx.harness_version}`)
  if (ctx.install_mode && ctx.install_mode !== 'host') pieces.push(ctx.install_mode)
  if (ctx.host_os) pieces.push(ctx.host_os)
  if (ctx.alias_intent === 'yolo') pieces.push('alias: --yolo')
  else if (ctx.alias_intent === 'safe') pieces.push('alias: safe')
  if (ctx.auth_mode === 'swap') pieces.push('swap mode')
  if (pieces.length === 0) return null
  return (
    <div className="mt-2 text-xs text-text-tertiary flex flex-wrap gap-x-2 gap-y-1">
      {pieces.map((p) => (
        <span key={p} className="inline-block bg-surface-2 rounded px-1.5 py-0.5">
          {p}
        </span>
      ))}
    </div>
  )
}

function ConnectionCard({
  request: cr,
  onApproved,
}: {
  request: ConnectionRequest
  onApproved?: () => void
}) {
  const qc = useQueryClient()
  const [result, setResult] = useState<string | null>(null)

  const approveMut = useMutation({
    mutationFn: () => api.connections.approve(cr.id),
    onSuccess: () => {
      setResult('Approved')
      onApproved?.()
      qc.invalidateQueries({ queryKey: ['connections'] })
      qc.invalidateQueries({ queryKey: ['agents'] })
      qc.invalidateQueries({ queryKey: ['overview'] })
      qc.invalidateQueries({ queryKey: ['welcome'] })
      // Approval burns the bootstrap claim that produced this request. A
      // follow-up "Connect another" in the same session would otherwise
      // render the cached (now-consumed) claim in the curl until the 4-min
      // refetch — and the next POST to /api/agents/connect would 401 with
      // INVALID_CLAIM.
      qc.invalidateQueries({ queryKey: ['connection-claim'] })
    },
    onError: (err: Error) => setResult(`Failed: ${err.message}`),
  })

  const denyMut = useMutation({
    mutationFn: () => api.connections.deny(cr.id),
    onSuccess: () => {
      setResult('Denied')
      qc.invalidateQueries({ queryKey: ['connections'] })
      qc.invalidateQueries({ queryKey: ['overview'] })
      // See approveMut: deny also burns the claim.
      qc.invalidateQueries({ queryKey: ['connection-claim'] })
    },
    onError: (err: Error) => setResult(`Failed: ${err.message}`),
  })

  const isPending = approveMut.isPending || denyMut.isPending

  if (result) {
    return (
      <div className="border border-border-default rounded-md bg-surface-1 px-5 py-4">
        <div className="flex items-center justify-between">
          <span className="font-medium text-text-primary">{cr.name}</span>
          <span className={`text-xs font-medium px-2 py-0.5 rounded ${
            result === 'Approved' ? 'bg-success/10 text-success' :
            result === 'Denied' ? 'bg-danger/10 text-danger' :
            'bg-surface-2 text-text-tertiary'
          }`}>
            {result}
          </span>
        </div>
      </div>
    )
  }

  return (
    <div className="bg-surface-1 border border-border-default rounded-md border-l-[3px] border-l-brand overflow-hidden">
      <div className="px-5 pt-5 pb-4">
        <div className="flex items-center justify-between">
          <span className="font-mono text-lg font-semibold text-text-primary">{cr.name}</span>
          <CountdownTimer expiresAt={cr.expires_at} />
        </div>
        {cr.description && (
          <p className="text-sm text-text-secondary mt-1.5">{cr.description}</p>
        )}
        {cr.install_context && <InstallContextSummary ctx={cr.install_context} />}
        <div className="flex items-center gap-3 mt-2 text-xs text-text-tertiary">
          <span>IP: <code className="font-mono">{cr.ip_address}</code></span>
          <span>Requested {formatDistanceToNow(new Date(cr.created_at), { addSuffix: true })}</span>
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

// ── Lite-proxy LLM credentials panel ─────────────────────────────────────────
//
// Stores the upstream API key (sk-ant-..., sk-...) the lite-proxy swaps in
// when forwarding /api/v1/messages and /api/v1/chat/completions for this specific
// agent. Falls back to the user-level credential when the agent-scoped one
// isn't set, so configuring this is optional.
function AgentLLMCredentialsPanel({ agentId }: { agentId: string }) {
  const qc = useQueryClient()
  const [open, setOpen] = useState(false)
  const [editingProvider, setEditingProvider] = useState<string | null>(null)
  const [apiKey, setApiKey] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [success, setSuccess] = useState<string | null>(null)

  const { data: creds } = useQuery({
    queryKey: ['llm-credentials', agentId],
    queryFn: () => api.llmCredentials.list(agentId),
    enabled: open,
  })

  const setMut = useMutation({
    mutationFn: (params: { provider: string; key: string }) =>
      api.llmCredentials.set(params.provider, params.key, agentId),
    onSuccess: (_data, vars) => {
      qc.invalidateQueries({ queryKey: ['llm-credentials', agentId] })
      setEditingProvider(null)
      setApiKey('')
      setError(null)
      setSuccess(`Stored ${vars.provider} key for this agent`)
      setTimeout(() => setSuccess(null), 5000)
    },
    onError: (err: Error) => setError(err.message),
  })

  const deleteMut = useMutation({
    mutationFn: (provider: string) => api.llmCredentials.delete(provider, agentId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['llm-credentials', agentId] })
      setSuccess('Deleted agent-scoped key — falling back to user-level credential')
      setTimeout(() => setSuccess(null), 5000)
    },
    onError: (err: Error) => setError(err.message),
  })

  function startEditing(provider: string) {
    setEditingProvider(provider)
    setApiKey('')
    setError(null)
  }

  function handleSubmit(provider: string) {
    if (!apiKey) { setError('API key is required'); return }
    setError(null)
    setMut.mutate({ provider, key: apiKey })
  }

  return (
    <div className="mt-3 rounded border border-border-subtle bg-surface-0">
      <button
        onClick={() => setOpen(v => !v)}
        className="flex w-full items-center justify-between px-4 py-3 text-left"
      >
        <div>
          <div className="text-sm font-medium text-text-primary">Lite-proxy LLM credentials</div>
          <div className="text-xs text-text-tertiary">
            Per-agent override for the upstream Anthropic / OpenAI API key the proxy swaps in. Falls back to your user-level key.
          </div>
        </div>
        <span className="text-xs text-text-tertiary">{open ? 'Hide' : 'Configure'}</span>
      </button>
      {open && (
        <div className="border-t border-border-subtle px-4 py-4 space-y-3">
          {error && <div className="text-sm text-danger">{error}</div>}
          {success && <div className="text-sm text-success">{success}</div>}
          {creds?.credentials.map(c => (
            <div key={c.provider} className="rounded border border-border-default bg-surface-1 p-3 space-y-2">
              <div className="flex items-center justify-between">
                <div>
                  <div className="text-sm font-medium text-text-primary capitalize">{c.provider}</div>
                  <div className="text-xs text-text-tertiary mt-0.5">
                    {c.agent_stored ? (
                      <span className="text-success">Agent-scoped key set · overrides user-level</span>
                    ) : c.stored ? (
                      <span>Using user-level key (no agent-scoped override)</span>
                    ) : (
                      <span className="text-warning">No key configured at user or agent level</span>
                    )}
                  </div>
                </div>
                <div className="flex items-center gap-2">
                  {c.agent_stored && (
                    <button
                      onClick={() => {
                        if (confirm(`Remove the ${c.provider} agent-scoped key? This agent will fall back to the user-level key.`)) {
                          deleteMut.mutate(c.provider)
                        }
                      }}
                      disabled={deleteMut.isPending}
                      className="text-xs px-3 py-1 rounded border border-danger/30 text-danger hover:bg-danger/10 disabled:opacity-50"
                    >
                      Remove
                    </button>
                  )}
                  <button
                    onClick={() => startEditing(c.provider)}
                    className="text-xs px-3 py-1 rounded border border-brand/30 text-brand hover:bg-brand/10"
                  >
                    {c.agent_stored ? 'Replace' : 'Set agent-scoped key'}
                  </button>
                </div>
              </div>
              {editingProvider === c.provider && (
                <div className="space-y-2 pt-2 border-t border-border-subtle">
                  <input
                    type="password"
                    value={apiKey}
                    onChange={e => { setApiKey(e.target.value); setError(null) }}
                    placeholder={c.provider === 'anthropic' ? 'sk-ant-...' : 'sk-...'}
                    className="block w-full text-sm rounded border border-border-default bg-surface-0 text-text-primary px-3 py-1.5 focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand placeholder:text-text-tertiary"
                  />
                  <div className="flex items-center gap-2">
                    <button
                      onClick={() => handleSubmit(c.provider)}
                      disabled={setMut.isPending || !apiKey}
                      className="px-3 py-1 text-xs rounded bg-brand text-surface-0 hover:bg-brand-strong disabled:opacity-50"
                    >
                      {setMut.isPending ? 'Saving…' : 'Save'}
                    </button>
                    <button
                      onClick={() => { setEditingProvider(null); setApiKey(''); setError(null) }}
                      className="text-xs text-text-tertiary hover:text-text-primary"
                    >
                      Cancel
                    </button>
                  </div>
                </div>
              )}
            </div>
          ))}
        </div>
      )}
    </div>
  )
}

// ── Lite-proxy connection details panel ─────────────────────────────────────
//
// Surfaces the URLs and env vars an agent harness needs to point at this
// daemon's lite-proxy (vs. running through the runtime-proxy CONNECT
// path). Covers the three flagship harnesses: Claude Code, Codex CLI,
// and a generic OpenAI/Anthropic SDK.
function AgentLiteProxyPanel({ agentId: _agentId }: { agentId: string }) {
  const [open, setOpen] = useState(false)
  const { data: pairInfo } = useQuery({
    queryKey: ['pairInfo'],
    queryFn: () => api.devices.pairInfo(),
  })
  const { data: publicConfig } = useQuery({
    queryKey: ['public-config'],
    queryFn: () => api.config.public(),
  })
  // window.location.origin points at the relay when the dashboard is
  // accessed via the cloud, not the per-daemon mount the agent harness
  // must talk to. Prefer the configured cloud lite-proxy URL, then the
  // daemon-scoped relay path when we have one and the dashboard isn't
  // local; otherwise fall back to the origin.
  const isLocal = window.location.hostname === 'localhost' || window.location.hostname === '127.0.0.1'
  const hasRelay = !!(pairInfo?.daemon_id && pairInfo?.relay_host)
  const configuredProxyLiteURL = normalizePublicURL(publicConfig?.proxy_lite_public_url)
  const baseURL = !isLocal && configuredProxyLiteURL
    ? configuredProxyLiteURL
    : !isLocal && hasRelay
      ? `https://${pairInfo!.relay_host}/d/${pairInfo!.daemon_id}`
    : window.location.origin
  const [copied, setCopied] = useState<string | null>(null)

  function copy(label: string, value: string) {
    // navigator.clipboard is undefined in insecure (http://) or sandboxed
    // contexts. Calling .writeText on undefined throws synchronously, so
    // the .catch handler below never runs. Guard before dispatching.
    if (!navigator.clipboard || typeof navigator.clipboard.writeText !== 'function') {
      setCopied(`${label}-failed`)
      setTimeout(() => setCopied(null), 2000)
      return
    }
    navigator.clipboard.writeText(value)
      .then(() => {
        setCopied(label)
        setTimeout(() => setCopied(null), 2000)
      })
      .catch(() => {
        // writeText can also reject asynchronously (permission denied,
        // user gesture missing on Safari, etc.).
        setCopied(`${label}-failed`)
        setTimeout(() => setCopied(null), 2000)
      })
  }

  // Anthropic SDK + Claude CLI: env var is the API family base; the SDK appends
  // `/v1/messages` itself. OpenAI SDK + Codex: base URL includes `/api/v1`
  // because the client appends just the action path (`/chat/completions`).
  const claudeCode = `ANTHROPIC_BASE_URL=${baseURL}/api ANTHROPIC_CUSTOM_HEADERS='X-Clawvisor-Agent-Token: cvis_<this-agent-token>' ANTHROPIC_AUTH_TOKEN= ANTHROPIC_API_KEY= claude`
  const codex = `CLAWVISOR_AGENT_TOKEN=cvis_<this-agent-token> codex exec \\
  -c model_provider=clawvisor \\
  -c 'model_providers.clawvisor.base_url="${baseURL}/api/v1"' \\
  -c 'model_providers.clawvisor.wire_api="responses"' \\
  -c 'model_providers.clawvisor.requires_openai_auth=true' \\
  -c 'model_providers.clawvisor.env_http_headers={"X-Clawvisor-Agent-Token"="CLAWVISOR_AGENT_TOKEN"}' \\
  -c 'model="gpt-4o-mini"'`
  const openaiSDK = `from openai import OpenAI
client = OpenAI(
    base_url="${baseURL}/api/v1",
    api_key="cvis_<this-agent-token>",
)`
  const anthropicSDK = `import anthropic
client = anthropic.Anthropic(
    base_url="${baseURL}/api",
    api_key="cvis_<this-agent-token>",
)`

  return (
    <div className="mt-3 rounded border border-border-subtle bg-surface-0">
      <button
        onClick={() => setOpen(v => !v)}
        className="flex w-full items-center justify-between px-4 py-3 text-left"
      >
        <div>
          <div className="text-sm font-medium text-text-primary">Lite-proxy connection</div>
          <div className="text-xs text-text-tertiary">
            Point an agent harness at this daemon's LLM endpoint. Clawvisor authenticates the agent and either preserves upstream auth or swaps in a vaulted provider key.
          </div>
        </div>
        <span className="text-xs text-text-tertiary">{open ? 'Hide' : 'Show'}</span>
      </button>
      {open && (
        <div className="border-t border-border-subtle px-4 py-4 space-y-4">
          <div>
            <div className="text-xs uppercase tracking-wider text-text-tertiary">Base URL</div>
            <div className="mt-1 flex items-center gap-2">
              <code className="flex-1 px-3 py-1.5 text-sm font-mono rounded border border-border-default bg-surface-1 text-text-primary">{baseURL}/api/v1</code>
              <button
                onClick={() => copy('base', `${baseURL}/api/v1`)}
                className="text-xs px-3 py-1 rounded border border-border-strong text-text-secondary hover:bg-surface-2"
              >
                {copied === 'base' ? 'Copied!' : copied === 'base-failed' ? 'Copy failed' : 'Copy'}
              </button>
            </div>
          </div>

          {[
            { label: 'Claude Code', key: 'claude', body: claudeCode },
            { label: 'Codex CLI', key: 'codex', body: codex },
            { label: 'OpenAI Python SDK', key: 'oai', body: openaiSDK },
            { label: 'Anthropic Python SDK', key: 'ant', body: anthropicSDK },
          ].map(snippet => (
            <div key={snippet.key}>
              <div className="flex items-center justify-between">
                <div className="text-xs uppercase tracking-wider text-text-tertiary">{snippet.label}</div>
                <button
                  onClick={() => copy(snippet.key, snippet.body)}
                  className="text-xs px-3 py-1 rounded border border-border-strong text-text-secondary hover:bg-surface-2"
                >
                  {copied === snippet.key ? 'Copied!' : copied === `${snippet.key}-failed` ? 'Copy failed' : 'Copy'}
                </button>
              </div>
              <pre className="mt-1 px-3 py-2 text-xs font-mono rounded border border-border-default bg-surface-1 text-text-primary overflow-x-auto whitespace-pre-wrap">{snippet.body}</pre>
            </div>
          ))}
        </div>
      )}
    </div>
  )
}
