import type { Agent, RuntimePolicyRule } from '../../api/client'
import { RuleSection, type RuleDraft } from '../../pages/Runtime'

export default function NetworkEgressSection({
  egressRules,
  agents,
  onNew,
  onEdit,
  onToggle,
  onDelete,
}: {
  egressRules: RuntimePolicyRule[]
  agents: Map<string, Agent>
  onNew: () => void
  onEdit: (rule: RuleDraft) => void
  onToggle: (rule: RuntimePolicyRule) => void
  onDelete: (rule: RuntimePolicyRule) => void
}) {
  return (
    <details className="policy-advanced-section policy-section rounded border border-border-subtle bg-surface-0">
      <summary className="flex items-center justify-between gap-3 px-4 py-3 text-sm font-medium text-text-primary hover:bg-surface-1">
        Network egress (advanced)
        <span className="text-xs font-normal text-text-tertiary">{egressRules.length} rules</span>
      </summary>
      <div className="border-t border-border-subtle p-4">
        <RuleSection
          title="Runtime Egress Rules"
          subtitle="Fast-path controls for background and harness HTTP activity before it falls through to review logic."
          rules={egressRules}
          agents={agents}
          onNew={onNew}
          onEdit={onEdit}
          onToggle={onToggle}
          onDelete={onDelete}
        />
      </div>
    </details>
  )
}
