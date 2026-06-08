import type { ReactNode } from 'react'
import type { Agent, RuntimePolicyRule, RuntimeToolControl } from '../../api/client'
import type { RuleDraft } from '../../pages/Runtime'
import ToolControlRow from './ToolControlRow'

export default function ToolControlListSection({
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
    <div className="policy-section">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="policy-section-heading">
          <h3>{title}</h3>
          <p>{subtitle}</p>
        </div>
        {headerControl}
      </div>
      {controls.length === 0 ? (
        <div className="rounded border border-dashed border-border-default bg-surface-0 px-4 py-6 text-sm text-text-tertiary">
          No policies yet.
        </div>
      ) : (
        <div className="policy-tool-list">
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
