import { useEffect, useMemo, useState } from 'react'
import type { Agent, RuntimePolicyRule } from '../../api/client'
import type { RuleDraft } from '../../pages/Runtime'
import { toolPolicyActionLabel } from './policyUtils'

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
    <div className="policy-section">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h3 className="text-sm font-semibold text-text-primary">Agent Tool Policies</h3>
          <p className="text-sm text-text-tertiary mt-1">Selected-agent overrides and tools that are governed by task scopes.</p>
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
    <div className="policy-section">
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

export default function ScopedRuntimeToolRules({
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
