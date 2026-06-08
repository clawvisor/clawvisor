import { useState } from 'react'
import { useMutation } from '@tanstack/react-query'
import { api, type Agent } from '../../api/client'
import { type RuleDraft } from '../../pages/Runtime'

export default function ServicePresetsPanel({
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
