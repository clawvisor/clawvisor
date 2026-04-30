import { useMemo, useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { api, type Agent, type Restriction, type OrgRestriction, type RuntimeEvent, type ServiceInfo } from '../api/client'
import { useAuth } from '../hooks/useAuth'
import { serviceName, actionName } from '../lib/services'
import {
  type RuleDraft,
  isActiveRuntimeSession,
  RuntimeStatusPanel,
  StarterProfilesPanel,
  RuleEditorCard,
  RuleSection,
  emptyEgressRule,
  emptyToolRule,
} from './Runtime'

function Toggle({
  checked,
  disabled,
  loading,
  onChange,
}: {
  checked: boolean
  disabled?: boolean
  loading?: boolean
  onChange: (checked: boolean) => void
}) {
  return (
    <button
      role="switch"
      aria-checked={checked}
      disabled={disabled || loading}
      onClick={() => onChange(!checked)}
      className={`relative inline-flex h-5 w-9 shrink-0 rounded-full transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-offset-2 ${
        disabled ? 'opacity-40 cursor-not-allowed' : 'cursor-pointer'
      } ${loading ? 'opacity-60' : ''} ${checked ? 'bg-danger' : 'bg-border-strong'}`}
    >
      <span
        className={`pointer-events-none inline-block h-4 w-4 rounded-full bg-white shadow transform transition-transform mt-0.5 ${
          checked ? 'translate-x-[18px] ml-0' : 'translate-x-0.5'
        }`}
      />
    </button>
  )
}

function ActionRow({
  serviceId,
  action,
  restrictionId,
  disabled,
  orgId,
}: {
  serviceId: string
  action: string
  restrictionId: string | null
  disabled: boolean
  orgId?: string
}) {
  const qc = useQueryClient()
  const [reason, setReason] = useState('')
  const [showReason, setShowReason] = useState(false)

  const createMut = useMutation({
    mutationFn: async (r: string) => {
      if (orgId) await api.orgs.restrictions.create(orgId, serviceId, action, r)
      else await api.restrictions.create(serviceId, action, r)
    },
    onSuccess: () => {
      setReason('')
      setShowReason(false)
      qc.invalidateQueries({ queryKey: ['restrictions'] })
    },
  })

  const deleteMut = useMutation({
    mutationFn: async () => {
      if (orgId) await api.orgs.restrictions.delete(orgId, restrictionId!)
      else await api.restrictions.delete(restrictionId!)
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['restrictions'] })
    },
  })

  const isBlocked = !!restrictionId
  const loading = createMut.isPending || deleteMut.isPending

  function handleToggle(checked: boolean) {
    if (checked) {
      setShowReason(true)
    } else if (restrictionId) {
      deleteMut.mutate()
    }
  }

  function handleConfirmBlock() {
    createMut.mutate(reason.trim())
  }

  return (
    <div className={`flex items-center gap-3 py-2 px-4 ${loading ? 'opacity-60' : ''}`}>
      <Toggle
        checked={isBlocked}
        disabled={disabled}
        loading={loading}
        onChange={handleToggle}
      />
      <span className={`text-sm flex-1 ${isBlocked ? 'text-danger font-medium' : 'text-text-secondary'}`}>
        {actionName(action)}
      </span>
      {isBlocked && !showReason && (
        <span className="text-xs text-danger">Blocked</span>
      )}
      {showReason && !isBlocked && (
        <div className="flex items-center gap-2">
          <input
            type="text"
            value={reason}
            onChange={e => setReason(e.target.value)}
            onKeyDown={e => e.key === 'Enter' && handleConfirmBlock()}
            placeholder="Reason (optional)"
            className="text-xs rounded border border-border-default bg-surface-0 text-text-primary px-2 py-1 focus:outline-none focus:ring-1 focus:ring-danger/30 focus:border-danger w-44 placeholder:text-text-tertiary"
            autoFocus
          />
          <button
            onClick={handleConfirmBlock}
            disabled={createMut.isPending}
            className="text-xs px-2 py-1 rounded bg-danger text-surface-0 hover:bg-red-500 disabled:opacity-50"
          >
            Block
          </button>
          <button
            onClick={() => setShowReason(false)}
            className="text-xs text-text-tertiary hover:text-text-primary"
          >
            Cancel
          </button>
        </div>
      )}
    </div>
  )
}

