import { useCallback, useEffect, useMemo, useState, type ReactNode } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { Link, useSearchParams } from 'react-router-dom'
import { api, type Agent, type Restriction, type OrgRestriction, type RuntimePolicyRule, type RuntimeToolControl, type ServiceInfo } from '../api/client'
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
  const { currentOrg, features } = useAuth()
  const orgId = currentOrg?.id
  const qc = useQueryClient()
  const [searchParams, setSearchParams] = useSearchParams()
  const linkedAgentId = searchParams.get('agent_id') ?? ''
  const runtimePolicyUI = !orgId && !!features?.runtime_policy_ui
  const servicePresetsUI = !orgId && !!features?.service_presets
  const [showAll, setShowAll] = useState(false)
  const [agentFilter, setAgentFilter] = useState<string>(() => linkedAgentId || 'all')
  const [editingRule, setEditingRule] = useState<RuleDraft | null>(null)
  const [rulesTab, setRulesTab] = useState<'service' | 'egress' | 'tool'>('service')
  const [proxyLiteTab, setProxyLiteTab] = useState<'tools' | 'accounts'>('tools')

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
    enabled: !orgId && (runtimePolicyUI || servicePresetsUI),
  })
  const { data: status } = useQuery({
    queryKey: ['runtime-status'],
    queryFn: () => api.runtime.status(),
    enabled: runtimePolicyUI,
  })
  const proxyLiteOnly = runtimePolicyUI && !!status?.proxy_lite_enabled && !status.enabled
  const fullProxyActive = runtimePolicyUI && !!status?.enabled
  const { data: sessions } = useQuery({
    queryKey: ['runtime-sessions'],
    queryFn: () => api.runtime.listSessions(),
    enabled: fullProxyActive,
    refetchInterval: 15_000,
  })
  const { data: egressRules } = useQuery({
    queryKey: ['runtime-rules', 'egress'],
    queryFn: () => api.runtime.listRules({ kind: 'egress' }),
    enabled: fullProxyActive,
  })
  const { data: toolRules } = useQuery({
    queryKey: ['runtime-rules', 'tool'],
    queryFn: () => api.runtime.listRules({ kind: 'tool' }),
    enabled: fullProxyActive,
  })
  const { data: toolControls } = useQuery({
    queryKey: ['runtime-tool-controls', agentFilter],
    queryFn: () => api.runtime.listToolControls(agentFilter),
    enabled: proxyLiteOnly && agentFilter !== 'all',
  })
  const { data: starterProfiles } = useQuery({
    queryKey: ['runtime-starter-profiles'],
    queryFn: () => api.runtime.listStarterProfiles(),
    enabled: fullProxyActive,
  })
  const isLoading = servicesLoading || restrictionsLoading
  const allServices = servicesData?.services ?? []
  const allRestrictions = restrictions ?? []
  const agentMap = useMemo(() => new Map(agents.map((agent: Agent) => [agent.id, agent])), [agents])

  const activated = allServices.filter(s => s.status === 'activated')
  const unactivated = allServices.filter(s => s.status !== 'activated')

  const setPolicyAgentFilter = useCallback((nextAgentId: string) => {
    setAgentFilter(nextAgentId)
    setSearchParams(current => {
      const next = new URLSearchParams(current)
      if (nextAgentId && nextAgentId !== 'all') next.set('agent_id', nextAgentId)
      else next.delete('agent_id')
      return next
    }, { replace: true })
  }, [setSearchParams])

  const refreshRuntime = () => {
    qc.invalidateQueries({ queryKey: ['runtime-rules'] })
    qc.invalidateQueries({ queryKey: ['runtime-tool-controls'] })
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
  const updateToolControlMut = useMutation({
    mutationFn: (control: { agent_id: string; tool_name: string; action?: 'unset' | 'allow' | 'deny'; scope: 'global' | 'agent'; read_only_commands_allowed?: boolean; sensitive_file_guard_enabled?: boolean }) => api.runtime.updateToolControl(control),
    onSuccess: refreshRuntime,
  })

  const startCreateRule = (kind: 'egress' | 'tool', patch: Partial<RuleDraft> = {}) => {
    const base = kind === 'egress' ? emptyEgressRule() : emptyToolRule()
    setEditingRule({ ...base, ...patch })
  }

  useEffect(() => {
    if (!runtimePolicyUI && rulesTab !== 'service') {
      setRulesTab('service')
    }
  }, [runtimePolicyUI, rulesTab])

  useEffect(() => {
    if (linkedAgentId && linkedAgentId !== agentFilter) {
      setAgentFilter(linkedAgentId)
    }
  }, [agentFilter, linkedAgentId])

  useEffect(() => {
    if (linkedAgentId && fullProxyActive && rulesTab === 'service') {
      setRulesTab('tool')
    }
  }, [fullProxyActive, linkedAgentId, rulesTab])

  useEffect(() => {
    if (!proxyLiteOnly || agents.length === 0) return
    if (agentFilter !== 'all' && agents.some(agent => agent.id === agentFilter)) return
    const linkedAgent = linkedAgentId && agents.some(agent => agent.id === linkedAgentId) ? linkedAgentId : ''
    setPolicyAgentFilter(linkedAgent || agents[0].id)
  }, [agentFilter, agents, linkedAgentId, proxyLiteOnly, setPolicyAgentFilter])

  return (
    <div className="p-4 sm:p-8 space-y-8">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h1 className="text-2xl font-bold text-text-primary">Policy</h1>
          <p className="text-sm text-text-tertiary mt-1">
            {proxyLiteOnly
              ? 'Configure harness tool access and connected account restrictions.'
              : runtimePolicyUI || servicePresetsUI
                ? 'Configure presets, runtime rules, defaults, and legacy service restrictions from one control surface.'
              : 'Configure service restrictions for your connected adapters and integrations.'}
          </p>
        </div>
      </div>

      {fullProxyActive && status && (
        <RuntimeStatusPanel
          status={status}
          activeSessionCount={(sessions?.entries ?? []).filter(isActiveRuntimeSession).length}
        />
      )}

      {fullProxyActive && (
        <StarterProfilesPanel
          profiles={starterProfiles?.entries ?? []}
          agents={agents}
          agentFilter="all"
          onApplied={refreshRuntime}
        />
      )}

      {servicePresetsUI && fullProxyActive && (
        <ServicePresetsPanel
          agents={agents}
          agentFilter="all"
          onApplied={refreshRuntime}
        />
      )}

      {(fullProxyActive || proxyLiteOnly) && editingRule && (
        <RuleEditorCard
          key={editingRule.id ?? `${editingRule.kind}-${editingRule.action}-${editingRule.host ?? editingRule.tool_name ?? 'new'}`}
          agents={agents}
          draft={editingRule}
          busy={createRuleMut.isPending || updateRuleMut.isPending}
          allowedKinds={proxyLiteOnly ? ['tool'] : undefined}
          defaultAgentId={proxyLiteOnly && agentFilter !== 'all' ? agentFilter : undefined}
          toolNameOptions={proxyLiteOnly ? (toolControls?.entries ?? []).map(control => control.tool_name) : undefined}
          onCancel={() => setEditingRule(null)}
          onSave={(draft) => {
            if (draft.id) updateRuleMut.mutate(draft)
            else createRuleMut.mutate(draft)
          }}
        />
      )}

      {proxyLiteOnly ? (
        <ProxyLitePolicyTabs
          activeTab={proxyLiteTab}
          onTabChange={setProxyLiteTab}
          toolCount={toolControls?.entries?.length ?? 0}
          accountCount={allRestrictions.length}
          tools={
            <ProxyLiteToolControlsPanel
              agentId={agentFilter}
              agentList={agents}
              onAgentChange={setPolicyAgentFilter}
              controls={toolControls?.entries ?? []}
              agents={agentMap}
              busy={updateToolControlMut.isPending}
              onChange={(toolName, action, scope) => updateToolControlMut.mutate({ agent_id: agentFilter, tool_name: toolName, action, scope })}
              onReadOnlyCommandsChange={(toolName, allowed, scope) => updateToolControlMut.mutate({ agent_id: agentFilter, tool_name: toolName, read_only_commands_allowed: allowed, scope })}
              onSensitiveFileGuardChange={(toolName, enabled, scope) => updateToolControlMut.mutate({ agent_id: agentFilter, tool_name: toolName, sensitive_file_guard_enabled: enabled, scope })}
              ruleBusy={createRuleMut.isPending || updateRuleMut.isPending}
              onSaveRule={async (draft) => {
                if (draft.id) await updateRuleMut.mutateAsync(draft)
                else await createRuleMut.mutateAsync(draft)
              }}
              onToggleAdvanced={(rule) => updateRuleMut.mutate({ ...rule, scope: rule.agent_id ? 'agent' : 'global', enabled: !rule.enabled })}
              onDeleteAdvanced={(rule) => deleteRuleMut.mutate(rule.id)}
            />
          }
          accounts={
            <AccountControlsPanel
              orgId={orgId}
              isLoading={isLoading}
              allServices={allServices}
              activated={activated}
              unactivated={unactivated}
              restrictions={allRestrictions}
              showAll={showAll}
              onToggleShowAll={() => setShowAll(s => !s)}
            />
          }
        />
      ) : (
        <PolicyRulesPanel
          orgId={orgId}
          rulesTab={rulesTab}
          onTabChange={setRulesTab}
          serviceRuleCount={allRestrictions.length}
          egressRuleCount={egressRules?.entries?.length ?? 0}
          toolRuleCount={toolRules?.entries?.length ?? 0}
          runtimePolicyUI={fullProxyActive}
          isLoading={isLoading}
          allServices={allServices}
          activated={activated}
          unactivated={unactivated}
          restrictions={allRestrictions}
          showAll={showAll}
          onToggleShowAll={() => setShowAll(s => !s)}
          agents={agentMap}
          agentList={agents}
          linkedAgentId={linkedAgentId}
          onAgentFilterChange={setPolicyAgentFilter}
          egressRules={egressRules?.entries ?? []}
          toolRules={toolRules?.entries ?? []}
          onNewRule={startCreateRule}
          onEditRule={setEditingRule}
          onToggleRule={(rule) => updateRuleMut.mutate({ ...rule, scope: rule.agent_id ? 'agent' : 'global', enabled: !rule.enabled })}
          onDeleteRule={(rule) => deleteRuleMut.mutate(rule.id)}
        />
      )}
    </div>
  )
}

