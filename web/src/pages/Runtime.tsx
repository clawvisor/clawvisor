import { useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, type Agent, type ApprovalRecord, type RuntimeEvent, type RuntimePolicyRule, type RuntimeStatus, type RuntimeSession, type StarterProfile } from '../api/client'

export type RuleDraft = Partial<RuntimePolicyRule> & { scope?: 'agent' | 'global' }

export function isActiveRuntimeSession(session: RuntimeSession): boolean {
  if (session.revoked_at) return false
  return new Date(session.expires_at).getTime() > Date.now()
}

export function filterLiveRuntimeApprovals(approvals: ApprovalRecord[], sessions: RuntimeSession[]): ApprovalRecord[] {
  const activeSessionIds = new Set(sessions.filter(isActiveRuntimeSession).map(session => session.id))
  return approvals.filter(approval => {
    if (!approval.session_id) return true
    return activeSessionIds.has(approval.session_id)
  })
}

export const emptyEgressRule = (): RuleDraft => ({
  scope: 'agent',
  kind: 'egress',
  action: 'allow',
  host: '',
  method: 'GET',
  path: '',
  path_regex: '',
  reason: '',
  enabled: true,
  source: 'user',
})

export const emptyToolRule = (): RuleDraft => ({
  scope: 'agent',
  kind: 'tool',
  action: 'allow',
  tool_name: '',
  input_regex: '',
  reason: '',
  enabled: true,
  source: 'user',
})

