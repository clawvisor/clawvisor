import { useEffect, useMemo, useState } from 'react'
import { Link, useNavigate, useParams, useSearchParams } from 'react-router-dom'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api, type Agent, type ApprovalRecord, type AgentRuntimeSettings, type AuditEntry, type RuntimePolicyRule, type RuntimeSession } from '../api/client'
import type { ConnectionRequest } from '../api/client'
import { useAuth } from '../hooks/useAuth'
import { formatDistanceToNow } from 'date-fns'
import CountdownTimer from '../components/CountdownTimer'
import { RuntimeApprovalsPanel, RuntimeSessionsPanel, filterLiveRuntimeApprovals, isActiveRuntimeSession } from './Runtime'

export default function Agents() {
  const { currentOrg, features } = useAuth()
  const { agentId } = useParams()
  const navigate = useNavigate()
  const orgId = currentOrg?.id
  const qc = useQueryClient()
  const liveSessionsUI = !orgId && !!features?.agent_live_sessions
  const runtimePolicyUI = !orgId && !!features?.runtime_policy_ui
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
  const { data: runtimeSessions } = useQuery({
    queryKey: ['runtime-sessions'],
    queryFn: () => api.runtime.listSessions(),
    enabled: liveSessionsUI,
    refetchInterval: 15_000,
  })
  const { data: runtimeApprovals } = useQuery({
    queryKey: ['runtime-approvals'],
    queryFn: () => api.runtime.listApprovals(),
    enabled: liveSessionsUI,
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
        liveSessionsUI={liveSessionsUI}
        runtimePolicyUI={runtimePolicyUI}
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
      {!orgId && <ConnectAgentGuide />}

      {/* Pending connection requests (personal context only) */}
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
            const liveSessions = liveSessionsUI ? (sessionsByAgent.get(agent.id) ?? []) : []
            const pendingApprovals = liveSessionsUI ? (approvalsByAgent.get(agent.id) ?? []) : []
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
                    {liveSessionsUI && liveSessions.length > 0 && (
                      <span className="text-xs font-medium px-2 py-0.5 rounded-full bg-success/10 text-success">
                        {liveSessions.length} live session{liveSessions.length === 1 ? '' : 's'}
                      </span>
                    )}
                    {liveSessionsUI && pendingApprovals.length > 0 && (
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
  onDeleted,
}: {
  agent: Agent
  orgId?: string
  sessions: RuntimeSession[]
  approvals: ApprovalRecord[]
  liveSessionsUI: boolean
  runtimePolicyUI: boolean
  onDeleted: () => void
}) {
  const qc = useQueryClient()
  const { data: allRuntimeSessions } = useQuery({
    queryKey: ['runtime-sessions'],
    queryFn: () => api.runtime.listSessions(),
    enabled: liveSessionsUI,
    refetchInterval: 15_000,
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

      <div className={`grid gap-3 ${liveSessionsUI ? 'md:grid-cols-3' : 'md:grid-cols-1'}`}>
        {liveSessionsUI && <AgentMetric label="Live sessions" value={String(sessions.length)} />}
        {liveSessionsUI && <AgentMetric label="Pending approvals" value={String(approvals.length)} />}
        <AgentMetric label="Active tasks" value={String(agent.active_task_count)} />
      </div>

      <div className="flex flex-wrap gap-3">
        <Link to={`/dashboard/activity?agent_id=${encodeURIComponent(agent.id)}`} className="rounded border border-border-default px-4 py-2 text-sm text-text-secondary hover:bg-surface-2">
          Open Activity for Agent
        </Link>
        <Link to="/dashboard/policy" className="rounded border border-border-default px-4 py-2 text-sm text-text-secondary hover:bg-surface-2">
          Open Policy
        </Link>
      </div>

      {runtimePolicyUI && <AgentRuntimePanel agentId={agent.id} defaultOpen />}

      {runtimePolicyUI && (
        <AgentPolicyPanel
          agent={agent}
          rules={agentRules}
          recentActivity={recentActivity?.entries ?? []}
        />
      )}

      {liveSessionsUI && (
        <RecentSessionsPanel sessions={recentSessions} />
      )}

      {liveSessionsUI && (
        <RuntimeSessionsPanel
          sessions={sessions}
          agents={agentMap}
          onUpdated={() => {
            qc.invalidateQueries({ queryKey: ['runtime-sessions'] })
            qc.invalidateQueries({ queryKey: ['overview'] })
          }}
        />
      )}

      {liveSessionsUI && (
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

function AgentRuntimePanel({ agentId, defaultOpen = false }: { agentId: string; defaultOpen?: boolean }) {
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

  return (
    <div className="mt-3 rounded border border-border-subtle bg-surface-0">
      <button
        onClick={() => {
          setOpen(v => !v)
          if (!open && settings && !draft) setDraft(settings)
        }}
        className="flex w-full items-center justify-between px-4 py-3 text-left"
      >
        <div>
          <div className="text-sm font-medium text-text-primary">Runtime settings</div>
          <div className="text-xs text-text-tertiary">
            {current
              ? `${current.runtime_enabled ? 'enabled' : 'disabled'} · ${current.runtime_mode} · ${current.starter_profile || 'none'}`
              : 'Configure observe vs enforce defaults, starter profile, and outbound credential posture.'}
          </div>
        </div>
        <span className="text-xs text-text-tertiary">{open ? 'Hide' : 'Edit'}</span>
      </button>
      {open && current && (
        <div className="border-t border-border-subtle px-4 py-4 space-y-3">
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

// ── Connect an Agent guide ───────────────────────────────────────────────────

type AgentTab = 'openclaw' | 'claude-code' | 'claude-desktop' | 'other'

const AGENT_TABS: AgentTab[] = ['openclaw', 'claude-code', 'claude-desktop', 'other']

function ConnectAgentGuide() {
  const [searchParams, setSearchParams] = useSearchParams()
  const initialTab = (AGENT_TABS.includes(searchParams.get('agent') as AgentTab)
    ? (searchParams.get('agent') as AgentTab)
    : 'openclaw')
  const [tab, setTabState] = useState<AgentTab>(initialTab)
  const setTab = (next: AgentTab) => {
    setTabState(next)
    const params = new URLSearchParams(searchParams)
    params.set('agent', next)
    setSearchParams(params, { replace: true })
  }
  const [copied, setCopied] = useState(false)
  const { user } = useAuth()

  const { data: pairInfo } = useQuery({
    queryKey: ['pairInfo'],
    queryFn: () => api.devices.pairInfo(),
  })

  const isLocal = window.location.hostname === 'localhost' || window.location.hostname === '127.0.0.1'
  const hasRelay = !!(pairInfo?.daemon_id && pairInfo?.relay_host)

  // When accessed locally, agents should talk to the daemon directly rather
  // than routing through the relay. Use the relay URL only when the dashboard
  // itself is being accessed remotely (cloud-hosted).
  const clawvisorURL = isLocal
    ? window.location.origin
    : hasRelay
      ? `https://${pairInfo!.relay_host}/d/${pairInfo!.daemon_id}`
      : window.location.origin

  const userIdParam = user?.id ? `?user_id=${encodeURIComponent(user.id)}` : ''

  const setupURL = hasRelay
    ? `https://${pairInfo!.relay_host}/d/${pairInfo!.daemon_id}/skill/setup${userIdParam}`
    : `${window.location.origin}/skill/setup${userIdParam}`

  const copyText = (text: string) => {
    navigator.clipboard.writeText(text)
    setCopied(true)
    setTimeout(() => setCopied(false), 2000)
  }

  const tabs: { id: AgentTab; label: string }[] = [
    { id: 'openclaw', label: 'OpenClaw / Hermes' },
    { id: 'claude-code', label: 'Claude Code' },
    { id: 'claude-desktop', label: 'Claude Desktop' },
    { id: 'other', label: 'Other Agents' },
  ]

  return (
    <section className="bg-surface-1 border border-border-default rounded-md overflow-hidden">
      <div className="px-5 pt-5 pb-0">
        <h2 className="text-lg font-semibold text-text-primary">Connect an Agent</h2>
        <p className="text-sm text-text-tertiary mt-1">
          Follow the steps below to connect a coding agent to Clawvisor.
        </p>
      </div>

      {/* Tabs */}
      <div className="flex gap-0 px-5 mt-4 border-b border-border-subtle overflow-x-auto">
        {tabs.map(t => (
          <button
            key={t.id}
            onClick={() => { setTab(t.id); setCopied(false) }}
            className={`px-4 py-2.5 text-sm font-medium border-b-2 transition-colors ${
              tab === t.id
                ? 'border-brand text-brand'
                : 'border-transparent text-text-tertiary hover:text-text-secondary'
            }`}
          >
            {t.label}
          </button>
        ))}
      </div>

      <div className="p-5">
        {tab === 'openclaw' && <OpenClawGuide setupURL={setupURL} copied={copied} onCopy={copyText} />}
        {tab === 'claude-code' && <ClaudeCodeGuide clawvisorURL={clawvisorURL} userIdParam={userIdParam} onCopy={copyText} />}
        {tab === 'claude-desktop' && <ClaudeDesktopGuide isLocal={isLocal} onCopy={copyText} />}
        {tab === 'other' && <OtherAgentGuide setupURL={setupURL} clawvisorURL={clawvisorURL} copied={copied} onCopy={copyText} />}
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

function CodeBlock({ children, onCopy }: { children: string; onCopy?: () => void }) {
  return (
    <div className="relative group bg-surface-0 border border-border-subtle rounded overflow-hidden">
      <pre className="px-3 py-2.5 text-xs font-mono text-text-primary overflow-x-auto whitespace-pre-wrap break-all">
        {children}
      </pre>
      {onCopy && (
        <>
          {/* Desktop: inline overlay */}
          <button
            onClick={onCopy}
            className="hidden sm:block absolute top-2 right-2 text-xs px-2 py-1 rounded border border-border-subtle text-text-tertiary hover:text-text-primary hover:bg-surface-1 opacity-0 group-hover:opacity-100 transition-opacity"
          >
            Copy
          </button>
          {/* Mobile: footer bar */}
          <div className="sm:hidden border-t border-border-subtle px-3 py-1.5 flex justify-end">
            <button
              onClick={onCopy}
              className="text-xs px-2.5 py-1 rounded border border-border-subtle text-text-tertiary hover:text-text-primary hover:bg-surface-1"
            >
              Copy
            </button>
          </div>
        </>
      )}
    </div>
  )
}

function ClaudeCodeGuide({ clawvisorURL, userIdParam, onCopy }: {
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

function ClaudeDesktopGuide({ isLocal, onCopy }: { isLocal: boolean; onCopy: (text: string) => void }) {
  const pluginName = isLocal ? 'clawvisor-local@cowork-plugins' : 'clawvisor@cowork-plugins'
  const marketplaceCmd = 'claude plugin marketplace add clawvisor/cowork-plugins'
  const installCmd = `/plugin install ${pluginName}`

  return (
    <div className="space-y-5">
      <p className="text-sm text-text-secondary">
        {isLocal
          ? 'Connect Claude Desktop to your local Clawvisor instance via the Cowork plugin.'
          : 'Connect Claude Desktop to your Clawvisor cloud account via the Cowork plugin.'}
      </p>

      <div className="flex items-start gap-3">
        <StepNumber n={1} />
        <div className="space-y-1.5 min-w-0 flex-1">
          <p className="text-sm font-medium text-text-primary">Add the marketplace</p>
          <p className="text-xs text-text-tertiary">
            Run this in your terminal:
          </p>
          <CodeBlock onCopy={() => onCopy(marketplaceCmd)}>{marketplaceCmd}</CodeBlock>
        </div>
      </div>

      <div className="flex items-start gap-3">
        <StepNumber n={2} />
        <div className="space-y-1.5 min-w-0 flex-1">
          <p className="text-sm font-medium text-text-primary">Install the plugin</p>
          <p className="text-xs text-text-tertiary">
            From within Claude Desktop:
          </p>
          <CodeBlock onCopy={() => onCopy(installCmd)}>{installCmd}</CodeBlock>
        </div>
      </div>

      {!isLocal && (
        <div className="flex items-start gap-3">
          <StepNumber n={3} />
          <div className="space-y-1.5 min-w-0 flex-1">
            <p className="text-sm font-medium text-text-primary">Authenticate</p>
            <p className="text-xs text-text-tertiary">
              The first time Claude uses a Clawvisor tool, you'll be prompted to authenticate via OAuth.
              Follow the link in your terminal to sign in and connect Claude Desktop to your Clawvisor cloud account.
            </p>
          </div>
        </div>
      )}

      <div className="flex items-start gap-3">
        <StepNumber n={isLocal ? 3 : 4} />
        <div className="space-y-1.5 min-w-0 flex-1">
          <p className="text-sm font-medium text-text-primary">Start using it</p>
          <p className="text-xs text-text-tertiary">
            Ask Claude to do something that requires an external service — e.g. "check my Gmail" or
            "list my GitHub issues." Claude will create a task, ask you to approve, and execute through
            Clawvisor.{' '}
            {isLocal &&
              <>Open the dashboard with <code className="font-mono text-text-secondary">clawvisor tui</code> or visit <code className="font-mono text-text-secondary">http://localhost:25297</code> to manage services, approvals, and restrictions.</>
            }
          </p>
        </div>
      </div>
    </div>
  )
}

function OpenClawGuide({ setupURL, copied, onCopy }: {
  setupURL: string
  copied: boolean
  onCopy: (text: string) => void
}) {
  const prompt = `I'd like to set up Clawvisor as the trusted gateway for using data and services. Please follow the instructions at:\n${setupURL}`

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
            <div className="relative group bg-surface-0 border border-brand/20 rounded overflow-hidden">
              <pre className="px-3 py-2.5 text-xs font-mono text-text-primary overflow-x-auto whitespace-pre-wrap break-all">
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

      {/* Telegram tip */}
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

function OtherAgentGuide({ setupURL, clawvisorURL, copied, onCopy }: {
  setupURL: string
  clawvisorURL: string
  copied: boolean
  onCopy: (text: string) => void
}) {
  const prompt = `I'd like to set up Clawvisor as the trusted gateway for using data and services. Please follow the instructions at:\n${setupURL}`

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
            <div className="relative group bg-surface-0 border border-brand/20 rounded overflow-hidden">
              <pre className="px-3 py-2.5 text-xs font-mono text-text-primary overflow-x-auto whitespace-pre-wrap break-all">
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

      {/* Manual path */}
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
              <CodeBlock>{`curl -sf -H "Authorization: Bearer $CLAWVISOR_AGENT_TOKEN" \\\n  "$CLAWVISOR_URL/api/skill/catalog" | head -20`}</CodeBlock>
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

// ── Connection request card ──────────────────────────────────────────────────

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
    },
    onError: (err: Error) => setResult(`Failed: ${err.message}`),
  })

  const denyMut = useMutation({
    mutationFn: () => api.connections.deny(cr.id),
    onSuccess: () => {
      setResult('Denied')
      qc.invalidateQueries({ queryKey: ['connections'] })
      qc.invalidateQueries({ queryKey: ['overview'] })
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