function ProxyLitePolicyTabs({
  activeTab,
  onTabChange,
  toolCount,
  accountCount,
  tools,
  accounts,
}: {
  activeTab: 'tools' | 'accounts'
  onTabChange: (tab: 'tools' | 'accounts') => void
  toolCount: number
  accountCount: number
  tools: ReactNode
  accounts: ReactNode
}) {
  const tabs: Array<{ id: 'tools' | 'accounts'; label: string; count: number }> = [
    { id: 'tools', label: 'Tool Controls', count: toolCount },
    { id: 'accounts', label: 'Account Controls', count: accountCount },
  ]

  return (
    <section className="rounded-md border border-border-default bg-surface-1 p-5 space-y-4">
      <div className="inline-flex rounded-lg border border-border-default bg-surface-0 p-1">
        {tabs.map(tab => {
          const active = activeTab === tab.id
          return (
            <button
              key={tab.id}
              onClick={() => onTabChange(tab.id)}
              className={`inline-flex items-center gap-2 rounded-md px-3 py-2 text-sm transition ${
                active
                  ? 'bg-surface-1 text-text-primary shadow-sm'
                  : 'text-text-tertiary hover:text-text-primary'
              }`}
            >
              <span>{tab.label}</span>
              <span className={`rounded-full px-2 py-0.5 text-xs ${active ? 'bg-border-subtle text-text-primary' : 'bg-surface-1 text-text-tertiary'}`}>
                {tab.count}
              </span>
            </button>
          )
        })}
      </div>

      {activeTab === 'tools' ? tools : accounts}
    </section>
  )
}