function WildcardToggle({
  serviceId,
  restrictionId,
  orgId,
}: {
  serviceId: string
  restrictionId: string | null
  orgId?: string
}) {
  const qc = useQueryClient()
  const [reason, setReason] = useState('')
  const [showReason, setShowReason] = useState(false)

  const createMut = useMutation({
    mutationFn: async (r: string) => {
      if (orgId) await api.orgs.restrictions.create(orgId, serviceId, '*', r)
      else await api.restrictions.create(serviceId, '*', r)
    },
    onSuccess: () => {
      setReason('')
      setShowReason(false)
      qc.invalidateQueries({ queryKey: ['restrictions'] })
    },
  })

  const deleteMut = useMutation({
    mutationFn: async () => {
      if (orgId) await api.orgs.restrictions.delete(orgId, restrictionId!)
      else await api.restrictions.delete(restrictionId!)
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['restrictions'] })
    },
  })

  const isBlocked = !!restrictionId
  const loading = createMut.isPending || deleteMut.isPending

  function handleToggle(checked: boolean) {
    if (checked) {
      setShowReason(true)
    } else if (restrictionId) {
      deleteMut.mutate()
    }
  }

  function handleConfirmBlock() {
    createMut.mutate(reason.trim())
  }

  return (
    <div className={`flex items-center gap-3 ${loading ? 'opacity-60' : ''}`}>
      <Toggle checked={isBlocked} loading={loading} onChange={handleToggle} />
      <span className={`text-xs font-medium ${isBlocked ? 'text-danger' : 'text-text-tertiary'}`}>
        Block all actions
      </span>
      {showReason && !isBlocked && (
        <div className="flex flex-wrap items-center gap-2 ml-2">
          <input
            type="text"
            value={reason}
            onChange={e => setReason(e.target.value)}
            onKeyDown={e => e.key === 'Enter' && handleConfirmBlock()}
            placeholder="Reason (optional)"
            className="text-xs rounded border border-border-default bg-surface-0 text-text-primary px-2 py-1 focus:outline-none focus:ring-1 focus:ring-danger/30 focus:border-danger w-44 placeholder:text-text-tertiary"
            autoFocus
          />
          <button
            onClick={handleConfirmBlock}
            disabled={createMut.isPending}
            className="text-xs px-2 py-1 rounded bg-danger text-surface-0 hover:bg-red-500 disabled:opacity-50"
          >
            Block
          </button>
          <button
            onClick={() => setShowReason(false)}
            className="text-xs text-text-tertiary hover:text-text-primary"
          >
            Cancel
          </button>
        </div>
      )}
    </div>
  )
}

function ServiceGroup({
  svc,
  restrictions,
  orgId,
}: {
  svc: ServiceInfo
  restrictions: (Restriction | OrgRestriction)[]
  orgId?: string
}) {
  // The restriction service key includes the alias when present (e.g. "google.gmail:personal").
  const svcKey = svc.alias ? `${svc.id}:${svc.alias}` : svc.id

  // Build lookup: "service:action" → restriction ID
  const lookup = new Map<string, string>()
  for (const r of restrictions) {
    if (r.service === svcKey) {
      lookup.set(`${r.service}:${r.action}`, r.id)
    }
  }

  const wildcardId = lookup.get(`${svcKey}:*`) ?? null
  const hasWildcard = !!wildcardId

  return (
    <div className="bg-surface-1 border border-border-default rounded-md overflow-hidden">
      <div className="px-4 py-3 flex items-center justify-between">
        <div>
          <h3 className="text-sm font-semibold text-text-primary">{serviceName(svc.id, svc.alias)}</h3>
          <p className="text-xs text-text-tertiary">{svcKey}</p>
        </div>
        <WildcardToggle serviceId={svcKey} restrictionId={wildcardId} orgId={orgId} />
      </div>
      <div className="border-t border-border-default divide-y divide-border-subtle">
        {svc.actions.map(action => (
          <ActionRow
            key={action.id}
            serviceId={svcKey}
            action={action.id}
            restrictionId={lookup.get(`${svcKey}:${action.id}`) ?? null}
            disabled={hasWildcard}
            orgId={orgId}
          />
        ))}
      </div>
    </div>
  )
}