export default function Runtime() {
  const qc = useQueryClient()
  const [agentFilter, setAgentFilter] = useState<string>('all')
  const [editingRule, setEditingRule] = useState<RuleDraft | null>(null)

  const { data: agents = [] } = useQuery({
    queryKey: ['agents'],
    queryFn: () => api.agents.list(),
  })
  const { data: status } = useQuery({
    queryKey: ['runtime-status'],
    queryFn: () => api.runtime.status(),
  })
  const { data: sessions } = useQuery({
    queryKey: ['runtime-sessions'],
    queryFn: () => api.runtime.listSessions(),
    refetchInterval: 15_000,
  })
  const { data: approvals } = useQuery({
    queryKey: ['runtime-approvals'],
    queryFn: () => api.runtime.listApprovals(),
    refetchInterval: 10_000,
  })
  const { data: events } = useQuery({
    queryKey: ['runtime-events'],
    queryFn: () => api.runtime.listEvents(),
    refetchInterval: 10_000,
  })
  const { data: egressRules } = useQuery({
    queryKey: ['runtime-rules', 'egress', agentFilter],
    queryFn: () => api.runtime.listRules({ kind: 'egress', agent_id: agentFilter === 'all' ? undefined : agentFilter }),
  })
  const { data: toolRules } = useQuery({
    queryKey: ['runtime-rules', 'tool', agentFilter],
    queryFn: () => api.runtime.listRules({ kind: 'tool', agent_id: agentFilter === 'all' ? undefined : agentFilter }),
  })
  const { data: starterProfiles } = useQuery({
    queryKey: ['runtime-starter-profiles'],
    queryFn: () => api.runtime.listStarterProfiles(),
  })

  const agentMap = useMemo(() => new Map(agents.map(agent => [agent.id, agent])), [agents])
  const liveApprovals = useMemo(
    () => filterLiveRuntimeApprovals(approvals?.entries ?? [], sessions?.entries ?? []),
    [approvals, sessions],
  )

  const refreshRuntime = () => {
    qc.invalidateQueries({ queryKey: ['runtime-rules'] })
    qc.invalidateQueries({ queryKey: ['runtime-events'] })
    qc.invalidateQueries({ queryKey: ['runtime-approvals'] })
    qc.invalidateQueries({ queryKey: ['runtime-sessions'] })
    qc.invalidateQueries({ queryKey: ['tasks'] })
    qc.invalidateQueries({ queryKey: ['overview'] })
  }

  const createRuleMut = useMutation({
    mutationFn: (rule: RuleDraft) => api.runtime.createRule(rule),
    onSuccess: () => {
      setEditingRule(null)
      refreshRuntime()
    },
  })
  const updateRuleMut = useMutation({
    mutationFn: (rule: RuleDraft) => api.runtime.updateRule(rule.id!, rule),
    onSuccess: () => {
      setEditingRule(null)
      refreshRuntime()
    },
  })
  const deleteRuleMut = useMutation({
    mutationFn: (ruleId: string) => api.runtime.deleteRule(ruleId),
    onSuccess: refreshRuntime,
  })
  const promoteTaskMut = useMutation({
    mutationFn: ({ eventId, lifetime }: { eventId: string; lifetime: 'session' | 'standing' }) =>
      api.runtime.promoteEventToTask(eventId, lifetime),
    onSuccess: refreshRuntime,
  })

  const startCreateRule = (kind: 'egress' | 'tool') => {
    setEditingRule(kind === 'egress' ? emptyEgressRule() : emptyToolRule())
  }

  return (
    <div className="p-4 sm:p-8 space-y-8">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h1 className="text-2xl font-bold text-text-primary">Runtime Controls</h1>
          <p className="text-sm text-text-tertiary mt-1">
            Tune runtime policy, starter profiles, approvals, and live sessions without dropping into raw proxy details.
          </p>
        </div>
        <div className="flex items-center gap-2">
          <label className="text-xs text-text-tertiary">Scope</label>
          <select
            value={agentFilter}
            onChange={e => setAgentFilter(e.target.value)}
            className="rounded border border-border-default bg-surface-1 px-3 py-2 text-sm text-text-primary"
          >
            <option value="all">All agents</option>
            {agents.map(agent => (
              <option key={agent.id} value={agent.id}>{agent.name}</option>
            ))}
          </select>
        </div>
      </div>

      {status && (
        <RuntimeStatusPanel
          status={status}
          activeSessionCount={(sessions?.entries ?? []).filter(isActiveRuntimeSession).length}
        />
      )}

      <StarterProfilesPanel
        profiles={starterProfiles?.entries ?? []}
        agents={agents}
        agentFilter={agentFilter}
        onApplied={refreshRuntime}
      />

      {editingRule && (
        <RuleEditorCard
          key={editingRule.id ?? `${editingRule.kind}-${editingRule.action}-${editingRule.host ?? editingRule.tool_name ?? 'new'}`}
          agents={agents}
          draft={editingRule}
          busy={createRuleMut.isPending || updateRuleMut.isPending}
          onCancel={() => setEditingRule(null)}
          onSave={(draft) => {
            if (draft.id) updateRuleMut.mutate(draft)
            else createRuleMut.mutate(draft)
          }}
        />
      )}

      <RuleSection
        title="Global Egress Rules"
        subtitle="Fast-path controls for background and harness HTTP noise."
        rules={egressRules?.entries ?? []}
        agents={agentMap}
        onNew={() => startCreateRule('egress')}
        onEdit={setEditingRule}
        onToggle={(rule) => updateRuleMut.mutate({ ...rule, scope: rule.agent_id ? 'agent' : 'global', enabled: !rule.enabled })}
        onDelete={(rule) => deleteRuleMut.mutate(rule.id)}
      />

      <RuleSection
        title="Global Tool Rules"
        subtitle="Allow, review, or deny repeated tool-use patterns before they hit task friction."
        rules={toolRules?.entries ?? []}
        agents={agentMap}
        onNew={() => startCreateRule('tool')}
        onEdit={setEditingRule}
        onToggle={(rule) => updateRuleMut.mutate({ ...rule, scope: rule.agent_id ? 'agent' : 'global', enabled: !rule.enabled })}
        onDelete={(rule) => deleteRuleMut.mutate(rule.id)}
      />

      <RuntimeApprovalsPanel approvals={liveApprovals} onResolved={refreshRuntime} />

      <RuntimeSessionsPanel sessions={sessions?.entries ?? []} agents={agentMap} onUpdated={refreshRuntime} />

      <RuntimeEventsPanel
        events={events?.entries ?? []}
        agents={agentMap}
        onResolved={refreshRuntime}
        onEditRule={async (event, action) => {
          const candidate = await api.runtime.getRuleCandidate(event.id, action)
          setEditingRule({
            ...candidate.rule,
            scope: candidate.scope_default,
          })
        }}
        onPromoteTask={(eventId, lifetime) => promoteTaskMut.mutate({ eventId, lifetime })}
      />
    </div>
  )
}