function ProxyLiteToolControlsPanel({
  agentId,
  agentList,
  onAgentChange,
  controls,
  agents,
  busy,
  onChange,
  onReadOnlyCommandsChange,
  onSensitiveFileGuardChange,
  ruleBusy,
  onSaveRule,
  onToggleAdvanced,
  onDeleteAdvanced,
}: {
  agentId: string
  agentList: Agent[]
  onAgentChange: (agentId: string) => void
  controls: RuntimeToolControl[]
  agents: Map<string, Agent>
  busy: boolean
  onChange: (toolName: string, action: 'unset' | 'allow' | 'deny', scope: 'global' | 'agent') => void
  onReadOnlyCommandsChange: (toolName: string, allowed: boolean, scope: 'global' | 'agent') => void
  onSensitiveFileGuardChange: (toolName: string, enabled: boolean, scope: 'global' | 'agent') => void
  ruleBusy: boolean
  onSaveRule: (draft: RuleDraft) => Promise<void>
  onToggleAdvanced: (rule: RuntimePolicyRule) => void
  onDeleteAdvanced: (rule: RuntimePolicyRule) => void
}) {
  const needsAgent = agentId === 'all'
  const globalControls = controls.filter(control =>
    isShellLikeToolName(control.tool_name) || control.sensitive_file_guard_applies || !!control.global_rule_id || (control.advanced_rules ?? []).some(rule => !rule.agent_id),
  )
  const agentSelector = (
    <div className="flex items-center gap-2">
      <label className="text-xs text-text-tertiary">Agent</label>
      <select
        value={agentId}
        onChange={e => onAgentChange(e.target.value)}
        className="rounded border border-border-default bg-surface-0 px-3 py-2 text-sm text-text-primary"
      >
        {agentList.map(agent => (
          <option key={agent.id} value={agent.id}>{agent.name}</option>
        ))}
      </select>
    </div>
  )
  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h2 className="text-lg font-semibold text-text-primary">Tool Controls</h2>
          <p className="text-sm text-text-tertiary mt-1">
            Tools are detected from the harness request body and from recent tool calls.
          </p>
        </div>
      </div>

      {needsAgent && (
        <div className="space-y-3">
          <div className="flex flex-wrap items-center justify-between gap-3">
            <div>
              <h3 className="text-sm font-semibold text-text-primary">Agent Tool Policies</h3>
              <p className="mt-1 text-xs text-text-tertiary">Selected-agent overrides and tools that are governed by task scopes.</p>
            </div>
            {agentList.length > 0 && agentSelector}
          </div>
          <div className="rounded border border-dashed border-border-default bg-surface-0 px-4 py-6 text-sm text-text-tertiary">
            {agentList.length > 0 ? 'Select an agent to configure its tool controls.' : 'No agents yet.'}
          </div>
        </div>
      )}

      {!needsAgent && controls.length === 0 && (
        <div className="space-y-3">
          <div className="flex flex-wrap items-center justify-between gap-3">
            <div>
              <h3 className="text-sm font-semibold text-text-primary">Agent Tool Policies</h3>
              <p className="mt-1 text-xs text-text-tertiary">Selected-agent overrides and tools that are governed by task scopes.</p>
            </div>
            {agentSelector}
          </div>
          <div className="rounded border border-dashed border-border-default bg-surface-0 px-4 py-6 text-sm text-text-tertiary">
            No tools discovered yet. Run the harness once and this list will populate from its request.
          </div>
        </div>
      )}

      {!needsAgent && controls.length > 0 && (
        <div className="space-y-5">
          <ToolControlListSection
            title="Global Tool Policies"
            subtitle="Apply to every agent."
            sectionScope="global"
            controls={globalControls}
            agents={agents}
            agentList={agentList}
            agentId={agentId}
            busy={busy}
            ruleBusy={ruleBusy}
            onChange={onChange}
            onReadOnlyCommandsChange={onReadOnlyCommandsChange}
            onSensitiveFileGuardChange={onSensitiveFileGuardChange}
            onSaveRule={onSaveRule}
            onToggleAdvanced={onToggleAdvanced}
            onDeleteAdvanced={onDeleteAdvanced}
          />
          <ToolControlListSection
            title="Agent Tool Policies"
            subtitle="Selected-agent overrides and tools that are governed by task scopes."
            sectionScope="agent"
            headerControl={agentSelector}
            controls={controls}
            agents={agents}
            agentList={agentList}
            agentId={agentId}
            busy={busy}
            ruleBusy={ruleBusy}
            onChange={onChange}
            onReadOnlyCommandsChange={onReadOnlyCommandsChange}
            onSensitiveFileGuardChange={onSensitiveFileGuardChange}
            onSaveRule={onSaveRule}
            onToggleAdvanced={onToggleAdvanced}
            onDeleteAdvanced={onDeleteAdvanced}
          />
        </div>
      )}
    </div>
  )
}