export default function Policy() {
  const { currentOrg } = useAuth()
  const orgId = currentOrg?.id
  const qc = useQueryClient()
  const [showAll, setShowAll] = useState(false)
  const [agentFilter, setAgentFilter] = useState<string>('all')
  const [editingRule, setEditingRule] = useState<RuleDraft | null>(null)

  const { data: servicesData, isLoading: servicesLoading } = useQuery({
    queryKey: ['services'],
    queryFn: () => api.services.list(),
  })

  const { data: restrictions, isLoading: restrictionsLoading } = useQuery({
    queryKey: ['restrictions', orgId ?? 'personal'],
    queryFn: async (): Promise<(Restriction | OrgRestriction)[]> => orgId
      ? api.orgs.restrictions.list(orgId)
      : api.restrictions.list(),
  })
  const { data: agents = [] } = useQuery({
    queryKey: ['agents'],
    queryFn: () => api.agents.list(),
    enabled: !orgId,
  })
  const { data: status } = useQuery({
    queryKey: ['runtime-status'],
    queryFn: () => api.runtime.status(),
    enabled: !orgId,
  })
  const { data: sessions } = useQuery({
    queryKey: ['runtime-sessions'],
    queryFn: () => api.runtime.listSessions(),
    enabled: !orgId,
    refetchInterval: 15_000,
  })
  const { data: egressRules } = useQuery({
    queryKey: ['runtime-rules', 'egress', agentFilter],
    queryFn: () => api.runtime.listRules({ kind: 'egress', agent_id: agentFilter === 'all' ? undefined : agentFilter }),
    enabled: !orgId,
  })
  const { data: toolRules } = useQuery({
    queryKey: ['runtime-rules', 'tool', agentFilter],
    queryFn: () => api.runtime.listRules({ kind: 'tool', agent_id: agentFilter === 'all' ? undefined : agentFilter }),
    enabled: !orgId,
  })
  const { data: starterProfiles } = useQuery({
    queryKey: ['runtime-starter-profiles'],
    queryFn: () => api.runtime.listStarterProfiles(),
    enabled: !orgId,
  })
  const { data: runtimeEvents } = useQuery({
    queryKey: ['runtime-events', 'policy'],
    queryFn: () => api.runtime.listEvents(),
    enabled: !orgId,
    refetchInterval: 15_000,
  })

  const isLoading = servicesLoading || restrictionsLoading
  const allServices = servicesData?.services ?? []
  const allRestrictions = restrictions ?? []
  const agentMap = useMemo(() => new Map(agents.map((agent: Agent) => [agent.id, agent])), [agents])

  const activated = allServices.filter(s => s.status === 'activated')
  const unactivated = allServices.filter(s => s.status !== 'activated')

  const refreshRuntime = () => {
    qc.invalidateQueries({ queryKey: ['runtime-rules'] })
    qc.invalidateQueries({ queryKey: ['runtime-events'] })
    qc.invalidateQueries({ queryKey: ['runtime-approvals'] })
    qc.invalidateQueries({ queryKey: ['runtime-sessions'] })
    qc.invalidateQueries({ queryKey: ['tasks'] })
    qc.invalidateQueries({ queryKey: ['overview'] })
    qc.invalidateQueries({ queryKey: ['agents'] })
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

  const startCreateRule = (kind: 'egress' | 'tool') => {
    setEditingRule(kind === 'egress' ? emptyEgressRule() : emptyToolRule())
  }

  return (
    <div className="p-4 sm:p-8 space-y-8">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h1 className="text-2xl font-bold text-text-primary">Policy</h1>
          <p className="text-sm text-text-tertiary mt-1">
            Configure presets, runtime rules, defaults, and legacy service restrictions from one control surface.
          </p>
        </div>
        {!orgId && (
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
        )}
      </div>

      {!orgId && status && (
        <RuntimeStatusPanel
          status={status}
          activeSessionCount={(sessions?.entries ?? []).filter(isActiveRuntimeSession).length}
        />
      )}

      {!orgId && (
        <StarterProfilesPanel
          profiles={starterProfiles?.entries ?? []}
          agents={agents}
          agentFilter={agentFilter}
          onApplied={refreshRuntime}
        />
      )}

      {!orgId && (
        <ServicePresetsPanel
          agents={agents}
          agentFilter={agentFilter}
          onApplied={refreshRuntime}
        />
      )}

      {!orgId && (
        <PolicySuggestionsPanel
          events={runtimeEvents?.entries ?? []}
          agents={agentMap}
          agentFilter={agentFilter}
          onCreateRule={async (event, action) => {
            const candidate = await api.runtime.getRuleCandidate(event.id, action)
            setEditingRule({
              ...candidate.rule,
              scope: candidate.scope_default,
            })
          }}
        />
      )}

      {!orgId && editingRule && (
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

      {!orgId && (
        <RuleSection
          title="Runtime Egress Rules"
          subtitle="Fast-path controls for background and harness HTTP activity before it falls through to review logic."
          rules={egressRules?.entries ?? []}
          agents={agentMap}
          onNew={() => startCreateRule('egress')}
          onEdit={setEditingRule}
          onToggle={(rule) => updateRuleMut.mutate({ ...rule, scope: rule.agent_id ? 'agent' : 'global', enabled: !rule.enabled })}
          onDelete={(rule) => deleteRuleMut.mutate(rule.id)}
        />
      )}

      {!orgId && (
        <RuleSection
          title="Runtime Tool Rules"
          subtitle="Allow, review, or deny repeated tool-use patterns before they turn into unnecessary approval churn."
          rules={toolRules?.entries ?? []}
          agents={agentMap}
          onNew={() => startCreateRule('tool')}
          onEdit={setEditingRule}
          onToggle={(rule) => updateRuleMut.mutate({ ...rule, scope: rule.agent_id ? 'agent' : 'global', enabled: !rule.enabled })}
          onDelete={(rule) => deleteRuleMut.mutate(rule.id)}
        />
      )}

      <section className="rounded-md border border-border-default bg-surface-1 p-5 space-y-4">
        <div>
          <h2 className="text-lg font-semibold text-text-primary">Legacy Service Restrictions</h2>
          <p className="text-sm text-text-tertiary mt-1">
            Existing service-level block rules remain active here during the migration into unified policy.
          </p>
        </div>

        {isLoading && <div className="text-sm text-text-tertiary">Loading...</div>}

        {!isLoading && allServices.length === 0 && (
          <div className="text-sm text-text-tertiary py-8 text-center">
            No services registered. Add adapters in the server configuration to manage restrictions.
          </div>
        )}

        {!isLoading && allServices.length > 0 && activated.length === 0 && (
          <div className="text-sm text-text-tertiary py-8 text-center">
            Activate a service first to manage restrictions.{' '}
            <Link to="/dashboard/accounts" className="text-brand hover:underline">Go to Accounts</Link>
          </div>
        )}

        <div className="space-y-4">
          {activated.map(svc => (
            <ServiceGroup
              key={svc.alias ? `${svc.id}:${svc.alias}` : svc.id}
              svc={svc}
              restrictions={allRestrictions}
              orgId={orgId}
            />
          ))}
        </div>

        {unactivated.length > 0 && (
          <div className="space-y-4">
            <button
              onClick={() => setShowAll(s => !s)}
              className="text-sm text-text-tertiary hover:text-text-primary"
            >
              {showAll ? 'Hide unactivated services' : `Show all services (${unactivated.length} not activated)`}
            </button>
            {showAll && (
              <div className="space-y-4 opacity-50">
                {unactivated.map(svc => (
                  <ServiceGroup
                    key={svc.alias ? `${svc.id}:${svc.alias}` : svc.id}
                    svc={svc}
                    restrictions={allRestrictions}
                    orgId={orgId}
                  />
                ))}
              </div>
            )}
          </div>
        )}
      </section>
    </div>
  )
}

function PolicySuggestionsPanel({
  events,
  agents,
  agentFilter,
  onCreateRule,
}: {
  events: RuntimeEvent[]
  agents: Map<string, Agent>
  agentFilter: string
  onCreateRule: (event: RuntimeEvent, action: 'allow' | 'review' | 'deny') => Promise<void>
}) {
  const [busyEventId, setBusyEventId] = useState<string | null>(null)
  const suggestionEvents = useMemo(() => {
    const filtered = events
      .filter(event => event.action_kind === 'egress' || event.action_kind === 'tool_use')
      .filter(event => agentFilter === 'all' || event.agent_id === agentFilter)
      .slice(0, 8)
    return filtered
  }, [agentFilter, events])

  return (
    <section className="rounded-md border border-border-default bg-surface-1 p-5 space-y-4">
      <div>
        <h2 className="text-lg font-semibold text-text-primary">Suggestions</h2>
        <p className="text-sm text-text-tertiary mt-1">
          Promote repeated runtime observations into policy without switching over to raw event inspection.
        </p>
      </div>
      <div className="space-y-3">
        {suggestionEvents.length === 0 && (
          <div className="rounded border border-dashed border-border-default bg-surface-0 px-4 py-6 text-sm text-text-tertiary">
            No recent policy suggestions yet.
          </div>
        )}
        {suggestionEvents.map(event => {
          const meta = (event.metadata_json ?? {}) as Record<string, any>
          const subject = event.action_kind === 'tool_use'
            ? [meta.tool_name, meta.tool_input?.path || meta.tool_input?.file_path || meta.tool_input?.pattern].filter(Boolean).join(' ')
            : [meta.method, meta.host, meta.path].filter(Boolean).join(' ')
          return (
            <div key={event.id} className="rounded border border-border-subtle bg-surface-0 p-4">
              <div className="flex flex-wrap items-start justify-between gap-3">
                <div>
                  <div className="text-sm font-medium text-text-primary">{subject || event.event_type}</div>
                  <div className="mt-1 text-xs text-text-tertiary">
                    {agents.get(event.agent_id)?.name ?? event.agent_id} · {event.action_kind} · {event.decision || 'observe'} / {event.outcome || 'n/a'}
                  </div>
                  {event.reason && <div className="mt-2 text-sm text-text-secondary">{event.reason}</div>}
                </div>
                <div className="text-xs text-text-tertiary">{new Date(event.timestamp).toLocaleString()}</div>
              </div>
              <div className="mt-3 flex flex-wrap gap-2">
                {(['allow', 'review', 'deny'] as const).map(action => (
                  <button
                    key={action}
                    onClick={async () => {
                      setBusyEventId(event.id + action)
                      try {
                        await onCreateRule(event, action)
                      } finally {
                        setBusyEventId(null)
                      }
                    }}
                    className={`rounded px-3 py-1.5 text-xs ${
                      action === 'allow'
                        ? 'border border-brand/30 text-brand hover:bg-brand/10'
                        : action === 'review'
                          ? 'border border-warning/30 text-warning hover:bg-warning/10'
                          : 'border border-danger/20 text-danger hover:bg-danger/10'
                    }`}
                    disabled={busyEventId === event.id + action}
                  >
                    {busyEventId === event.id + action ? 'Loading…' : `Create ${action} rule`}
                  </button>
                ))}
              </div>
            </div>
          )
        })}
      </div>
    </section>
  )
}

function ServicePresetsPanel({
  agents,
  agentFilter,
  onApplied,
}: {
  agents: Agent[]
  agentFilter: string
  onApplied: () => void
}) {
  const [targetAgent, setTargetAgent] = useState<string>(agentFilter === 'all' ? '' : agentFilter)
  const applyPresetMut = useMutation({
    mutationFn: async (agentId?: string) => {
      const baseRule: RuleDraft = {
        scope: agentId ? 'agent' : 'global',
        agent_id: agentId || undefined,
        kind: 'egress' as const,
        action: 'allow' as const,
        method: 'POST',
        host: 'api.telegram.org',
        enabled: true,
        source: 'system' as const,
      }
      const rules = [
        {
          ...baseRule,
          path_regex: '^/bot[^/]+/(getMe|getUpdates|deleteWebhook)$',
          reason: 'Telegram bot control-plane calls',
        },
        {
          ...baseRule,
          path_regex: '^/bot[^/]+/(sendMessage|sendChatAction|editMessageText)$',
          reason: 'Telegram bot messaging actions',
        },
      ]
      for (const rule of rules) {
        await api.runtime.createRule(rule)
      }
    },
    onSuccess: onApplied,
  })

  return (
    <section className="rounded-md border border-border-default bg-surface-1 p-5 space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h2 className="text-lg font-semibold text-text-primary">Service Presets</h2>
          <p className="text-sm text-text-tertiary mt-1">
            Apply narrow allowlists for common integrations without hand-authoring every runtime rule.
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
        <div className="rounded border border-border-subtle bg-surface-0 p-4 space-y-3">
          <div>
            <div className="text-sm font-medium text-text-primary">Telegram</div>
            <div className="text-xs text-text-tertiary mt-1">
              Installs narrow runtime egress allow rules for Telegram bot polling and messaging endpoints.
            </div>
          </div>
          <div className="text-xs text-text-secondary">
            2 recommended rules · control plane + messaging actions
          </div>
          <button
            onClick={() => applyPresetMut.mutate(targetAgent || undefined)}
            disabled={applyPresetMut.isPending}
            className="rounded bg-brand px-4 py-2 text-sm font-medium text-surface-0 hover:bg-brand-strong disabled:opacity-50"
          >
            {applyPresetMut.isPending ? 'Applying…' : 'Apply preset'}
          </button>
        </div>
      </div>
    </section>
  )
}