export function RuntimeStatusPanel({ status, activeSessionCount }: { status: RuntimeStatus; activeSessionCount: number }) {
  return (
    <section className="rounded-md border border-border-default bg-surface-1 p-5 space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h2 className="text-lg font-semibold text-text-primary">Runtime posture</h2>
          <p className="text-sm text-text-tertiary mt-1">Global defaults for proxy-backed sessions.</p>
        </div>
        <div className="flex flex-wrap items-center gap-2 text-xs font-mono">
          <span className={`rounded px-2.5 py-1 ${status.enabled ? 'bg-success/15 text-success' : 'bg-surface-2 text-text-tertiary'}`}>
            {status.enabled ? 'proxy enabled' : 'proxy disabled'}
          </span>
          <span className="rounded bg-surface-2 px-2.5 py-1 text-text-secondary">
            {activeSessionCount} active session{activeSessionCount === 1 ? '' : 's'}
          </span>
        </div>
      </div>
      <div className="grid gap-3 md:grid-cols-4">
        <Metric label="Default mode" value={status.observation_mode_default ? 'Observe' : 'Enforce'} />
        <Metric label="Inline approvals" value={status.inline_approval_enabled ? 'Enabled' : 'Disabled'} />
        <Metric label="Outbound credential mode" value={status.autovault_mode ?? 'observe'} />
        <Metric label="Inject stored bearer" value={status.inject_stored_bearer ? 'On' : 'Off'} />
      </div>
      {status.proxy_url && (
        <div className="rounded border border-border-subtle bg-surface-0 p-3">
          <div className="text-xs uppercase tracking-wider text-text-tertiary">Proxy endpoint</div>
          <code className="mt-1 block text-xs text-text-primary break-all">{status.proxy_url}</code>
        </div>
      )}
    </section>
  )
}

function Metric({ label, value }: { label: string; value: string }) {
  return (
    <div className="rounded border border-border-subtle bg-surface-0 p-3">
      <div className="text-xs uppercase tracking-wider text-text-tertiary">{label}</div>
      <div className="mt-1 text-sm font-medium text-text-primary">{value}</div>
    </div>
  )
}

export function StarterProfilesPanel({
  profiles,
  agents,
  agentFilter,
  onApplied,
}: {
  profiles: StarterProfile[]
  agents: Agent[]
  agentFilter: string
  onApplied: () => void
}) {
  const [applyingProfile, setApplyingProfile] = useState<string | null>(null)
  const [targetAgent, setTargetAgent] = useState<string>(agentFilter === 'all' ? '' : agentFilter)
  const applyMut = useMutation({
    mutationFn: ({ profileId, agentId }: { profileId: string; agentId?: string }) => api.runtime.applyStarterProfile(profileId, agentId),
    onSuccess: () => {
      setApplyingProfile(null)
      onApplied()
    },
  })

  return (
    <section className="rounded-md border border-border-default bg-surface-1 p-5 space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h2 className="text-lg font-semibold text-text-primary">Starter Profiles</h2>
          <p className="text-sm text-text-tertiary mt-1">
            Apply recommended runtime rules for common harnesses, then edit the resulting rules directly.
          </p>
        </div>
        <select
          value={targetAgent}
          onChange={e => setTargetAgent(e.target.value)}
          className="rounded border border-border-default bg-surface-0 px-3 py-2 text-sm text-text-primary"
        >
          <option value="">All agents</option>
          {agents.map(agent => (
            <option key={agent.id} value={agent.id}>{agent.name}</option>
          ))}
        </select>
      </div>
      <div className="grid gap-3 md:grid-cols-2">
        {profiles.map(profile => (
          <div key={profile.id} className="rounded border border-border-subtle bg-surface-0 p-4 space-y-3">
            <div>
              <div className="text-sm font-medium text-text-primary">{profile.display_name}</div>
              <div className="text-xs text-text-tertiary mt-1">{profile.description}</div>
            </div>
            <div className="text-xs text-text-secondary">
              {profile.rules.length} recommended rule{profile.rules.length === 1 ? '' : 's'}
            </div>
            <button
              onClick={() => {
                setApplyingProfile(profile.id)
                applyMut.mutate({ profileId: profile.id, agentId: targetAgent || undefined })
              }}
              disabled={applyMut.isPending}
              className="rounded bg-brand px-4 py-2 text-sm font-medium text-surface-0 hover:bg-brand-strong disabled:opacity-50"
            >
              {applyMut.isPending && applyingProfile === profile.id ? 'Applying…' : 'Apply profile'}
            </button>
          </div>
        ))}
      </div>
    </section>
  )
}