function ToolControlListSection({
  title,
  subtitle,
  sectionScope,
  headerControl,
  controls,
  agents,
  agentList,
  agentId,
  busy,
  ruleBusy,
  onChange,
  onReadOnlyCommandsChange,
  onSensitiveFileGuardChange,
  onSaveRule,
  onToggleAdvanced,
  onDeleteAdvanced,
}: {
  title: string
  subtitle: string
  sectionScope: 'global' | 'agent'
  headerControl?: ReactNode
  controls: RuntimeToolControl[]
  agents: Map<string, Agent>
  agentList: Agent[]
  agentId: string
  busy: boolean
  ruleBusy: boolean
  onChange: (toolName: string, action: 'unset' | 'allow' | 'deny', scope: 'global' | 'agent') => void
  onReadOnlyCommandsChange: (toolName: string, allowed: boolean, scope: 'global' | 'agent') => void
  onSensitiveFileGuardChange: (toolName: string, enabled: boolean, scope: 'global' | 'agent') => void
  onSaveRule: (draft: RuleDraft) => Promise<void>
  onToggleAdvanced: (rule: RuntimePolicyRule) => void
  onDeleteAdvanced: (rule: RuntimePolicyRule) => void
}) {
  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h3 className="text-sm font-semibold text-text-primary">{title}</h3>
          <p className="mt-1 text-xs text-text-tertiary">{subtitle}</p>
        </div>
        {headerControl}
      </div>
      {controls.length === 0 ? (
        <div className="rounded border border-dashed border-border-default bg-surface-0 px-4 py-6 text-sm text-text-tertiary">
          No policies yet.
        </div>
      ) : (
        <div className="divide-y divide-border-subtle rounded border border-border-subtle bg-surface-0">
          {controls.map(control => (
            <ToolControlRow
              key={control.tool_name}
              control={control}
              sectionScope={sectionScope}
              agents={agents}
              agentList={agentList}
              agentId={agentId}
              busy={busy}
              ruleBusy={ruleBusy}
              onChange={onChange}
              onReadOnlyCommandsChange={onReadOnlyCommandsChange}
              onSensitiveFileGuardChange={onSensitiveFileGuardChange}
              onSaveRule={onSaveRule}
              onToggleAdvanced={onToggleAdvanced}
              onDeleteAdvanced={onDeleteAdvanced}
            />
          ))}
        </div>
      )}
    </div>
  )
}

function ToolControlRow({
  control,
  sectionScope,
  agents,
  agentList,
  agentId,
  busy,
  ruleBusy,
  onChange,
  onReadOnlyCommandsChange,
  onSensitiveFileGuardChange,
  onSaveRule,
  onToggleAdvanced,
  onDeleteAdvanced,
}: {
  control: RuntimeToolControl
  sectionScope: 'global' | 'agent'
  agents: Map<string, Agent>
  agentList: Agent[]
  agentId: string
  busy: boolean
  ruleBusy: boolean
  onChange: (toolName: string, action: 'unset' | 'allow' | 'deny', scope: 'global' | 'agent') => void
  onReadOnlyCommandsChange: (toolName: string, allowed: boolean, scope: 'global' | 'agent') => void
  onSensitiveFileGuardChange: (toolName: string, enabled: boolean, scope: 'global' | 'agent') => void
  onSaveRule: (draft: RuleDraft) => Promise<void>
  onToggleAdvanced: (rule: RuntimePolicyRule) => void
  onDeleteAdvanced: (rule: RuntimePolicyRule) => void
}) {
  const [inlineDraft, setInlineDraft] = useState<RuleDraft | null>(null)
  const shellLike = isShellLikeToolName(control.tool_name)
  const guardApplies = !!control.sensitive_file_guard_applies
  const advancedRules = (control.advanced_rules ?? []).filter(rule =>
    sectionScope === 'global' ? !rule.agent_id : !!rule.agent_id,
  )
  const action = sectionScope === 'global' ? control.global_action : control.agent_action
  const showSimpleControl = sectionScope === 'agent' || shellLike || guardApplies || !!control.global_rule_id || advancedRules.length > 0
  if (!showSimpleControl && advancedRules.length === 0) return null
  const scopeLabel = sectionScope === 'global'
    ? control.global_rule_id ? 'Global policy' : 'No global policy'
    : control.agent_rule_id && action !== 'unset' ? 'Agent policy' : 'Task scopes'
  const readOnlyCommandsAllowed = sectionScope === 'global'
    ? control.global_read_only_commands_allowed ?? true
    : control.agent_read_only_commands_allowed ?? control.global_read_only_commands_allowed ?? true
  const readOnlyCommandsExplicit = sectionScope === 'global'
    ? control.global_read_only_commands_allowed !== undefined
    : control.agent_read_only_commands_allowed !== undefined
  const sensitiveGuardEnabled = sectionScope === 'global'
    ? control.global_sensitive_file_guard_enabled ?? true
    : control.agent_sensitive_file_guard_enabled ?? control.global_sensitive_file_guard_enabled ?? true
  const sensitiveGuardExplicit = sectionScope === 'global'
    ? control.global_sensitive_file_guard_enabled !== undefined
    : control.agent_sensitive_file_guard_enabled !== undefined
  return (
    <div className="p-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <div className="flex flex-wrap items-center gap-2">
            <span className="text-sm font-medium text-text-primary">{control.tool_name}</span>
            <span className="text-xs text-text-tertiary">{scopeLabel}</span>
            {advancedRules.length > 0 && (
              <span className="rounded bg-brand/10 px-2 py-0.5 text-xs text-brand">
                {advancedRules.length} advanced
              </span>
            )}
          </div>
          <div className="mt-1 text-xs text-text-tertiary">
            {control.last_seen_at ? `Last seen ${new Date(control.last_seen_at).toLocaleString()}` : 'Not seen yet'}
          </div>
        </div>
        {showSimpleControl && (
          <div className="flex flex-wrap items-center gap-2">
            <SegmentedToolAction value={action} disabled={busy} onChange={nextAction => onChange(control.tool_name, nextAction, sectionScope)} />
            <button
              onClick={() => setInlineDraft({ ...emptyToolRule(), scope: sectionScope, agent_id: sectionScope === 'agent' ? agentId : undefined, tool_name: control.tool_name })}
              className="rounded border border-border-default px-3 py-1.5 text-xs text-text-secondary hover:bg-surface-2"
            >
              Create rule
            </button>
          </div>
        )}
      </div>

      {shellLike && showSimpleControl && (
        <div className="mt-3 flex flex-wrap items-center justify-between gap-3 rounded border border-border-subtle bg-surface-1 px-3 py-2">
          <div>
            <label className="flex items-center gap-2 text-sm text-text-primary">
              <input
                type="checkbox"
                checked={readOnlyCommandsAllowed}
                disabled={busy}
                onChange={e => onReadOnlyCommandsChange(control.tool_name, e.target.checked, sectionScope)}
              />
              Allow read-only commands
            </label>
            <div className="mt-1 text-xs text-text-tertiary">
              {readOnlyCommandsExplicit ? 'Explicit policy' : 'Default on'} · applies to commands like ls, cat, grep, rg, find, wc, and pwd.
            </div>
          </div>
          <span className={`rounded px-2 py-0.5 text-xs ${readOnlyCommandsAllowed ? 'bg-success/15 text-success' : 'bg-warning/15 text-warning'}`}>
            {readOnlyCommandsAllowed ? 'allowed' : 'reviewed'}
          </span>
        </div>
      )}

      {guardApplies && showSimpleControl && (
        <div className="mt-3 flex flex-wrap items-center justify-between gap-3 rounded border border-border-subtle bg-surface-1 px-3 py-2">
          <div>
            <label className="flex items-center gap-2 text-sm text-text-primary">
              <input
                type="checkbox"
                checked={sensitiveGuardEnabled}
                disabled={busy}
                onChange={e => onSensitiveFileGuardChange(control.tool_name, e.target.checked, sectionScope)}
              />
              Require approval to read sensitive files
            </label>
            <div className="mt-1 text-xs text-text-tertiary">
              {sensitiveGuardExplicit ? 'Explicit policy' : 'Default on'} · routes reads of .env, ~/.ssh, ~/.aws, *.pem and similar through task scope / approval.
            </div>
          </div>
          <span className={`rounded px-2 py-0.5 text-xs ${sensitiveGuardEnabled ? 'bg-warning/15 text-warning' : 'bg-success/15 text-success'}`}>
            {sensitiveGuardEnabled ? 'guarded' : 'allowed'}
          </span>
        </div>
      )}

      {advancedRules.length > 0 && (
        <div className="mt-3 space-y-2 border-l border-border-subtle pl-3">
          {advancedRules.map(rule => (
            <AdvancedToolRuleRow
              key={rule.id}
              rule={rule}
              busy={busy}
              agentName={rule.agent_id ? agents.get(rule.agent_id)?.name : undefined}
              onEdit={(nextRule) => setInlineDraft({ ...nextRule, scope: nextRule.agent_id ? 'agent' : 'global' })}
              onToggle={onToggleAdvanced}
              onDelete={onDeleteAdvanced}
            />
          ))}
        </div>
      )}

      {inlineDraft && (
        <div className="mt-4">
          <RuleEditorCard
            key={inlineDraft.id ?? `${sectionScope}-${control.tool_name}`}
            agents={agentList}
            draft={inlineDraft}
            busy={ruleBusy}
            allowedKinds={['tool']}
            defaultAgentId={sectionScope === 'agent' ? agentId : undefined}
            agentScopeLabel={sectionScope === 'agent' ? 'This agent' : undefined}
            toolNameOptions={[control.tool_name]}
            onCancel={() => setInlineDraft(null)}
            onSave={async (draft) => {
              await onSaveRule(draft)
              setInlineDraft(null)
            }}
          />
        </div>
      )}
    </div>
  )
}

