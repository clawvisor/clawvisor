import { Fragment, useEffect, useMemo, useRef, useState } from 'react'
import { Link, useNavigate, useParams, useSearchParams } from 'react-router-dom'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api, type Agent, type ApprovalRecord, type AgentRuntimeSettings, type AuditEntry, type RuntimePolicyRule, type RuntimeSession } from '../api/client'
import type { ConnectionRequest, InstallContext } from '../api/client'
import { useAuth } from '../hooks/useAuth'
import { formatDistanceToNow } from 'date-fns'
import CountdownTimer from '../components/CountdownTimer'
import TaskCard from '../components/TaskCard'
import { RuntimeApprovalsPanel, RuntimeSessionsPanel, filterLiveRuntimeApprovals, isActiveRuntimeSession } from './Runtime'
import { ActiveServiceRow, openOAuthUrl } from './Services'

export default function Agents() {
  const { currentOrg, features } = useAuth()
  const { agentId } = useParams()
  const navigate = useNavigate()
  const orgId = currentOrg?.id
  const qc = useQueryClient()
  const liveSessionsUI = !orgId && !!features?.agent_live_sessions
  const runtimePolicyUI = !orgId && !!features?.runtime_policy_ui
  const proxyLiteUI = !orgId && !!features?.proxy_lite
  // The Connect-an-Agent wizard owns its step via `?agent=<harness>`. While
  // a harness is picked, hide the global Pending Connections section —
  // the wizard renders its own copy inline so the user sees their request
  // in the same scroll position as the install instructions.
  const [topLevelSearchParams] = useSearchParams()
  const wizardActive = !orgId && !!topLevelSearchParams.get('agent')
  const [name, setName] = useState('')
  const [description, setDescription] = useState('')
  const [newToken, setNewToken] = useState<string | null>(null)
  const [formError, setFormError] = useState<string | null>(null)
  const [showCreateForm, setShowCreateForm] = useState(false)

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

  const createMut = useMutation({
    mutationFn: () => orgId
      ? api.orgs.createAgent(orgId, name, description)
      : api.agents.create(name, description).then(agent => ({ agent, token: agent.token ?? '' })),
    onSuccess: (result) => {
      qc.invalidateQueries({ queryKey: ['agents'] })
      qc.invalidateQueries({ queryKey: ['welcome'] })
      setNewToken(result.token ?? null)
      setName('')
      setDescription('')
      setFormError(null)
      setShowCreateForm(false)
    },
    onError: (err: Error) => setFormError(err.message),
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

  const selectedAgent = useMemo(() => agents?.find(agent => agent.id === agentId), [agents, agentId])

  if (agentId) {
    if (isLoading) {
      return <div className="p-4 sm:p-8 text-sm text-text-tertiary">Loading…</div>
    }
    if (!selectedAgent) {
      return (
        <div className="p-4 sm:p-8 space-y-4">
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
    <div className="p-4 sm:p-8 space-y-8">
      <h1 className="text-2xl font-bold text-text-primary">Agents</h1>
      <p className="text-sm text-text-tertiary">
        An agent is any AI system (Claude, a custom bot, etc.) that you want to give controlled access to your services.
        Each agent gets a unique token — paste it into your agent's configuration to connect it to Clawvisor.
      </p>

      {/* Connect an Agent guide (personal context only) */}
      {!orgId && <ConnectAgentGuide newToken={newToken} />}

      {/* Pending connection requests (personal context only).
          Hidden while the wizard is mid-flight — the wizard renders its
          own copy of these cards inline so the user can approve without
          scrolling. */}
      {!orgId && !wizardActive && pending.length > 0 && (
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

      {/* New token display */}
      {newToken && (
        <div className="bg-success/10 border border-success/30 rounded-md p-4 space-y-2">
          <p className="text-sm font-medium text-success">Agent created — copy your token now, it won't be shown again.</p>
          <div className="flex items-center gap-2">
            <code className="flex-1 bg-surface-1 border border-success/30 rounded px-3 py-2 text-xs font-mono text-text-primary break-all">
              {newToken}
            </code>
            <button
              onClick={() => navigator.clipboard.writeText(newToken)}
              className="text-xs px-3 py-1.5 rounded border border-success/30 text-success hover:bg-success/10"
            >
              Copy
            </button>
          </div>
          <button onClick={() => setNewToken(null)} className="text-xs text-success hover:underline">
            Dismiss
          </button>
        </div>
      )}

      {/* Agent list */}
      <section>
        <div className="flex items-center justify-between mb-3">
          <h2 className="text-lg font-semibold text-text-primary">Your Agents</h2>
          <button
            onClick={() => { setShowCreateForm(!showCreateForm); setFormError(null) }}
            className="text-sm px-3 py-1.5 rounded bg-brand text-surface-0 hover:bg-brand-strong"
          >
            {showCreateForm ? 'Cancel' : 'Add Agent'}
          </button>
        </div>

        {/* Inline create form */}
        {showCreateForm && (
          <div className="bg-surface-1 border border-border-default rounded-md p-4 mb-3 space-y-3">
            {formError && <div className="text-xs text-danger">{formError}</div>}
            <div className="flex gap-3">
              <div className="flex-1 space-y-3">
                <input
                  value={name}
                  onChange={e => setName(e.target.value)}
                  onKeyDown={e => { if (e.key === 'Enter' && name.trim()) createMut.mutate() }}
                  placeholder="Agent name"
                  autoFocus
                  className="w-full text-sm rounded border border-border-default bg-surface-0 text-text-primary px-3 py-1.5 focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand placeholder:text-text-tertiary"
                />
                <textarea
                  value={description}
                  onChange={e => setDescription(e.target.value)}
                  placeholder="Short description of what this agent does"
                  className="w-full min-h-[84px] text-sm rounded border border-border-default bg-surface-0 text-text-primary px-3 py-2 focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand placeholder:text-text-tertiary"
                />
              </div>
              <button
                onClick={() => createMut.mutate()}
                disabled={createMut.isPending || !name.trim()}
                className="self-start px-4 py-1.5 text-sm rounded bg-brand text-surface-0 hover:bg-brand-strong disabled:opacity-50"
              >
                {createMut.isPending ? 'Creating…' : 'Create'}
              </button>
            </div>
          </div>
        )}

        {isLoading && <div className="text-sm text-text-tertiary">Loading…</div>}

        {!isLoading && (!agents || agents.length === 0) && !showCreateForm && (
          <div className="text-sm text-text-tertiary text-center py-8 bg-surface-1 border border-border-default rounded-md">
            No agents yet. Follow the setup guides above or click <strong>Add Agent</strong> to create one manually.
          </div>
        )}

        <div className="space-y-2">
          {agents?.map(agent => {
            const hasActiveTasks = agent.active_task_count > 0
            const liveSessions = fullRuntimeSessionsUI ? (sessionsByAgent.get(agent.id) ?? []) : []
            const pendingApprovals = fullRuntimeSessionsUI ? (approvalsByAgent.get(agent.id) ?? []) : []
            return (
              <div
                key={agent.id}
                role="link"
                tabIndex={0}
                onClick={() => navigate(`/dashboard/agents/${agent.id}`)}
                onKeyDown={e => {
                  if (e.key === 'Enter' || e.key === ' ') {
                    e.preventDefault()
                    navigate(`/dashboard/agents/${agent.id}`)
                  }
                }}
                className={`bg-surface-1 border rounded-md px-5 py-4 flex flex-col sm:flex-row sm:items-center justify-between gap-3 ${
                  hasActiveTasks
                    ? 'border-brand/40 border-l-[3px] border-l-brand'
                    : 'border-border-default'
                } cursor-pointer hover:bg-surface-2 focus:outline-none focus:ring-2 focus:ring-brand/30`}
              >
                <div className="min-w-0 flex-1">
                  <div className="flex items-center gap-2">
                    <span className="font-medium text-text-primary truncate">
                      {agent.name}
                    </span>
                    {hasActiveTasks && (
                      <span className="text-xs font-medium px-2 py-0.5 rounded-full bg-brand/10 text-brand">
                        {agent.active_task_count} active {agent.active_task_count === 1 ? 'task' : 'tasks'}
                      </span>
                    )}
                    {fullRuntimeSessionsUI && liveSessions.length > 0 && (
                      <span className="text-xs font-medium px-2 py-0.5 rounded-full bg-success/10 text-success">
                        {liveSessions.length} live session{liveSessions.length === 1 ? '' : 's'}
                      </span>
                    )}
                    {fullRuntimeSessionsUI && pendingApprovals.length > 0 && (
                      <span className="text-xs font-medium px-2 py-0.5 rounded-full bg-warning/10 text-warning">
                        {pendingApprovals.length} pending approval{pendingApprovals.length === 1 ? '' : 's'}
                      </span>
                    )}
                  </div>
                  {agent.description && (
                    <p className="text-sm text-text-secondary mt-1 line-clamp-2">{agent.description}</p>
                  )}
                  <p className="text-xs text-text-tertiary mt-0.5">
                    Created {formatDistanceToNow(new Date(agent.created_at), { addSuffix: true })} · {agent.id}
                    {agent.last_task_at && (
                      <> · Last task {formatDistanceToNow(new Date(agent.last_task_at), { addSuffix: true })}</>
                    )}
                  </p>
                </div>
                <span className="text-xs text-text-tertiary">View details →</span>
              </div>
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
    <div className="p-4 sm:p-8 space-y-8">
      <div className="space-y-3">
        <Link to="/dashboard/agents" className="text-sm text-brand hover:underline">← Back to agents</Link>
        <div className="flex flex-wrap items-start justify-between gap-4">
          <div>
            <h1 className="text-2xl font-bold text-text-primary">{agent.name}</h1>
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

      {proxyLiteActive && <AgentLiteProxyPanel agent={agent} />}
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
            to={`/dashboard/agents?agent=${encodeURIComponent(reinstallTarget)}`}
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
                <span className="rounded border border-border-subtle px-2 py-0.5 text-[11px] uppercase tracking-wider text-text-tertiary">
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

type AgentTab = 'openclaw' | 'hermes' | 'claude-code' | 'codex' | 'claude-desktop' | 'gbrain' | 'cloud-agent' | 'other'

// `gbrain` and `cloud-agent` are escape hatches from the LLM-proxy default:
// both mint an agent token + standing task without trying to wire model
// traffic through Clawvisor. Surfaced only when the LLM proxy is on, since
// that's the regime where they need a discoverable path.
const PROXY_LITE_AGENT_TABS: AgentTab[] = ['openclaw', 'hermes', 'claude-code', 'codex', 'claude-desktop', 'gbrain', 'cloud-agent', 'other']
const LEGACY_AGENT_TABS: AgentTab[] = ['openclaw', 'claude-code', 'claude-desktop', 'other']

interface AgentMeta {
  label: string
  tagline: string
  // primitive is the install mechanism — shown as a small tag so the user
  // knows up front whether they're running a skill, downloading a config
  // profile, or doing it manually. Sets expectations on effort.
  primitive: 'Skill' | 'Configuration profile' | 'Manual'
}

const AGENT_META: Record<AgentTab, AgentMeta> = {
  'claude-code': {
    label: 'Claude Code',
    tagline: "Anthropic's CLI coding agent",
    primitive: 'Skill',
  },
  codex: {
    label: 'Codex',
    tagline: "OpenAI's CLI coding agent",
    primitive: 'Skill',
  },
  hermes: {
    label: 'Hermes',
    tagline: 'Nous Research general-purpose agent',
    primitive: 'Skill',
  },
  openclaw: {
    label: 'OpenClaw',
    tagline: 'Open-source Claude Code workspace',
    primitive: 'Skill',
  },
  'claude-desktop': {
    label: 'Claude Desktop',
    tagline: 'Anthropic desktop app (macOS)',
    primitive: 'Configuration profile',
  },
  gbrain: {
    label: 'GBrain',
    tagline: 'Personal-brain data pipeline — no LLM proxy, just a token',
    primitive: 'Skill',
  },
  'cloud-agent': {
    label: 'Cloud agent',
    tagline: "Perplexity Computer, hosted ChatGPT — agents you can't reconfigure",
    primitive: 'Manual',
  },
  other: {
    label: 'Other agent',
    tagline: 'Custom HTTP clients, harnesses without skill support',
    primitive: 'Manual',
  },
}

function ConnectAgentGuide({ newToken }: { newToken: string | null }) {
  const [searchParams, setSearchParams] = useSearchParams()
  const { user, features } = useAuth()
  const proxyLiteUI = !!features?.proxy_lite
  const agentTabs = proxyLiteUI ? PROXY_LITE_AGENT_TABS : LEGACY_AGENT_TABS

  // Wizard step is derived from the URL: no param → picker; valid param →
  // install. Invalid params resolve to the picker so a stale deep link
  // doesn't strand the user in a broken state.
  const paramTarget = searchParams.get('agent') as AgentTab | null
  const picked: AgentTab | null = paramTarget && agentTabs.includes(paramTarget) ? paramTarget : null

  const setPicked = (next: AgentTab | null) => {
    const params = new URLSearchParams(searchParams)
    if (next) params.set('agent', next)
    else params.delete('agent')
    // `push` not `replace` so the browser back button rewinds the wizard.
    setSearchParams(params)
  }

  // `?mode=skill` opens the fallback path with its skill escape hatch expanded
  // by default — useful for support / docs deep links.
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
  // Installer-driven wizards (hermes / openclaw) render the pending connection
  // inline at their approve step, so suppress the duplicate below the wizard
  // for those targets.
  const isInstallerWizard = picked === 'hermes' || picked === 'openclaw'
  const pendingForWizard = (connections ?? []).filter(c => c.status === 'pending')
  const showOuterPending = !isInstallerWizard && pendingForWizard.length > 0

  // Mint a single-use claim code so the bootstrap curl never has to embed
  // the user's ID. Codes expire server-side at claimCodeTTL (5 min); refetch
  // every 4 min to keep the visible curl warm.
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

  return (
    <section className="bg-surface-1 border border-border-default rounded-md overflow-hidden">
      <div className="px-5 pt-5 pb-4">
        <h2 className="text-lg font-semibold text-text-primary">Connect an Agent</h2>
        <p className="text-sm text-text-tertiary mt-1">
          {picked
            ? <>Connecting <strong className="text-text-primary">{AGENT_META[picked].label}</strong> to Clawvisor.</>
            : 'Pick the agent you want to connect.'}
        </p>
      </div>

      <div className="p-5 pt-0">
        {picked ? (
          <>
            <button
              onClick={() => {
                // Backing out to the picker is the escape hatch: clear any
                // in-flight installer progress for this target so re-picking
                // the same harness starts fresh at Configure.
                if (picked === 'openclaw' || picked === 'hermes') {
                  clearInstallerProgress(picked)
                }
                setPicked(null)
                setCopied(false)
              }}
              className="text-xs text-text-tertiary hover:text-text-primary mb-4 inline-flex items-center gap-1"
            >
              ← Choose a different agent
            </button>

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
                {picked === 'openclaw' && <LegacyOpenClawGuide setupURL={setupURL} copied={copied} onCopy={copyText} />}
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
        ) : (
          <AgentPickerGrid agents={agentTabs} onPick={setPicked} />
        )}
      </div>
    </section>
  )
}

function AgentPickerGrid({ agents, onPick }: {
  agents: AgentTab[]
  onPick: (next: AgentTab) => void
}) {
  return (
    <div className="grid max-w-3xl grid-cols-1 sm:grid-cols-2 gap-2">
      {agents.map(id => {
        const m = AGENT_META[id]
        return (
          <button
            key={id}
            onClick={() => onPick(id)}
            className="text-left rounded-md border border-border-default bg-surface-0 hover:border-brand hover:bg-surface-1 px-3 py-2.5 transition-colors group"
          >
            <div className="flex items-center justify-between gap-2">
              <span className="text-sm font-medium text-text-primary group-hover:text-brand">{m.label}</span>
              <span className="text-xs text-text-tertiary bg-surface-1 group-hover:bg-surface-0 px-1.5 py-0.5 rounded whitespace-nowrap">
                {m.primitive}
              </span>
            </div>
            <p className="text-xs text-text-tertiary mt-1 leading-snug">{m.tagline}</p>
          </button>
        )
      })}
    </div>
  )
}

function StepNumber({ n }: { n: number }) {
  return (
    <span className="flex-shrink-0 w-6 h-6 rounded-full bg-brand/10 text-brand text-xs font-bold flex items-center justify-center">
      {n}
    </span>
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
  )
}

function LegacyOpenClawGuide({ setupURL, copied, onCopy }: {
  setupURL: string
  copied: boolean
  onCopy: (text: string) => void
}) {
  const taggedSetupURL = withHarness(setupURL, 'openclaw')
  const prompt = `Please install Clawvisor. It's a security gateway between you and external services like Gmail, Slack, and GitHub. You don't hold any API keys directly; instead, you make requests through Clawvisor and I approve which actions you can take. Every call is logged, and I can revoke access at any time.\n\nSetup is just registering an agent token and installing a skill that teaches you how to use it. I'll review each step before it happens.\n\nInstructions: ${taggedSetupURL}`

  return (
    <div className="space-y-5">
      <p className="text-sm text-text-secondary">
        Connect your agent to Clawvisor. Paste the setup prompt below into your agent — it will self-register and wait for your approval.
      </p>

      <div className="space-y-4">
        <div className="flex items-start gap-3">
          <StepNumber n={1} />
          <div className="space-y-1.5 min-w-0 flex-1">
            <p className="text-sm font-medium text-text-primary">Paste this into your agent</p>
            <LegacyPromptBlock prompt={prompt} copied={copied} onCopy={onCopy} />
            <p className="text-xs text-text-tertiary">
              Your agent will follow the setup instructions — registering itself
              and installing the Clawvisor skill.
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

      <div className="bg-surface-0 border border-border-subtle rounded-md px-4 py-3">
        <p className="text-sm text-text-secondary">
          <strong>Using Telegram?</strong> If you talk to your agent via Telegram, you can set up a
          group chat with Clawvisor to get inline approval notifications and auto-approvals.{' '}
          <a href="/dashboard/settings" className="text-brand hover:underline">Set it up in Settings &rarr; Telegram</a>.
        </p>
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
  const taggedSetupURL = withHarness(setupURL, 'other')
  const prompt = `Please install Clawvisor. It's a security gateway between you and external services like Gmail, Slack, and GitHub. You don't hold any API keys directly; instead, you make requests through Clawvisor and I approve which actions you can take. Every call is logged, and I can revoke access at any time.\n\nSetup is just registering an agent token and installing a skill that teaches you how to use it. I'll review each step before it happens.\n\nInstructions: ${taggedSetupURL}`

  return (
    <div className="space-y-5">
      <p className="text-sm text-text-secondary">
        Any agent that can make HTTP requests can connect to Clawvisor. The fastest way is to paste the setup
        prompt below directly into your agent's chat — it will self-register and wait for your approval.
      </p>

      <div className="space-y-4">
        <div className="flex items-start gap-3">
          <StepNumber n={1} />
          <div className="space-y-1.5 min-w-0 flex-1">
            <p className="text-sm font-medium text-text-primary">Paste this into your agent</p>
            <LegacyPromptBlock prompt={prompt} copied={copied} onCopy={onCopy} />
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

// withHarness appends `?harness=<slug>` (or `&harness=…` if the URL already
// has a query string) to a /skill/setup URL. The handler at /skill/setup
// reads the value and bakes it into the example connect curl's JSON body as
// `install_context.harness`, so the agent's self-register POST tags the
// resulting agent record with the harness it came from.
function withHarness(url: string, harness: string): string {
  const sep = url.includes('?') ? '&' : '?'
  return `${url}${sep}harness=${encodeURIComponent(harness)}`
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
  return `mkdir -p ~/.clawvisor/agents && curl -sf --remove-on-error -X POST \\
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
              <div className={`w-5 h-5 rounded-full flex items-center justify-center text-[10px] font-bold border transition-colors ${baseClass}${ringClass}`}>
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
}: {
  canBack: boolean
  canNext: boolean
  onBack: () => void
  onNext: () => void
  onSkip?: () => void
  nextLabel?: string
  skipLabel?: string
  nextDisabledHint?: string
}) {
  return (
    <div className="flex items-center justify-between gap-3 pt-4 mt-4 border-t border-border-subtle">
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
      <div className="flex items-center gap-4">
        {!canNext && nextDisabledHint && (
          <span className="text-xs text-text-tertiary">{nextDisabledHint}</span>
        )}
        {onSkip && (
          <button
            onClick={onSkip}
            className="text-sm text-text-secondary hover:text-text-primary"
          >
            {skipLabel}
          </button>
        )}
        <button
          onClick={onNext}
          disabled={!canNext}
          className="bg-brand text-surface-0 font-medium rounded px-4 py-1.5 text-sm hover:bg-brand-strong disabled:opacity-50 disabled:cursor-not-allowed"
        >
          {nextLabel}
        </button>
      </div>
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
  const [step, setStep] = useState(0)
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
  const taggedSetupURL = withHarness(setupURL, 'other')
  const prompt = `Please install Clawvisor. It's a security gateway between you and external services like Gmail, Slack, and GitHub. You don't hold any API keys directly; instead, you make requests through Clawvisor and I approve which actions you can take. Every call is logged, and I can revoke access at any time.\n\nSetup is just registering an agent token and installing a skill that teaches you how to use it. I'll review each step before it happens.\n\nInstructions: ${taggedSetupURL}`

  const wizardSteps: WizardStepDef[] = [
    { id: 'bootstrap', title: 'Bootstrap agent', done: connected },
    { id: 'key', title: 'Vault upstream key', done: keyReady },
    { id: 'use', title: 'Use it', done: step > 2 },
  ]

  return (
    <div className="space-y-5">
      <p className="text-sm text-text-secondary">
        If the agent lets you change its Anthropic or OpenAI-compatible LLM
        gateway URL, it can use Clawvisor. Clawvisor swaps your{' '}
        <code className="font-mono text-text-secondary">cvis_…</code> token for
        a vaulted upstream key on each call. Three steps — bootstrap, vault, use.
      </p>

      <div className="rounded-md border border-border-default bg-surface-1 px-4 py-5 space-y-4">
        <StepBar steps={wizardSteps} activeIndex={step} />

      {step === 0 && (
        <BootstrapApproveStep
          clawvisorURL={clawvisorURL}
          claim={claim}
          agentName={agentName}
          setAgentName={setAgentName}
          onCopy={onCopy}
          harness="other"
          onAdvance={() => setStep(1)}
        />
      )}

      {step === 1 && myAgent && (
        <>
          <VaultKeyStep agentId={myAgent.id} />
          <WizardNav
            canBack
            canNext={keyReady}
            onBack={() => setStep(0)}
            onNext={() => setStep(2)}
            onSkip={() => setStep(2)}
            skipLabel="Skip — I'll vault one elsewhere"
            nextDisabledHint={keyReady ? undefined : 'Vault at least one provider key to continue'}
          />
        </>
      )}

      {step === 2 && (
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
            canBack
            canNext
            onBack={() => setStep(1)}
            onNext={() => setStep(3)}
            nextLabel="Done"
          />
        </>
      )}

      {step >= 3 && (
        <div className="rounded border border-success/30 bg-success/10 px-4 py-3">
          <p className="text-sm font-medium text-success">All set.</p>
          <button
            onClick={() => setStep(2)}
            className="mt-2 text-xs text-brand hover:underline"
          >
            Show the SDK snippets again
          </button>
        </div>
      )}
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
type LLMProvider = 'anthropic' | 'openai'

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

type LLMCredentialsStatus = { credentials: { provider: string; stored: boolean; agent_stored?: boolean; agent_id?: string }[] }

function providerLabel(provider: LLMProvider): string {
  return provider === 'anthropic' ? 'Anthropic' : 'OpenAI'
}

// VaultKeyStep collects the upstream Anthropic / OpenAI key that the proxy
// swaps in when forwarding requests. With an agentId, it stores an
// agent-scoped override; without one, it stores the user-level key needed
// before swap-mode agents connect.
function VaultKeyStep({
  agentId,
  provider,
  title = 'Vault upstream key',
  description,
}: {
  agentId?: string
  provider?: LLMProvider
  title?: string
  description?: string
}) {
  const qc = useQueryClient()
  const [editingProvider, setEditingProvider] = useState<string | null>(null)
  const [apiKey, setApiKey] = useState('')
  const [error, setError] = useState<string | null>(null)

  const { data: creds } = useQuery({
    queryKey: ['llm-credentials', agentId ?? 'user'],
    queryFn: () => api.llmCredentials.list(agentId),
  })

  const setMut = useMutation({
    mutationFn: (params: { provider: string; key: string }) =>
      api.llmCredentials.set(params.provider, params.key, agentId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['llm-credentials', agentId ?? 'user'] })
      setEditingProvider(null)
      setApiKey('')
      setError(null)
    },
    onError: (err: Error) => setError(err.message),
  })
  const visibleCreds = provider
    ? creds?.credentials.filter(c => c.provider === provider)
    : creds?.credentials

  return (
    <div className="space-y-3">
      <div>
        <p className="text-sm font-medium text-text-primary">{title}</p>
        {description ? (
          <p className="text-sm text-text-secondary mt-1">{description}</p>
        ) : (
          <p className="text-sm text-text-secondary mt-1">
            Clawvisor swaps your <code className="font-mono">cvis_…</code> token for an upstream
            Anthropic or OpenAI key on each call. Vault at least one key — either now
            {agentId ? ' for this agent' : ' as a user-level key'}
            {' '}or globally on the <a href="/dashboard/credentials" className="text-brand hover:underline">Credentials</a> page.
          </p>
        )}
      </div>

      {error && <p className="text-xs text-danger">{error}</p>}

      {visibleCreds?.map(c => (
        <div key={c.provider} className="rounded border border-border-default bg-surface-1 p-3 space-y-2">
          <div className="flex items-center justify-between">
            <div>
              <div className="text-sm font-medium text-text-primary capitalize">{c.provider}</div>
              <div className="text-xs text-text-tertiary mt-0.5">
                {c.agent_stored ? (
                  <span className="text-success">Agent-scoped key set</span>
                ) : c.stored ? (
                  <span className="text-success">Using user-level key</span>
                ) : (
                  <span className="text-warning">No key configured</span>
                )}
              </div>
            </div>
            <button
              onClick={() => { setEditingProvider(c.provider); setApiKey(''); setError(null) }}
              className="text-xs px-3 py-1 rounded border border-brand/30 text-brand hover:bg-brand/10"
            >
              {c.agent_stored ? 'Replace' : c.stored ? (agentId ? 'Override for this agent' : 'Replace') : 'Set key'}
            </button>
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
                  onClick={() => { if (!apiKey) { setError('API key is required'); return } setMut.mutate({ provider: c.provider, key: apiKey }) }}
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
  )
}

// Whether the upstream-key step is satisfied: at least one provider has a key
// available, whether scoped to this agent or inherited from the user.
function hasAnyUpstreamKey(creds: LLMCredentialsStatus | undefined): boolean {
  if (!creds) return false
  return creds.credentials.some(c => c.stored || c.agent_stored)
}

function hasProviderUpstreamKey(creds: LLMCredentialsStatus | undefined, provider: LLMProvider): boolean {
  if (!creds) return false
  return creds.credentials.some(c => c.provider === provider && (c.stored || c.agent_stored))
}

function hasProviderAgentKey(creds: LLMCredentialsStatus | undefined, provider: LLMProvider): boolean {
  if (!creds) return false
  return creds.credentials.some(c => c.provider === provider && c.agent_stored)
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
              harness={target}
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
type InstallerHelper = 'claude' | 'codex'

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

const INSTALLER_HELPERS: Record<InstallerHelper, {
  // shortName is the agent's product name on its own (used in body copy);
  // pillLabel is what shows on the helper-toggle in the Run step;
  // skillPath/invokeCommand are spliced into the bootstrap script.
  shortName: string
  pillLabel: string
  skillPath: string
  invokeCommand: string
}> = {
  claude: {
    shortName: 'Claude Code',
    pillLabel: 'Claude Code skill',
    skillPath: '~/.claude/commands/clawvisor-install.md',
    invokeCommand: 'claude /clawvisor-install',
  },
  codex: {
    shortName: 'Codex',
    pillLabel: 'Codex skill',
    skillPath: '~/.codex/skills/clawvisor-install/SKILL.md',
    invokeCommand: "codex '$clawvisor-install'",
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
    `mkdir -p ~/.clawvisor/agents && curl -sf --remove-on-error -X POST \\`,
    `  "${connectURL}" \\`,
    `  -o ~/.clawvisor/agents/${name}.json \\`,
    `  && chmod 600 ~/.clawvisor/agents/${name}.json`,
  ].join('\n')
}

function buildHelperCommand(opts: {
  skillURL: string
  helper: InstallerHelper
}): string {
  const helperSpec = INSTALLER_HELPERS[opts.helper]
  return [
    `curl -sf "${opts.skillURL}" \\`,
    `  --create-dirs -o ${helperSpec.skillPath} \\`,
    `  && ${helperSpec.invokeCommand}`,
  ].join('\n')
}

interface InstallerAnswers {
  hermesConfig: 'env' | 'file'
  hermesMode: 'host' | 'docker' | 'remote'
  openclawMode: 'host' | 'docker' | 'remote'
  llmProvider: LLMProvider
}

type InstallerCredentialScope = 'user' | 'agent'

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

  const { data: userCreds } = useQuery({
    queryKey: ['llm-credentials', 'user'],
    queryFn: () => api.llmCredentials.list(),
  })
  const { data: agentCreds } = useQuery({
    queryKey: ['llm-credentials', candidateAgent?.id ?? 'pending-agent'],
    queryFn: () => api.llmCredentials.list(candidateAgent!.id),
    enabled: credentialScope === 'agent' && !!candidateAgent?.id,
  })
  const userKeyReady = hasProviderUpstreamKey(userCreds, answers.llmProvider)
  const agentKeyReady = hasProviderAgentKey(agentCreds, answers.llmProvider)
  // `apiKeyReady` gates the post-approval API key screen. We now write the
  // key directly against the (already-minted) agent_id rather than holding a
  // user-typed string in browser memory until approval, so the readiness
  // check is straightforward — no separate "staged but not yet applied"
  // bookkeeping.
  const apiKeyReady = credentialScope === 'user' ? userKeyReady : agentKeyReady

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
  const helperSpec = INSTALLER_HELPERS[helper]
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
            <div>
              <p className="text-sm font-medium text-text-primary">Set the {providerLabel(answers.llmProvider)} key for {screen.agent.name}</p>
              <p className="text-xs text-text-tertiary mt-1 leading-relaxed">
                Clawvisor swaps your <code className="font-mono">cvis_…</code> token for an upstream
                {' '}{providerLabel(answers.llmProvider)} key on each call. Pick a scope and save the key
                — it's written directly against this agent, not held in the browser.
              </p>
            </div>
            <QuestionToggleGroup
              label="Which API key should Clawvisor use for this agent?"
              value={credentialScope}
              onChange={value => setCredentialScope(value as InstallerCredentialScope)}
              options={[
                ['user', `Use user-level ${providerLabel(answers.llmProvider)} key`],
                ['agent', `Set agent-specific ${providerLabel(answers.llmProvider)} key`],
              ]}
            />
            {credentialScope === 'user' ? (
              <VaultKeyStep
                provider={answers.llmProvider}
                title={`Use user-level ${providerLabel(answers.llmProvider)} key`}
                description={`Clawvisor will use your user-level ${providerLabel(answers.llmProvider)} key for ${screen.agent.name}. You can override with an agent-specific key later from the agent settings page.`}
              />
            ) : (
              <VaultKeyStep
                agentId={screen.agent.id}
                provider={answers.llmProvider}
                title={`Set agent-specific ${providerLabel(answers.llmProvider)} key`}
                description={`Saved directly to ${screen.agent.name}. Only this agent uses this key — other agents fall through to your user-level vault.`}
              />
            )}
            <WizardNav
              canBack={false}
              canNext={apiKeyReady}
              onBack={() => {}}
              onNext={() => setApiKeyAcknowledged(true)}
              nextLabel="Continue"
              nextDisabledHint={apiKeyReady
                ? undefined
                : credentialScope === 'agent'
                  ? `Save an agent-specific ${providerLabel(answers.llmProvider)} key above to continue`
                  : `Add a user-level ${providerLabel(answers.llmProvider)} key above to continue`}
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
          <div className="mt-5 space-y-4">
            <div>
              <p className="text-sm font-medium text-text-primary">Install and run the helper</p>
              <p className="text-xs text-text-tertiary mt-1 leading-relaxed">
                Paste this into your terminal. {helperSpec.shortName} reads the token from disk and configures {spec.label}.
              </p>
            </div>
            <div>
              <p className="text-xs text-text-tertiary mb-1.5">Which helper agent is running this?</p>
              <QuestionToggleGroup
                label=""
                value={helper}
                onChange={value => setHelper(value as InstallerHelper)}
                options={(Object.keys(INSTALLER_HELPERS) as InstallerHelper[]).map(h => [h, INSTALLER_HELPERS[h].pillLabel])}
              />
            </div>
            <CodeBlock onCopy={() => onCopy(helperCommand)}>{helperCommand}</CodeBlock>
            <AgentStartStatus
              liveSession={liveSession}
              startActivity={startActivity}
              waitingText={candidateAgent
                ? `Watching ${candidateAgent.name} for the helper's smoke test.`
                : 'Waiting for the helper to make its first Clawvisor-routed call.'}
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
}: {
  target: InstallerTarget
  answers: InstallerAnswers
  onChange: (answers: InstallerAnswers) => void
}) {
  const targetLabel = INSTALLER_SPECS[target].label
  const set = <K extends keyof InstallerAnswers>(key: K, value: InstallerAnswers[K]) => {
    onChange({ ...answers, [key]: value })
  }
  return (
    <div className="space-y-4">
      <div>
        <div>
          <p className="text-sm font-medium text-text-primary">Answer setup questions</p>
          <p className="text-xs text-text-tertiary mt-1">
            These answers are baked into the installer skill URL, so the helper
            follows your preferences instead of asking again.
          </p>
        </div>
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

function QuestionToggleGroup({
  label,
  value,
  options,
  onChange,
}: {
  label: string
  value: string
  options: Array<[string, string]>
  onChange: (value: string) => void
}) {
  return (
    <div>
      {label && <p className="text-xs font-medium text-text-primary">{label}</p>}
      <div
        role="group"
        aria-label={label || undefined}
        className={`${label ? 'mt-1 ' : ''}inline-flex max-w-full flex-wrap rounded-md border border-border-default bg-surface-0 p-1`}
      >
        {options.map(([optionValue, optionLabel]) => (
          <button
            key={optionValue}
            type="button"
            onClick={() => onChange(optionValue)}
            aria-pressed={value === optionValue}
            className={`rounded px-3 py-1.5 text-sm font-medium leading-snug transition ${
              value === optionValue
                ? 'bg-surface-1 text-text-primary shadow-sm'
                : 'text-text-tertiary hover:text-text-primary'
            }`}
          >
            {optionLabel}
          </button>
        ))}
      </div>
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
  // Tag the setup URL with the harness slug the dashboard knows the user
  // picked. The /skill/setup handler reads `?harness=` and splices it into
  // the example connect curl as `install_context.harness`, so the agent's
  // self-register call stamps the resulting agent record with the harness
  // and the agent-detail "Connect this agent" panel renders the right snippet.
  const taggedSetupURL = withHarness(setupURL, 'cloud-agent')
  const prompt = `Please install Clawvisor. It's a security gateway between you and external services like Gmail, Slack, and GitHub. You don't hold any API keys directly; instead, you make requests through Clawvisor and I approve which actions you can take. Every call is logged, and I can revoke access at any time.\n\nSetup is just registering an agent token and installing a skill that teaches you how to use it. I'll review each step before it happens.\n\nInstructions: ${taggedSetupURL}`

  return (
    <div className="space-y-5">
      <p className="text-sm text-text-secondary">
        For hosted agents (Perplexity Computer, hosted ChatGPT, any harness
        where you can't change the LLM endpoint). Paste the prompt below into
        your agent — it will fetch the setup instructions, register itself,
        and wait for your approval. Your LLM traffic stays with the vendor;
        Clawvisor only intermediates external service calls.
      </p>

      <div className="space-y-4">
        <div className="flex items-start gap-3">
          <StepNumber n={1} />
          <div className="space-y-1.5 min-w-0 flex-1">
            <p className="text-sm font-medium text-text-primary">Paste this into your agent</p>
            <LegacyPromptBlock prompt={prompt} copied={copied} onCopy={onCopy} />
            <p className="text-xs text-text-tertiary">
              The agent follows the setup instructions at that URL — registers
              itself, sets up E2E encryption, and installs the Clawvisor skill.
            </p>
          </div>
        </div>

        <div className="flex items-start gap-3">
          <StepNumber n={2} />
          <div className="space-y-1.5 min-w-0 flex-1">
            <p className="text-sm font-medium text-text-primary">Approve the connection</p>
            <p className="text-xs text-text-tertiary">
              A connection request will appear in the <strong>Pending Connections</strong>{' '}
              section below. Click <strong>Approve</strong> to grant the agent a token. It
              receives the token automatically and is ready to go.
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
      return api.agents.create(name, GBRAIN_AGENT_DESCRIPTION, { harness: 'gbrain' })
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

function ConnectionCard({ request: cr }: { request: ConnectionRequest }) {
  const qc = useQueryClient()
  const [result, setResult] = useState<string | null>(null)

  const approveMut = useMutation({
    mutationFn: () => api.connections.approve(cr.id),
    onSuccess: () => {
      setResult('Approved')
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
          <div className="text-sm font-medium text-text-primary">LLM credentials</div>
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

// ── Connect-this-agent panel ────────────────────────────────────────────────
//
// Surfaces the commands an agent harness needs to point at this daemon's
// lite-proxy. Snippets mirror the Connect-an-Agent flow, including the
// token-file lookup written by the bootstrap step.
function AgentLiteProxyPanel({ agent }: { agent: Agent }) {
  const [open, setOpen] = useState(false)
  const [skipPermissions, setSkipPermissions] = useState(false)
  const harness = agent.install_context?.harness
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
  const tokenPath = `~/.clawvisor/agents/${agent.name}.json`
  const copySnippet = (value: string) => {
    if (navigator.clipboard && typeof navigator.clipboard.writeText === 'function') {
      void navigator.clipboard.writeText(value)
    }
  }

  const claudeSkipFlag = '--dangerously-skip-permissions'
  const codexSkipFlag = '--dangerously-bypass-approvals-and-sandbox'
  const isCodexHarness = harness === 'codex'
  const skipPermsFlag = isCodexHarness ? codexSkipFlag : claudeSkipFlag
  const skipPermsClaude = skipPermissions ? ` ${claudeSkipFlag}` : ''
  const skipPermsCodex = skipPermissions ? ` ${codexSkipFlag}` : ''
  const claudeStartCommand = `ANTHROPIC_BASE_URL=${baseURL}/api \\
ANTHROPIC_CUSTOM_HEADERS="X-Clawvisor-Agent-Token: $(jq -r .token ${tokenPath})" \\
ANTHROPIC_AUTH_TOKEN= ANTHROPIC_API_KEY= \\
claude${skipPermsClaude}`
  const claudeAliasCommand = `cat >> ~/.zshrc <<'EOF'
claude-cv() {
  ANTHROPIC_BASE_URL=${baseURL}/api \\
  ANTHROPIC_CUSTOM_HEADERS="X-Clawvisor-Agent-Token: $(jq -r .token ${tokenPath})" \\
  ANTHROPIC_AUTH_TOKEN= ANTHROPIC_API_KEY= \\
  claude${skipPermsClaude} "$@"
}
EOF`
  const codexConfigureCommand = `mkdir -p ~/.codex
grep -q '^\\[model_providers\\.clawvisor\\]' ~/.codex/config.toml 2>/dev/null || cat >> ~/.codex/config.toml <<'EOF'

[model_providers.clawvisor]
name = "Clawvisor"
base_url = "${baseURL}/api/v1"
wire_api = "responses"
requires_openai_auth = true

[model_providers.clawvisor.env_http_headers]
X-Clawvisor-Agent-Token = "CLAWVISOR_AGENT_TOKEN"
EOF`
  const codexStartCommand = `CLAWVISOR_AGENT_TOKEN=$(jq -r .token ${tokenPath}) codex${skipPermsCodex} -c model_provider=clawvisor`
  const codexAliasCommand = `cat >> ~/.zshrc <<'EOF'
codex-cv() {
  CLAWVISOR_AGENT_TOKEN=$(jq -r .token ${tokenPath}) codex${skipPermsCodex} -c model_provider=clawvisor "$@"
}
EOF`
  const anthropicSDK = `import anthropic, json, os
data = json.load(open(os.path.expanduser("${tokenPath}")))
client = anthropic.Anthropic(
    base_url="${baseURL}/api",
    api_key=data["token"],
)`
  const openaiSDK = `from openai import OpenAI
import json, os
data = json.load(open(os.path.expanduser("${tokenPath}")))
client = OpenAI(
    base_url="${baseURL}/api/v1",
    api_key=data["token"],
)`
  const clawvisorEnv = `export CLAWVISOR_URL="${baseURL}"
export CLAWVISOR_AGENT_TOKEN="$(jq -r .token ${tokenPath})"`

  type ConnectionSnippet = {
    label: string
    body: string
    description?: string
  }
  let snippets: ConnectionSnippet[]
  if (harness === 'codex') {
    snippets = [
      {
        label: 'Configure Codex',
        description: `Uses ${tokenPath} for the agent token header.`,
        body: codexConfigureCommand,
      },
      {
        label: 'Start a session',
        body: codexStartCommand,
      },
      {
        label: 'Create an alias',
        description: 'Add this to zsh. Use the same shape in bash/fish if needed.',
        body: codexAliasCommand,
      },
    ]
  } else if (harness === 'claude-code' || harness === 'openclaw' || harness === 'hermes') {
    snippets = [
      {
        label: 'Start a session',
        body: claudeStartCommand,
      },
      {
        label: 'Create an alias',
        description: 'Add this to zsh. Use the same shape in bash/fish if needed.',
        body: claudeAliasCommand,
      },
    ]
  } else if (harness === 'gbrain' || harness === 'cloud-agent') {
    snippets = [
      {
        label: 'Environment variables',
        description: `Reads the token from ${tokenPath}.`,
        body: clawvisorEnv,
      },
    ]
  } else {
    snippets = [
      {
        label: 'Anthropic SDK (Python)',
        body: anthropicSDK,
      },
      {
        label: 'OpenAI SDK (Python)',
        body: openaiSDK,
      },
    ]
  }

  // Claude Desktop installs via configuration profile — there's no shell
  // snippet that re-creates the install. Direct the user back to the
  // wizard's profile download instead.
  const isClaudeDesktop = harness === 'claude-desktop'

  return (
    <div className="mt-3 rounded border border-border-subtle bg-surface-0">
      <button
        onClick={() => setOpen(v => !v)}
        className="flex w-full items-center justify-between px-4 py-3 text-left"
      >
        <div>
          <div className="text-sm font-medium text-text-primary">Connect this agent</div>
          <div className="text-xs text-text-tertiary">
            How to point this harness at Clawvisor. Clawvisor authenticates the agent and either preserves upstream auth or swaps in a vaulted provider key.
          </div>
        </div>
        <span className="text-xs text-text-tertiary">{open ? 'Hide' : 'Show'}</span>
      </button>
      {open && (
        <div className="border-t border-border-subtle px-4 py-4 space-y-4">
          {isClaudeDesktop ? (
            <div className="text-sm text-text-secondary">
              Claude Desktop is installed via a macOS configuration profile.
              Re-download the profile from the{' '}
              <Link to="/dashboard/agents?agent=claude-desktop" className="text-brand hover:underline">
                Connect an Agent
              </Link>{' '}
              wizard — each download mints a fresh agent.
            </div>
          ) : (
            <div className="space-y-4">
              {(harness === 'codex' || harness === 'claude-code' || harness === 'openclaw' || harness === 'hermes') && (
                <SkipPermissionsCheckbox
                  checked={skipPermissions}
                  onChange={setSkipPermissions}
                  flag={skipPermsFlag}
                  label={harness === 'codex' ? 'Codex' : 'Claude Code'}
                />
              )}
              <p className="text-xs text-text-tertiary">
                Commands expect the token file at <code className="font-mono text-text-secondary">{tokenPath}</code>.
              </p>
              {snippets.map(snippet => (
                <div key={snippet.label} className="space-y-1.5">
                  <div>
                    <div className="text-xs uppercase tracking-wider text-text-tertiary">{snippet.label}</div>
                    {snippet.description && (
                      <p className="mt-1 text-xs text-text-tertiary">{snippet.description}</p>
                    )}
                  </div>
                  <CodeBlock onCopy={() => copySnippet(snippet.body)}>{snippet.body}</CodeBlock>
                </div>
              ))}
            </div>
          )}
        </div>
      )}
    </div>
  )
}