export function RuleSection({
  title,
  subtitle,
  rules,
  agents,
  onNew,
  onEdit,
  onToggle,
  onDelete,
}: {
  title: string
  subtitle: string
  rules: RuntimePolicyRule[]
  agents: Map<string, Agent>
  onNew: () => void
  onEdit: (rule: RuleDraft) => void
  onToggle: (rule: RuntimePolicyRule) => void
  onDelete: (rule: RuntimePolicyRule) => void
}) {
  return (
    <section className="rounded-md border border-border-default bg-surface-1 p-5 space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h2 className="text-lg font-semibold text-text-primary">{title}</h2>
          <p className="text-sm text-text-tertiary mt-1">{subtitle}</p>
        </div>
        <button onClick={onNew} className="rounded border border-brand/30 px-4 py-2 text-sm font-medium text-brand hover:bg-brand/10">
          Add rule
        </button>
      </div>
      <div className="space-y-3">
        {rules.length === 0 && (
          <div className="rounded border border-dashed border-border-default bg-surface-0 px-4 py-6 text-sm text-text-tertiary">
            No rules yet.
          </div>
        )}
        {rules.map(rule => (
          <div key={rule.id} className="rounded border border-border-subtle bg-surface-0 p-4">
            <div className="flex flex-wrap items-start justify-between gap-3">
              <div>
                <div className="flex flex-wrap items-center gap-2">
                  <span className={`rounded px-2 py-0.5 text-xs font-mono ${
                    rule.action === 'allow' ? 'bg-success/15 text-success' :
                    rule.action === 'deny' ? 'bg-danger/15 text-danger' :
                    'bg-warning/15 text-warning'
                  }`}>
                    {rule.action}
                  </span>
                  <span className="text-sm font-medium text-text-primary">
                    {rule.kind === 'egress'
                      ? [rule.host, rule.method, rule.path || rule.path_regex].filter(Boolean).join(' ')
                      : rule.tool_name}
                  </span>
                  <span className="text-xs text-text-tertiary">
                    {rule.agent_id ? (agents.get(rule.agent_id)?.name ?? 'Agent scoped') : 'All agents'}
                  </span>
                </div>
                <div className="mt-1 text-xs text-text-tertiary">
                  source: {rule.source} {rule.last_matched_at ? `· last matched ${new Date(rule.last_matched_at).toLocaleString()}` : ''}
                </div>
                {rule.reason && <div className="mt-2 text-sm text-text-secondary">{rule.reason}</div>}
              </div>
              <div className="flex flex-wrap gap-2">
                <button onClick={() => onToggle(rule)} className="rounded border border-border-default px-3 py-1.5 text-xs text-text-secondary hover:bg-surface-2">
                  {rule.enabled ? 'Disable' : 'Enable'}
                </button>
                <button onClick={() => onEdit({ ...rule, scope: rule.agent_id ? 'agent' : 'global' })} className="rounded border border-border-default px-3 py-1.5 text-xs text-text-secondary hover:bg-surface-2">
                  Edit
                </button>
                <button onClick={() => onDelete(rule)} className="rounded border border-danger/20 px-3 py-1.5 text-xs text-danger hover:bg-danger/10">
                  Delete
                </button>
              </div>
            </div>
          </div>
        ))}
      </div>
    </section>
  )
}