function AdvancedToolRuleRow({
  rule,
  busy,
  agentName,
  onEdit,
  onToggle,
  onDelete,
}: {
  rule: RuntimePolicyRule
  busy: boolean
  agentName?: string
  onEdit: (rule: RuntimePolicyRule) => void
  onToggle: (rule: RuntimePolicyRule) => void
  onDelete: (rule: RuntimePolicyRule) => void
}) {
  return (
    <div className="rounded border border-border-subtle bg-surface-1 px-3 py-2">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-2">
            <span className={`rounded px-2 py-0.5 text-xs font-medium ${
              rule.action === 'allow'
                ? 'bg-success/15 text-success'
                : rule.action === 'deny'
                  ? 'bg-danger/15 text-danger'
                  : 'bg-warning/15 text-warning'
            }`}>
              {toolPolicyActionLabel(rule.action)}
            </span>
            {!rule.enabled && (
              <span className="rounded bg-surface-2 px-2 py-0.5 text-xs text-text-tertiary">disabled</span>
            )}
            <span className="text-xs text-text-secondary">
              {rule.input_regex ? `when input matches ${rule.input_regex}` : 'custom input shape'}
            </span>
            <span className="text-xs text-text-tertiary">
              {agentName ?? 'All agents'}
            </span>
          </div>
          {rule.reason && <div className="mt-1 text-xs text-text-tertiary">{rule.reason}</div>}
        </div>
        <div className="flex flex-wrap gap-2">
          <button disabled={busy} onClick={() => onToggle(rule)} className="rounded border border-border-default px-2.5 py-1 text-xs text-text-secondary hover:bg-surface-2 disabled:opacity-50">
            {rule.enabled ? 'Disable' : 'Enable'}
          </button>
          <button disabled={busy} onClick={() => onEdit(rule)} className="rounded border border-border-default px-2.5 py-1 text-xs text-text-secondary hover:bg-surface-2 disabled:opacity-50">
            Edit
          </button>
          <button disabled={busy} onClick={() => onDelete(rule)} className="rounded border border-danger/20 px-2.5 py-1 text-xs text-danger hover:bg-danger/10 disabled:opacity-50">
            Delete
          </button>
        </div>
      </div>
    </div>
  )
}

function toolPolicyActionLabel(action: RuntimePolicyRule['action'] | RuntimeToolControl['action']): string {
  switch (action) {
    case 'allow':
      return 'always allow'
    case 'deny':
      return 'always deny'
    case 'unset':
      return 'unset'
    default:
      return 'review'
  }
}

const SHELL_LIKE_TOOL_NAMES = ['bash', 'shell', 'exec', 'exec_command', 'mcp__shell__exec', 'terminal']

function isShellLikeToolName(name: string): boolean {
  return SHELL_LIKE_TOOL_NAMES.includes(name.trim().toLowerCase())
}

