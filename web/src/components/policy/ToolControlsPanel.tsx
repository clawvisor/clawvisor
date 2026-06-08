import type { Agent, RuntimePolicyRule, RuntimeToolControl } from '../../api/client'
import { filterGlobalToolControls, mergeToolControlsWithDefaults } from '../../lib/toolControlDefaults'

/** Synthetic agent id for rendering default policies before any agent exists. */
const POLICY_PREVIEW_AGENT_ID = '__policy_preview__'
import type { RuleDraft } from '../../pages/Runtime'
import NetworkEgressSection from './NetworkEgressSection'
import ScopedRuntimeToolRules from './ScopedRuntimeToolRules'
import ToolControlListSection from './ToolControlListSection'

export default function ToolControlsPanel({
  variant,
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
  globalRules,
  agentRules,
  linkedAgentId,
  egressRules,
  onNewRule,
  onEditRule,
  onToggleRule,
  onDeleteRule,
}: {
  variant: 'lite' | 'full' | 'none'
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
  globalRules: RuntimePolicyRule[]
  agentRules: RuntimePolicyRule[]
  linkedAgentId: string
  egressRules: RuntimePolicyRule[]
  onNewRule: (kind: 'egress' | 'tool', patch?: Partial<RuleDraft>) => void
  onEditRule: (rule: RuleDraft) => void
  onToggleRule: (rule: RuntimePolicyRule) => void
  onDeleteRule: (rule: RuntimePolicyRule) => void
}) {
  if (variant === 'none') {
    return (
      <div className="rounded border border-dashed border-border-default bg-surface-0 px-4 py-6 text-sm text-text-tertiary">
        Harness tool policies appear here after your first agent activity. Use Account Controls to manage connected service restrictions.
      </div>
    )
  }

  const useToolControlsUI = variant === 'lite' || (variant === 'full' && agentId !== 'all')

  if (!useToolControlsUI && variant === 'full') {
    return (
      <div className="space-y-5">
        <ScopedRuntimeToolRules
          globalRules={globalRules}
          agentRules={agentRules}
          agents={agents}
          agentList={agentList}
          linkedAgentId={linkedAgentId}
          onAgentFilterChange={onAgentChange}
          onNewGlobal={() => onNewRule('tool', { scope: 'global', agent_id: undefined })}
          onNewAgent={(nextAgentId) => onNewRule('tool', { scope: 'agent', agent_id: nextAgentId })}
          onEdit={onEditRule}
          onToggle={onToggleRule}
          onDelete={onDeleteRule}
        />
        <NetworkEgressSection
          egressRules={egressRules}
          agents={agents}
          onNew={() => onNewRule('egress')}
          onEdit={onEditRule}
          onToggle={onToggleRule}
          onDelete={onDeleteRule}
        />
      </div>
    )
  }

  const previewAgentId = agentId !== 'all' ? agentId : (agentList[0]?.id ?? POLICY_PREVIEW_AGENT_ID)
  const mergedControls = mergeToolControlsWithDefaults(previewAgentId, controls)
  const globalControls = filterGlobalToolControls(mergedControls)
  const agentControlsReady = agentId !== 'all'
  const agentSelector = (
    <div className="policy-agent-select">
      <label htmlFor="policy-agent-filter">Agent</label>
      <select
        id="policy-agent-filter"
        value={agentId}
        onChange={e => onAgentChange(e.target.value)}
      >
        {agentList.map(agent => (
          <option key={agent.id} value={agent.id}>{agent.name}</option>
        ))}
      </select>
    </div>
  )
  const panel = (
    <div className="space-y-5">
      <div className="policy-panel-heading">
        <h2>Tool Controls</h2>
        <p>Tools are detected from the harness request body and from recent tool calls.</p>
      </div>

      <ToolControlListSection
        title="Global Tool Policies"
        subtitle="Apply to every agent."
        sectionScope="global"
        controls={globalControls}
        agents={agents}
        agentList={agentList}
        agentId={previewAgentId}
        busy={busy || !agentControlsReady || previewAgentId === POLICY_PREVIEW_AGENT_ID}
        ruleBusy={ruleBusy}
        onChange={onChange}
        onReadOnlyCommandsChange={onReadOnlyCommandsChange}
        onSensitiveFileGuardChange={onSensitiveFileGuardChange}
        onSaveRule={onSaveRule}
        onToggleAdvanced={onToggleAdvanced}
        onDeleteAdvanced={onDeleteAdvanced}
      />

      {agentControlsReady ? (
        <ToolControlListSection
          title="Agent Tool Policies"
          subtitle="Selected-agent overrides and tools that are governed by task scopes."
          sectionScope="agent"
          headerControl={agentSelector}
          controls={mergedControls}
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
      ) : (
        <div className="policy-section">
          <div className="flex flex-wrap items-center justify-between gap-3">
            <div className="policy-section-heading">
              <h3>Agent Tool Policies</h3>
              <p>Selected-agent overrides and tools that are governed by task scopes.</p>
            </div>
            {agentList.length > 0 && agentSelector}
          </div>
          <div className="rounded border border-dashed border-border-default bg-surface-0 px-4 py-6 text-sm text-text-tertiary">
            {agentList.length > 0 ? 'Select an agent to configure per-agent overrides.' : 'Connect an agent to configure per-agent tool policies.'}
          </div>
        </div>
      )}
    </div>
  )

  if (variant === 'full') {
    return (
      <div className="space-y-5">
        {panel}
        <NetworkEgressSection
          egressRules={egressRules}
          agents={agents}
          onNew={() => onNewRule('egress')}
          onEdit={onEditRule}
          onToggle={onToggleRule}
          onDelete={onDeleteRule}
        />
      </div>
    )
  }

  return panel
}