export function RuleEditorCard({
  agents,
  draft,
  busy,
  onCancel,
  onSave,
}: {
  agents: Agent[]
  draft: RuleDraft
  busy: boolean
  onCancel: () => void
  onSave: (draft: RuleDraft) => void
}) {
  const [local, setLocal] = useState<RuleDraft>(draft)
  const update = (patch: Partial<RuleDraft>) => setLocal(current => ({ ...current, ...patch }))

  return (
    <section className="rounded-md border border-brand/30 bg-surface-1 p-5 space-y-4">
      <div className="flex items-center justify-between gap-3">
        <div>
          <h2 className="text-lg font-semibold text-text-primary">{local.id ? 'Edit runtime rule' : 'Create runtime rule'}</h2>
          <p className="text-sm text-text-tertiary mt-1">Default shapes stay narrow; you can broaden scope explicitly before saving.</p>
        </div>
        <button onClick={onCancel} className="rounded border border-border-default px-3 py-2 text-sm text-text-secondary hover:bg-surface-2">
          Cancel
        </button>
      </div>
      <div className="grid gap-3 md:grid-cols-4">
        <select value={local.kind ?? 'egress'} onChange={e => update({ kind: e.target.value as 'egress' | 'tool' })} className="rounded border border-border-default bg-surface-0 px-3 py-2 text-sm text-text-primary">
          <option value="egress">Egress</option>
          <option value="tool">Tool</option>
        </select>
        <select value={local.action ?? 'allow'} onChange={e => update({ action: e.target.value as 'allow' | 'deny' | 'review' })} className="rounded border border-border-default bg-surface-0 px-3 py-2 text-sm text-text-primary">
          <option value="allow">Allow</option>
          <option value="review">Review</option>
          <option value="deny">Deny</option>
        </select>
        <select value={local.scope ?? 'agent'} onChange={e => update({ scope: e.target.value as 'agent' | 'global' })} className="rounded border border-border-default bg-surface-0 px-3 py-2 text-sm text-text-primary">
          <option value="agent">This agent</option>
          <option value="global">All agents</option>
        </select>
        <select value={local.agent_id ?? ''} onChange={e => update({ agent_id: e.target.value || undefined })} disabled={local.scope === 'global'} className="rounded border border-border-default bg-surface-0 px-3 py-2 text-sm text-text-primary disabled:opacity-50">
          <option value="">Choose agent</option>
          {agents.map(agent => (
            <option key={agent.id} value={agent.id}>{agent.name}</option>
          ))}
        </select>
      </div>
      {local.kind === 'egress' ? (
        <div className="grid gap-3 md:grid-cols-3">
          <input value={local.host ?? ''} onChange={e => update({ host: e.target.value })} placeholder="host" className="rounded border border-border-default bg-surface-0 px-3 py-2 text-sm text-text-primary" />
          <input value={local.method ?? ''} onChange={e => update({ method: e.target.value.toUpperCase() })} placeholder="method" className="rounded border border-border-default bg-surface-0 px-3 py-2 text-sm text-text-primary" />
          <input value={local.path ?? ''} onChange={e => update({ path: e.target.value })} placeholder="/path" className="rounded border border-border-default bg-surface-0 px-3 py-2 text-sm text-text-primary" />
          <input value={local.path_regex ?? ''} onChange={e => update({ path_regex: e.target.value })} placeholder="optional path regex" className="rounded border border-border-default bg-surface-0 px-3 py-2 text-sm text-text-primary md:col-span-3" />
        </div>
      ) : (
        <div className="grid gap-3 md:grid-cols-2">
          <input value={local.tool_name ?? ''} onChange={e => update({ tool_name: e.target.value })} placeholder="tool name" className="rounded border border-border-default bg-surface-0 px-3 py-2 text-sm text-text-primary" />
          <input value={local.input_regex ?? ''} onChange={e => update({ input_regex: e.target.value })} placeholder="optional input regex" className="rounded border border-border-default bg-surface-0 px-3 py-2 text-sm text-text-primary" />
        </div>
      )}
      <textarea value={local.reason ?? ''} onChange={e => update({ reason: e.target.value })} placeholder="Short reason / note" className="min-h-[88px] w-full rounded border border-border-default bg-surface-0 px-3 py-2 text-sm text-text-primary" />
      <div className="flex justify-end">
        <button
          onClick={() => onSave(local)}
          disabled={busy}
          className="rounded bg-brand px-4 py-2 text-sm font-medium text-surface-0 hover:bg-brand-strong disabled:opacity-50"
        >
          {busy ? 'Saving…' : local.id ? 'Save rule' : 'Create rule'}
        </button>
      </div>
    </section>
  )
}