function SegmentedToolAction({
  value,
  disabled,
  onChange,
}: {
  value: 'unset' | 'allow' | 'review' | 'deny'
  disabled?: boolean
  onChange: (action: 'unset' | 'allow' | 'deny') => void
}) {
  const normalizedValue: 'unset' | 'allow' | 'deny' = value === 'allow' || value === 'deny' ? value : 'unset'
  const options: Array<{ value: 'unset' | 'allow' | 'deny'; label: string }> = [
    { value: 'unset', label: 'Unset' },
    { value: 'allow', label: 'Always allow' },
    { value: 'deny', label: 'Always deny' },
  ]
  return (
    <div className="inline-flex rounded-md border border-border-default bg-surface-1 p-1">
      {options.map(option => {
        const active = normalizedValue === option.value
        return (
          <button
            key={option.value}
            disabled={disabled || active}
            onClick={() => {
              if (active) return
              onChange(option.value)
            }}
            className={`rounded px-3 py-1.5 text-xs font-medium transition disabled:opacity-50 ${
              active
                ? option.value === 'allow'
                  ? 'bg-success/15 text-success'
                  : option.value === 'deny'
                    ? 'bg-danger/15 text-danger'
                    : 'bg-surface-2 text-text-secondary'
                : 'text-text-tertiary hover:text-text-primary'
            }`}
          >
            {option.label}
          </button>
        )
      })}
    </div>
  )
}

function AccountControlsPanel({
  orgId,
  isLoading,
  allServices,
  activated,
  unactivated,
  restrictions,
  showAll,
  onToggleShowAll,
}: {
  orgId?: string
  isLoading: boolean
  allServices: ServiceInfo[]
  activated: ServiceInfo[]
  unactivated: ServiceInfo[]
  restrictions: (Restriction | OrgRestriction)[]
  showAll: boolean
  onToggleShowAll: () => void
}) {
  return (
    <div className="space-y-4">
      <div>
        <h2 className="text-lg font-semibold text-text-primary">Account Controls</h2>
        <p className="text-sm text-text-tertiary mt-1">
          Manage action-level restrictions for connected adapters and integrations.
        </p>
      </div>

      {isLoading && <div className="text-sm text-text-tertiary">Loading...</div>}

      {!isLoading && allServices.length === 0 && (
        <div className="text-sm text-text-tertiary py-8 text-center">
          No services registered. Add adapters in the server configuration to manage policy.
        </div>
      )}

      {!isLoading && allServices.length > 0 && activated.length === 0 && (
        <div className="text-sm text-text-tertiary py-8 text-center">
          Activate a service first to manage account controls.{' '}
          <Link to="/dashboard/accounts" className="text-brand hover:underline">Go to Accounts</Link>
        </div>
      )}

      <div className="space-y-4">
        {activated.map(svc => (
          <ServiceGroup
            key={svc.alias ? `${svc.id}:${svc.alias}` : svc.id}
            svc={svc}
            restrictions={restrictions}
            orgId={orgId}
          />
        ))}
      </div>

      {unactivated.length > 0 && (
        <div className="space-y-4">
          <button
            onClick={onToggleShowAll}
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
                  restrictions={restrictions}
                  orgId={orgId}
                />
              ))}
            </div>
          )}
        </div>
      )}
    </div>
  )
}

function ScopedRuntimeToolRules({
  globalRules,
  agentRules,
  agents,
  agentList,
  linkedAgentId,
  onAgentFilterChange,
  onNewGlobal,
  onNewAgent,
  onEdit,
  onToggle,
  onDelete,
}: {
  globalRules: RuntimePolicyRule[]
  agentRules: RuntimePolicyRule[]
  agents: Map<string, Agent>
  agentList: Agent[]
  linkedAgentId: string
  onAgentFilterChange: (agentId: string) => void
  onNewGlobal: () => void
  onNewAgent: (agentId: string) => void
  onEdit: (rule: RuleDraft) => void
  onToggle: (rule: RuntimePolicyRule) => void
  onDelete: (rule: RuntimePolicyRule) => void
}) {
  return (
    <div className="space-y-5">
      <RuntimeToolRuleList
        title="Global Tool Policies"
        subtitle="Apply to every agent."
        rules={globalRules}
        agents={agents}
        onNew={onNewGlobal}
        onEdit={onEdit}
        onToggle={onToggle}
        onDelete={onDelete}
      />
      <AgentToolRuleGroups
        rules={agentRules}
        agents={agents}
        agentList={agentList}
        linkedAgentId={linkedAgentId}
        onAgentFilterChange={onAgentFilterChange}
        onNew={onNewAgent}
        onEdit={onEdit}
        onToggle={onToggle}
        onDelete={onDelete}
      />
    </div>
  )
}

function RuntimeToolRuleList({
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
    <div className="space-y-3">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h3 className="text-sm font-semibold text-text-primary">{title}</h3>
          <p className="text-sm text-text-tertiary mt-1">{subtitle}</p>
        </div>
        <button
          onClick={onNew}
          className="rounded border border-brand/30 px-4 py-2 text-sm font-medium text-brand hover:bg-brand/10"
        >
          Add rule
        </button>
      </div>
      <div className="space-y-3">
        {rules.length === 0 && (
          <div className="rounded border border-dashed border-border-default bg-surface-0 px-4 py-6 text-sm text-text-tertiary">
            No policies yet.
          </div>
        )}
        {rules.map(rule => (
          <RuntimeToolRuleRow
            key={rule.id}
            rule={rule}
            agents={agents}
            onEdit={onEdit}
            onToggle={onToggle}
            onDelete={onDelete}
          />
        ))}
      </div>
    </div>
  )
}

