import { useCallback, useEffect, useMemo, useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { useSearchParams } from 'react-router-dom'
import { api, type Agent, type Restriction, type OrgRestriction } from '../api/client'
import { useAuth } from '../hooks/useAuth'
import './library.css'
import './policy.css'
import AccountControlsPanel from '../components/policy/AccountControlsPanel'
import PolicyTopTabs, { type PolicyTopTab } from '../components/policy/PolicyTopTabs'
import ServicePresetsPanel from '../components/policy/ServicePresetsPanel'
import ToolControlsPanel from '../components/policy/ToolControlsPanel'
import { mergeToolControlsWithDefaults, TOOL_CONTROL_DEFAULTS } from '../lib/toolControlDefaults'
import {
  type RuleDraft,
  isActiveRuntimeSession,
  RuntimeStatusPanel,
  StarterProfilesPanel,
  RuleEditorCard,
  emptyEgressRule,
  emptyToolRule,
} from './Runtime'

export default function Policy() {
  const { currentOrg, features } = useAuth()
  const orgId = currentOrg?.id
  const qc = useQueryClient()
  const [searchParams, setSearchParams] = useSearchParams()
  const linkedAgentId = searchParams.get('agent_id') ?? ''
  const personalWorkspace = !orgId
  const proxyLiteFeature = personalWorkspace && !!features?.proxy_lite
  const runtimePolicyUI = personalWorkspace && !!features?.runtime_policy_ui
  const servicePresetsUI = personalWorkspace && !!features?.service_presets
  const [agentFilter, setAgentFilter] = useState<string>(() => linkedAgentId || 'all')
  const [editingRule, setEditingRule] = useState<RuleDraft | null>(null)
  const tabParam = searchParams.get('tab')
  const [topTab, setTopTab] = useState<PolicyTopTab>(() => (tabParam === 'accounts' ? 'accounts' : 'tools'))

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
    enabled: personalWorkspace,
  })
  const { data: status } = useQuery({
    queryKey: ['runtime-status'],
    queryFn: () => api.runtime.status(),
    enabled: personalWorkspace && (proxyLiteFeature || runtimePolicyUI),
  })
  const proxyLiteOnly = (proxyLiteFeature || (runtimePolicyUI && !!status?.proxy_lite_enabled)) && !status?.enabled
  const fullProxyActive = runtimePolicyUI && !!status?.enabled
  const showLiteToolControls = proxyLiteOnly || (proxyLiteFeature && !fullProxyActive)
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
  const toolControlsEnabled = personalWorkspace && agentFilter !== 'all' && (showLiteToolControls || fullProxyActive)
  const { data: toolControls } = useQuery({
    queryKey: ['runtime-tool-controls', agentFilter],
    queryFn: () => api.runtime.listToolControls(agentFilter),
    enabled: toolControlsEnabled,
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

  const setTopTabWithUrl = useCallback((tab: PolicyTopTab) => {
    setTopTab(tab)
    setSearchParams(current => {
      const next = new URLSearchParams(current)
      if (tab === 'accounts') next.set('tab', 'accounts')
      else next.delete('tab')
      return next
    }, { replace: true })
  }, [setSearchParams])

  useEffect(() => {
    const nextTab: PolicyTopTab = tabParam === 'accounts' ? 'accounts' : 'tools'
    if (topTab !== nextTab) setTopTab(nextTab)
  }, [tabParam, topTab])

  useEffect(() => {
    if (linkedAgentId && linkedAgentId !== agentFilter) {
      setAgentFilter(linkedAgentId)
    }
  }, [agentFilter, linkedAgentId])

  // Deep-linking with ?agent_id= opens Tool Controls, but never override an
  // explicit Account Controls tab (including when agents finish loading later).
  useEffect(() => {
    if (!linkedAgentId || !personalWorkspace) return
    if (tabParam === 'accounts') return
    setTopTab('tools')
  }, [linkedAgentId, personalWorkspace, tabParam])

  useEffect(() => {
    if (!personalWorkspace || agents.length === 0) return
    if (agentFilter !== 'all' && agents.some(agent => agent.id === agentFilter)) return
    const linkedAgent = linkedAgentId && agents.some(agent => agent.id === linkedAgentId) ? linkedAgentId : ''
    setPolicyAgentFilter(linkedAgent || agents[0].id)
  }, [agentFilter, agents, linkedAgentId, personalWorkspace, setPolicyAgentFilter])

  const toolVariant: 'lite' | 'full' | 'none' = fullProxyActive
    ? 'full'
    : personalWorkspace
      ? 'lite'
      : showLiteToolControls
        ? 'lite'
        : 'none'

  const toolCount = useMemo(() => {
    if (toolVariant === 'lite') {
      if (agentFilter !== 'all') {
        return mergeToolControlsWithDefaults(agentFilter, toolControls?.entries ?? []).length
      }
      return TOOL_CONTROL_DEFAULTS.length
    }
    if (fullProxyActive && agentFilter !== 'all') {
      return mergeToolControlsWithDefaults(agentFilter, toolControls?.entries ?? []).length
    }
    if (fullProxyActive && toolRules?.entries?.length) {
      return new Set(toolRules.entries.map(rule => rule.tool_name).filter(Boolean)).size
    }
    return 0
  }, [agentFilter, fullProxyActive, toolControls?.entries, toolRules?.entries, toolVariant])

  const accountCount = activated.length

  const policyDescription = personalWorkspace
    ? 'Configure harness tool access and connected account restrictions.'
    : runtimePolicyUI || servicePresetsUI
      ? 'Configure presets, runtime rules, defaults, and service restrictions from one place.'
      : 'Manage action-level restrictions for connected adapters and integrations.'

  return (
    <div className="lib-page">
      <header className="lib-hero">
        <h1 className="page-title">Policy</h1>
        <p className="page-desc">{policyDescription}</p>
      </header>

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

      {(fullProxyActive || toolVariant === 'lite') && editingRule && (
        <RuleEditorCard
          key={editingRule.id ?? `${editingRule.kind}-${editingRule.action}-${editingRule.host ?? editingRule.tool_name ?? 'new'}`}
          agents={agents}
          draft={editingRule}
          busy={createRuleMut.isPending || updateRuleMut.isPending}
          allowedKinds={toolVariant === 'lite' && !fullProxyActive ? ['tool'] : undefined}
          defaultAgentId={toolVariant === 'lite' && agentFilter !== 'all' ? agentFilter : undefined}
          toolNameOptions={toolVariant === 'lite' ? mergeToolControlsWithDefaults(agentFilter !== 'all' ? agentFilter : agents[0]?.id ?? '', toolControls?.entries ?? []).map(control => control.tool_name) : undefined}
          onCancel={() => setEditingRule(null)}
          onSave={(draft) => {
            if (draft.id) updateRuleMut.mutate(draft)
            else createRuleMut.mutate(draft)
          }}
        />
      )}

      <div className="policy-shell">
        <PolicyTopTabs
          activeTab={topTab}
          onTabChange={setTopTabWithUrl}
          toolCount={toolCount}
          accountCount={accountCount}
        />

        <div className="policy-shell-body">
        {topTab === 'tools' ? (
          <ToolControlsPanel
            variant={toolVariant}
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
            globalRules={(toolRules?.entries ?? []).filter(rule => !rule.agent_id)}
            agentRules={(toolRules?.entries ?? []).filter(rule => rule.agent_id)}
            linkedAgentId={linkedAgentId}
            egressRules={egressRules?.entries ?? []}
            onNewRule={startCreateRule}
            onEditRule={setEditingRule}
            onToggleRule={(rule) => updateRuleMut.mutate({ ...rule, scope: rule.agent_id ? 'agent' : 'global', enabled: !rule.enabled })}
            onDeleteRule={(rule) => deleteRuleMut.mutate(rule.id)}
          />
        ) : (
          <AccountControlsPanel
            isLoading={isLoading}
            allServices={allServices}
            activated={activated}
            unactivated={unactivated}
            restrictions={allRestrictions}
          />
        )}
        </div>
      </div>
    </div>
  )
}