export function RuntimeApprovalsPanel({ approvals, onResolved }: { approvals: ApprovalRecord[]; onResolved: () => void }) {
  const resolveMut = useMutation({
    mutationFn: ({ approvalId, resolution }: { approvalId: string; resolution: 'allow_once' | 'allow_session' | 'allow_always' | 'deny' }) =>
      api.runtime.resolveApproval(approvalId, resolution),
    onSuccess: onResolved,
  })

  return (
    <section className="rounded-md border border-border-default bg-surface-1 p-5 space-y-4">
      <div>
        <h2 className="text-lg font-semibold text-text-primary">Pending Runtime Approvals</h2>
        <p className="text-sm text-text-tertiary mt-1">Held tool calls, one-off egress reviews, and credential reviews all resolve here.</p>
      </div>
      <div className="space-y-3">
        {approvals.length === 0 && <EmptyPanel text="No pending runtime approvals." />}
        {approvals.map(approval => {
          const payload = approval.payload_json ?? {}
          const summary = approval.summary_json ?? {}
          const label = payload.tool_name
            ? payload.tool_name
            : payload.host
              ? `${payload.method ?? 'HTTP'} ${payload.host}${payload.path ?? ''}`
              : (summary.title ?? approval.kind)
          const detail = formatRuntimeApprovalDetail(payload)
          return (
            <div key={approval.id} className="rounded border border-border-subtle bg-surface-0 p-4">
              <div className="text-sm font-medium text-text-primary">{label}</div>
              <div className="mt-1 text-xs text-text-tertiary">{approval.kind} · {approval.resolution_transport || 'runtime review'}</div>
              {detail && <div className="mt-2 text-sm text-text-secondary">{detail}</div>}
              {typeof payload.reason === 'string' && payload.reason.trim() !== '' && (
                <div className="mt-1 text-xs text-text-tertiary">{payload.reason}</div>
              )}
              <div className="mt-3 flex flex-wrap gap-2">
                <ApprovalButton label="Allow Once" onClick={() => resolveMut.mutate({ approvalId: approval.id, resolution: 'allow_once' })} busy={resolveMut.isPending} />
                <ApprovalButton label="Allow Session" onClick={() => resolveMut.mutate({ approvalId: approval.id, resolution: 'allow_session' })} busy={resolveMut.isPending} />
                <ApprovalButton label="Allow Always" onClick={() => resolveMut.mutate({ approvalId: approval.id, resolution: 'allow_always' })} busy={resolveMut.isPending} />
                <button onClick={() => resolveMut.mutate({ approvalId: approval.id, resolution: 'deny' })} className="rounded border border-danger/20 px-3 py-1.5 text-xs text-danger hover:bg-danger/10">
                  Deny
                </button>
              </div>
            </div>
          )
        })}
      </div>
    </section>
  )
}