function AgentToolRuleGroups({
  rules,
  agents,
  agentList,
  linkedAgentId,
  onAgentFilterChange,
  onNew,
  onEdit,
  onToggle,
  onDelete,
}: {
  rules: RuntimePolicyRule[]
  agents: Map<string, Agent>
  agentList: Agent[]
  linkedAgentId: string
  onAgentFilterChange: (agentId: string) => void
  onNew: (agentId: string) => void
  onEdit: (rule: RuleDraft) => void
  onToggle: (rule: RuntimePolicyRule) => void
  onDelete: (rule: RuntimePolicyRule) => void
}) {
  const [agentFilter, setAgentFilter] = useState(linkedAgentId || 'all')
  const rulesByAgent = useMemo(() => {
    const grouped = new Map<string, RuntimePolicyRule[]>()
    for (const rule of rules) {
      if (!rule.agent_id) continue
      const group = grouped.get(rule.agent_id) ?? []
      group.push(rule)
      grouped.set(rule.agent_id, group)
    }
    return grouped
  }, [rules])

  const sortedAgents = useMemo(
    () => [...agentList].sort((a, b) => a.name.localeCompare(b.name)),
    [agentList],
  )
  const unknownAgentIds = useMemo(
    () => [...rulesByAgent.keys()].filter(agentId => !agents.has(agentId)).sort(),
    [agents, rulesByAgent],
  )
  const visibleAgents = useMemo(
    () => agentFilter === 'all' ? sortedAgents : sortedAgents.filter(agent => agent.id === agentFilter),
    [agentFilter, sortedAgents],
  )
  const visibleUnknownAgentIds = useMemo(
    () => agentFilter === 'all' ? unknownAgentIds : unknownAgentIds.filter(agentId => agentId === agentFilter),
    [agentFilter, unknownAgentIds],
  )

  useEffect(() => {
    if (linkedAgentId && linkedAgentId !== agentFilter) {
      setAgentFilter(linkedAgentId)
    }
  }, [agentFilter, linkedAgentId])

  const handleAgentFilterChange = (nextAgentId: string) => {
    setAgentFilter(nextAgentId)
    onAgentFilterChange(nextAgentId)
  }

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h3 className="text-sm font-semibold text-text-primary">Agent Tool Policies</h3>
          <p className="text-sm text-text-tertiary mt-1">Agent-specific policies and overrides.</p>
        </div>
        <div className="flex items-center gap-2">
          <label className="text-xs text-text-tertiary">Agent</label>
          <select
            value={agentFilter}
            onChange={e => handleAgentFilterChange(e.target.value)}
            className="rounded border border-border-default bg-surface-0 px-3 py-2 text-sm text-text-primary"
          >
            <option value="all">All agents</option>
            {sortedAgents.map(agent => (
              <option key={agent.id} value={agent.id}>{agent.name}</option>
            ))}
            {unknownAgentIds.map(agentId => (
              <option key={agentId} value={agentId}>Unknown agent ({agentId})</option>
            ))}
          </select>
        </div>
      </div>

      <div className="space-y-2">
        {visibleAgents.length === 0 && visibleUnknownAgentIds.length === 0 && (
          <div className="rounded border border-dashed border-border-default bg-surface-0 px-4 py-6 text-sm text-text-tertiary">
            {agentFilter === 'all' ? 'No agents yet.' : 'No policies for this agent yet.'}
          </div>
        )}
        {visibleAgents.map(agent => (
          <AgentToolRuleGroup
            key={agent.id}
            agentId={agent.id}
            title={agent.name}
            subtitle={agent.id}
            rules={rulesByAgent.get(agent.id) ?? []}
            agents={agents}
            onNew={onNew}
            onEdit={onEdit}
            onToggle={onToggle}
            onDelete={onDelete}
          />
        ))}
        {visibleUnknownAgentIds.map(agentId => (
          <AgentToolRuleGroup
            key={agentId}
            agentId={agentId}
            title="Unknown agent"
            subtitle={agentId}
            rules={rulesByAgent.get(agentId) ?? []}
            agents={agents}
            onNew={onNew}
            onEdit={onEdit}
            onToggle={onToggle}
            onDelete={onDelete}
          />
        ))}
      </div>
    </div>
  )
}

function AgentToolRuleGroup({
  agentId,
  title,
  subtitle,
  rules,
  agents,
  onNew,
  onEdit,
  onToggle,
  onDelete,
}: {
  agentId: string
  title: string
  subtitle: string
  rules: RuntimePolicyRule[]
  agents: Map<string, Agent>
  onNew: (agentId: string) => void
  onEdit: (rule: RuleDraft) => void
  onToggle: (rule: RuntimePolicyRule) => void
  onDelete: (rule: RuntimePolicyRule) => void
}) {
  const [open, setOpen] = useState(rules.length > 0)

  useEffect(() => {
    if (rules.length > 0) setOpen(true)
  }, [rules.length])

  return (
    <div className="rounded border border-border-subtle bg-surface-0">
      <div className="flex flex-wrap items-center justify-between gap-3 px-4 py-3">
        <button
          type="button"
          aria-expanded={open}
          onClick={() => setOpen(value => !value)}
          className="min-w-0 flex-1 text-left"
        >
          <h4 className="truncate text-sm font-medium text-text-primary">{title}</h4>
          <p className="truncate text-xs text-text-tertiary">{subtitle}</p>
        </button>
        <div className="flex shrink-0 items-center gap-2">
          <span className="rounded-full bg-surface-1 px-2 py-0.5 text-xs text-text-tertiary">
            {rules.length} {rules.length === 1 ? 'policy' : 'policies'}
          </span>
          <button
            type="button"
            onClick={() => setOpen(value => !value)}
            className="rounded border border-border-default px-3 py-1.5 text-xs text-text-secondary hover:bg-surface-2"
          >
            {open ? 'Hide' : 'Show'}
          </button>
          <button
            onClick={() => onNew(agentId)}
            className="rounded border border-brand/30 px-3 py-1.5 text-xs font-medium text-brand hover:bg-brand/10"
          >
            Add rule
          </button>
        </div>
      </div>
      {open && (
        <div className="space-y-3 border-t border-border-subtle p-4">
          {rules.length === 0 && (
            <div className="rounded border border-dashed border-border-default px-4 py-5 text-sm text-text-tertiary">
              No policies yet.
            </div>
          )}
          {rules.map(rule => (
            <RuntimeToolRuleRow
              key={rule.id}
              rule={rule}
              agents={agents}
              onEdit={onEdit}
              onToggle={onToggle}
              onDelete={onDelete}
            />
          ))}
        </div>
      )}
    </div>
  )
}

