import { useState } from 'react'
import type { Agent, RuntimePolicyRule, RuntimeToolControl } from '../../api/client'
import { emptyToolRule, RuleEditorCard, type RuleDraft } from '../../pages/Runtime'
import { isDefaultToolControlName } from '../../lib/toolControlDefaults'
import { isShellLikeToolName, toolPolicyActionLabel } from './policyUtils'
import SegmentedToolAction from './SegmentedToolAction'

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

export default function ToolControlRow({
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
  const showSimpleControl = sectionScope === 'agent'
    || isDefaultToolControlName(control.tool_name)
    || shellLike
    || guardApplies
    || !!control.global_rule_id
    || advancedRules.length > 0
  if (!showSimpleControl && advancedRules.length === 0) return null
  const scopeLabel = sectionScope === 'global'
    ? (control.global_rule_id || control.global_action === 'allow' || control.global_action === 'deny')
      ? 'Global policy'
      : 'No global policy'
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
  const defaultOnLabel = (explicit: boolean) => explicit ? 'Explicit policy' : 'Default on'

  return (
    <div className="policy-tool-block">
      <div className="policy-tool-header">
        <div>
          <div className="flex flex-wrap items-center gap-2">
            <span className="policy-tool-name">{control.tool_name}</span>
            <span className="policy-tool-scope">{scopeLabel}</span>
            {advancedRules.length > 0 && (
              <span className="rounded bg-brand/10 px-2 py-0.5 text-xs text-brand">
                {advancedRules.length} advanced
              </span>
            )}
          </div>
          <div className="policy-tool-meta">
            {control.last_seen_at ? `Last seen ${new Date(control.last_seen_at).toLocaleString()}` : 'Not seen yet'}
          </div>
        </div>
        {showSimpleControl && (
          <div className="policy-tool-actions">
            <SegmentedToolAction value={action} disabled={busy} onChange={nextAction => onChange(control.tool_name, nextAction, sectionScope)} />
            <button
              type="button"
              onClick={() => setInlineDraft({ ...emptyToolRule(), scope: sectionScope, agent_id: sectionScope === 'agent' ? agentId : undefined, tool_name: control.tool_name })}
              className="policy-create-rule-btn"
            >
              Create rule
            </button>
          </div>
        )}
      </div>

      {showSimpleControl && (shellLike || guardApplies) && (
        <div className="policy-tool-rules">
          {shellLike && (
            <div className="policy-tool-rule">
              <div className="min-w-0 flex-1">
                <label className="policy-tool-rule-label">
                  <input
                    type="checkbox"
                    checked={readOnlyCommandsAllowed}
                    disabled={busy}
                    onChange={e => onReadOnlyCommandsChange(control.tool_name, e.target.checked, sectionScope)}
                  />
                  <span>Allow read-only commands</span>
                </label>
                <p className="policy-tool-rule-hint">
                  {defaultOnLabel(readOnlyCommandsExplicit)} - applies to commands like ls, cat, grep, rg, find, wc, and pwd.
                </p>
              </div>
              <span className={`policy-status-badge ${readOnlyCommandsAllowed ? 'policy-status-badge--allowed' : 'policy-status-badge--reviewed'}`}>
                {readOnlyCommandsAllowed ? 'allowed' : 'reviewed'}
              </span>
            </div>
          )}

          {guardApplies && (
            <div className="policy-tool-rule">
              <div className="min-w-0 flex-1">
                <label className="policy-tool-rule-label">
                  <input
                    type="checkbox"
                    checked={sensitiveGuardEnabled}
                    disabled={busy}
                    onChange={e => onSensitiveFileGuardChange(control.tool_name, e.target.checked, sectionScope)}
                  />
                  <span>Require approval to read sensitive files</span>
                </label>
                <p className="policy-tool-rule-hint">
                  {defaultOnLabel(sensitiveGuardExplicit)} - routes reads of .env, ~/.ssh, ~/.aws, *.pem and similar through task scope / approval.
                </p>
              </div>
              <span className={`policy-status-badge ${sensitiveGuardEnabled ? 'policy-status-badge--guarded' : 'policy-status-badge--allowed'}`}>
                {sensitiveGuardEnabled ? 'guarded' : 'allowed'}
              </span>
            </div>
          )}
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