function ApprovalButton({ label, onClick, busy }: { label: string; onClick: () => void; busy: boolean }) {
  return (
    <button onClick={onClick} disabled={busy} className="rounded border border-brand/30 px-3 py-1.5 text-xs text-brand hover:bg-brand/10 disabled:opacity-50">
      {label}
    </button>
  )
}

function formatRuntimeApprovalDetail(payload: Record<string, any>): string {
  if (!payload || typeof payload !== 'object') return ''
  const toolName = typeof payload.tool_name === 'string' ? payload.tool_name : ''
  const toolInput = payload.tool_input && typeof payload.tool_input === 'object' ? payload.tool_input : {}
  if (toolName) {
    const filePath = readString(toolInput.file_path) || readString(toolInput.path) || readString(toolInput.directory)
    if (filePath) return `${toolName} ${filePath}`
    const pattern = readString(toolInput.pattern)
    if (pattern) return `${toolName} ${pattern}`
    const command = readString(toolInput.command)
    if (command) return `${toolName} ${command}`
    return toolName
  }
  if (typeof payload.host === 'string') {
    return [payload.method, payload.host, payload.path].filter(Boolean).join(' ')
  }
  return ''
}

function readString(value: unknown): string {
  return typeof value === 'string' ? value : ''
}

export function RuntimeSessionsPanel({
  sessions,
  agents,
  onUpdated,
}: {
  sessions: RuntimeSession[]
  agents: Map<string, Agent>
  onUpdated: () => void
}) {
  const revokeMut = useMutation({
    mutationFn: (sessionId: string) => api.runtime.revokeSession(sessionId),
    onSuccess: onUpdated,
  })
  const activeSessions = sessions.filter(isActiveRuntimeSession)
  return (
    <section className="rounded-md border border-border-default bg-surface-1 p-5 space-y-4">
      <div>
        <h2 className="text-lg font-semibold text-text-primary">Runtime Sessions</h2>
        <p className="text-sm text-text-tertiary mt-1">Current live sessions, including server-materialized durable Docker sessions.</p>
      </div>
      <div className="space-y-3">
        {activeSessions.length === 0 && <EmptyPanel text="No live runtime sessions." />}
        {activeSessions.map(session => (
          <div key={session.id} className="rounded border border-border-subtle bg-surface-0 p-4">
            <div className="flex flex-wrap items-center justify-between gap-3">
              <div>
                <div className="text-sm font-medium text-text-primary">{agents.get(session.agent_id)?.name ?? session.agent_id}</div>
                <div className="mt-1 text-xs text-text-tertiary">
                  {session.observation_mode ? 'observe' : 'enforce'} · started {new Date(session.created_at).toLocaleString()} · expires {new Date(session.expires_at).toLocaleString()}
                </div>
              </div>
              <button
                onClick={() => revokeMut.mutate(session.id)}
                disabled={revokeMut.isPending}
                className="rounded border border-danger/20 px-3 py-1.5 text-xs text-danger hover:bg-danger/10 disabled:opacity-50"
              >
                Revoke
              </button>
            </div>
          </div>
        ))}
      </div>
    </section>
  )
}