function RuntimeToolRuleRow({
  rule,
  agents,
  onEdit,
  onToggle,
  onDelete,
}: {
  rule: RuntimePolicyRule
  agents: Map<string, Agent>
  onEdit: (rule: RuleDraft) => void
  onToggle: (rule: RuntimePolicyRule) => void
  onDelete: (rule: RuntimePolicyRule) => void
}) {
  return (
    <div className="rounded border border-border-subtle bg-surface-0 p-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <div className="flex flex-wrap items-center gap-2">
            <span className={`rounded px-2 py-0.5 text-xs font-mono ${
              rule.action === 'allow' ? 'bg-success/15 text-success' :
              rule.action === 'deny' ? 'bg-danger/15 text-danger' :
              'bg-warning/15 text-warning'
            }`}>
              {toolPolicyActionLabel(rule.action)}
            </span>
            <span className="text-sm font-medium text-text-primary">{rule.tool_name}</span>
            <span className="text-xs text-text-tertiary">
              {rule.agent_id ? (agents.get(rule.agent_id)?.name ?? 'Agent scoped') : 'All agents'}
            </span>
          </div>
          <div className="mt-1 text-xs text-text-tertiary">
            source: {rule.source} {rule.last_matched_at ? `· last matched ${new Date(rule.last_matched_at).toLocaleString()}` : ''}
          </div>
          {rule.input_regex && <div className="mt-1 text-xs text-text-tertiary">input matches {rule.input_regex}</div>}
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
  )
}

function PolicyRulesPanel({
  orgId,
  rulesTab,
  onTabChange,
  serviceRuleCount,
  egressRuleCount,
  toolRuleCount,
  runtimePolicyUI,
  isLoading,
  allServices,
  activated,
  unactivated,
  restrictions,
  showAll,
  onToggleShowAll,
  agents,
  agentList,
  linkedAgentId,
  onAgentFilterChange,
  egressRules,
  toolRules,
  onNewRule,
  onEditRule,
  onToggleRule,
  onDeleteRule,
}: {
  orgId?: string
  rulesTab: 'service' | 'egress' | 'tool'
  onTabChange: (tab: 'service' | 'egress' | 'tool') => void
  serviceRuleCount: number
  egressRuleCount: number
  toolRuleCount: number
  runtimePolicyUI: boolean
  isLoading: boolean
  allServices: ServiceInfo[]
  activated: ServiceInfo[]
  unactivated: ServiceInfo[]
  restrictions: (Restriction | OrgRestriction)[]
  showAll: boolean
  onToggleShowAll: () => void
  agents: Map<string, Agent>
  agentList: Agent[]
  linkedAgentId: string
  onAgentFilterChange: (agentId: string) => void
  egressRules: RuntimePolicyRule[]
  toolRules: RuntimePolicyRule[]
  onNewRule: (kind: 'egress' | 'tool', patch?: Partial<RuleDraft>) => void
  onEditRule: (rule: RuleDraft) => void
  onToggleRule: (rule: RuntimePolicyRule) => void
  onDeleteRule: (rule: RuntimePolicyRule) => void
}) {
  const tabs: Array<{ id: 'service' | 'egress' | 'tool'; label: string; count: number }> = [
    { id: 'service', label: 'Service', count: serviceRuleCount },
    ...(runtimePolicyUI
      ? [
          { id: 'egress' as const, label: 'Egress', count: egressRuleCount },
          { id: 'tool' as const, label: 'Tool', count: toolRuleCount },
        ]
      : []),
  ]

  return (
    <section className="rounded-md border border-border-default bg-surface-1 p-5 space-y-4">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h2 className="text-lg font-semibold text-text-primary">Rules</h2>
          <p className="text-sm text-text-tertiary mt-1">
            {runtimePolicyUI
              ? 'Switch between service, egress, and tool policy without bouncing across separate sections.'
              : 'Manage service-level policy for connected adapters and integrations.'}
          </p>
        </div>
        <div className="inline-flex rounded-lg border border-border-default bg-surface-0 p-1">
          {tabs.map(tab => {
            const active = rulesTab === tab.id
            return (
              <button
                key={tab.id}
                onClick={() => onTabChange(tab.id)}
                className={`inline-flex items-center gap-2 rounded-md px-3 py-2 text-sm transition ${
                  active
                    ? 'bg-surface-1 text-text-primary shadow-sm'
                    : 'text-text-tertiary hover:text-text-primary'
                }`}
              >
                <span>{tab.label}</span>
                <span className={`rounded-full px-2 py-0.5 text-xs ${active ? 'bg-border-subtle text-text-primary' : 'bg-surface-1 text-text-tertiary'}`}>
                  {tab.count}
                </span>
              </button>
            )
          })}
        </div>
      </div>

      {rulesTab === 'service' && (
        <AccountControlsPanel
          orgId={orgId}
          isLoading={isLoading}
          allServices={allServices}
          activated={activated}
          unactivated={unactivated}
          restrictions={restrictions}
          showAll={showAll}
          onToggleShowAll={onToggleShowAll}
        />
      )}

      {rulesTab === 'egress' && !orgId && (
        <RuleSection
          title="Runtime Egress Rules"
          subtitle="Fast-path controls for background and harness HTTP activity before it falls through to review logic."
          rules={egressRules}
          agents={agents}
          onNew={() => onNewRule('egress')}
          onEdit={onEditRule}
          onToggle={onToggleRule}
          onDelete={onDeleteRule}
        />
      )}

      {rulesTab === 'tool' && !orgId && (
        <ScopedRuntimeToolRules
          globalRules={toolRules.filter(rule => !rule.agent_id)}
          agentRules={toolRules.filter(rule => rule.agent_id)}
          agents={agents}
          agentList={agentList}
          linkedAgentId={linkedAgentId}
          onAgentFilterChange={onAgentFilterChange}
          onNewGlobal={() => onNewRule('tool', { scope: 'global', agent_id: undefined })}
          onNewAgent={(agentId) => onNewRule('tool', { scope: 'agent', agent_id: agentId })}
          onEdit={onEditRule}
          onToggle={onToggleRule}
          onDelete={onDeleteRule}
        />
      )}
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