export function RuntimeEventsPanel({
  events,
  agents,
  onResolved,
  onEditRule,
  onPromoteTask,
}: {
  events: RuntimeEvent[]
  agents: Map<string, Agent>
  onResolved: () => void
  onEditRule: (event: RuntimeEvent, action: 'allow' | 'deny' | 'review') => void
  onPromoteTask: (eventId: string, lifetime: 'session' | 'standing') => void
}) {
  const resolveMut = useMutation({
    mutationFn: ({ approvalId, resolution }: { approvalId: string; resolution: 'allow_once' | 'allow_session' | 'allow_always' | 'deny' }) =>
      api.runtime.resolveApproval(approvalId, resolution),
    onSuccess: onResolved,
  })

  return (
    <section className="rounded-md border border-border-default bg-surface-1 p-5 space-y-4">
      <div>
        <h2 className="text-lg font-semibold text-text-primary">Recent Runtime Events</h2>
        <p className="text-sm text-text-tertiary mt-1">Use observed traffic to create narrow rules or promote it into task scope.</p>
      </div>
      <div className="space-y-3">
        {events.length === 0 && <EmptyPanel text="No runtime events yet." />}
        {events.map(event => {
          const meta = event.metadata_json ?? {}
          const isCredentialReview = event.event_type === 'runtime.autovault.review_required' && !!event.approval_id
          const subject = event.action_kind === 'tool_use'
            ? (meta.tool_name ?? event.event_type)
            : [meta.host, meta.method, meta.path].filter(Boolean).join(' ')
          return (
            <div key={event.id} className="rounded border border-border-subtle bg-surface-0 p-4">
              <div className="flex flex-wrap items-start justify-between gap-3">
                <div>
                  <div className="text-sm font-medium text-text-primary">{subject || event.event_type}</div>
                  <div className="mt-1 text-xs text-text-tertiary">
                    {agents.get(event.agent_id)?.name ?? event.agent_id} · {event.action_kind || 'runtime'} · {event.decision || 'observe'} / {event.outcome || 'n/a'}
                  </div>
                  {event.reason && <div className="mt-2 text-sm text-text-secondary">{event.reason}</div>}
                </div>
                <div className="text-xs text-text-tertiary">{new Date(event.timestamp).toLocaleString()}</div>
              </div>
              <div className="mt-3 flex flex-wrap gap-2">
                {isCredentialReview ? (
                  <>
                    <ApprovalButton
                      label="Allow Once"
                      onClick={() => resolveMut.mutate({ approvalId: event.approval_id!, resolution: 'allow_once' })}
                      busy={resolveMut.isPending}
                    />
                    <ApprovalButton
                      label="Allow For Session"
                      onClick={() => resolveMut.mutate({ approvalId: event.approval_id!, resolution: 'allow_session' })}
                      busy={resolveMut.isPending}
                    />
                    <ApprovalButton
                      label="Allow Always"
                      onClick={() => resolveMut.mutate({ approvalId: event.approval_id!, resolution: 'allow_always' })}
                      busy={resolveMut.isPending}
                    />
                    <button
                      onClick={() => resolveMut.mutate({ approvalId: event.approval_id!, resolution: 'deny' })}
                      className="rounded border border-danger/20 px-3 py-1.5 text-xs text-danger hover:bg-danger/10"
                    >
                      Deny
                    </button>
                  </>
                ) : (event.action_kind === 'egress' || event.action_kind === 'tool_use') && (
                  <>
                    <button onClick={() => onEditRule(event, 'allow')} className="rounded border border-brand/30 px-3 py-1.5 text-xs text-brand hover:bg-brand/10">Always allow</button>
                    <button onClick={() => onEditRule(event, 'review')} className="rounded border border-warning/30 px-3 py-1.5 text-xs text-warning hover:bg-warning/10">Always review</button>
                    <button onClick={() => onEditRule(event, 'deny')} className="rounded border border-danger/20 px-3 py-1.5 text-xs text-danger hover:bg-danger/10">Always deny</button>
                    <button onClick={() => onPromoteTask(event.id, 'session')} className="rounded border border-border-default px-3 py-1.5 text-xs text-text-secondary hover:bg-surface-2">Create session task</button>
                    <button onClick={() => onPromoteTask(event.id, 'standing')} className="rounded border border-border-default px-3 py-1.5 text-xs text-text-secondary hover:bg-surface-2">Create standing task</button>
                  </>
                )}
              </div>
            </div>
          )
        })}
      </div>
    </section>
  )
}

function EmptyPanel({ text }: { text: string }) {
  return (
    <div className="rounded border border-dashed border-border-default bg-surface-0 px-4 py-6 text-sm text-text-tertiary">
      {text}
    </div>
  )
}
