import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '../api/client'
import { type LLMProvider } from '../utils/llmCredentials'

export default function VaultKeyStep({
  agentId,
  provider,
  title = 'Vault upstream key',
  description,
}: {
  agentId?: string
  provider?: LLMProvider
  title?: string
  description?: string
}) {
  const qc = useQueryClient()
  const [editingProvider, setEditingProvider] = useState<string | null>(null)
  const [apiKey, setApiKey] = useState('')
  const [error, setError] = useState<string | null>(null)

  const { data: creds } = useQuery({
    queryKey: ['llm-credentials', agentId ?? 'user'],
    queryFn: () => api.llmCredentials.list(agentId),
  })

  const setMut = useMutation({
    mutationFn: (params: { provider: string; key: string }) =>
      api.llmCredentials.set(params.provider, params.key, agentId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['llm-credentials', agentId ?? 'user'] })
      qc.invalidateQueries({ queryKey: ['llm-credentials', 'user'] })
      setEditingProvider(null)
      setApiKey('')
      setError(null)
    },
    onError: (err: Error) => setError(err.message),
  })
  const visibleCreds = provider
    ? creds?.credentials.filter(c => c.provider === provider)
    : creds?.credentials

  return (
    <div className="space-y-3">
      <div>
        <p className="text-sm font-medium text-text-primary">{title}</p>
        {description ? (
          <p className="text-sm text-text-secondary mt-1">{description}</p>
        ) : (
          <p className="text-sm text-text-secondary mt-1">
            Clawvisor swaps your <code className="font-mono">cvis_…</code> token for an upstream
            Anthropic or OpenAI key on each call. Vault at least one key — either now
            {agentId ? ' for this agent' : ' as a user-level key'}
            {' '}or globally on the <a href="/dashboard/credentials" className="text-brand hover:underline">Credentials</a> page.
          </p>
        )}
      </div>

      {error && <p className="text-xs text-danger">{error}</p>}

      {visibleCreds?.map(c => (
        <div key={c.provider} className="rounded border border-border-default bg-surface-1 p-3 space-y-2">
          <div className="flex items-center justify-between">
            <div>
              <div className="text-sm font-medium text-text-primary capitalize">{c.provider}</div>
              <div className="text-xs text-text-tertiary mt-0.5">
                {c.agent_stored ? (
                  <span className="text-success">Agent-scoped key set</span>
                ) : c.stored ? (
                  <span className="text-success">Using user-level key</span>
                ) : (
                  <span className="text-warning">No key configured</span>
                )}
              </div>
            </div>
            <button
              type="button"
              onClick={() => { setEditingProvider(c.provider); setApiKey(''); setError(null) }}
              className="text-xs px-3 py-1 rounded border border-brand/30 text-brand hover:bg-brand/10"
            >
              {c.agent_stored ? 'Replace' : c.stored ? (agentId ? 'Override for this agent' : 'Replace') : 'Set key'}
            </button>
          </div>
          {editingProvider === c.provider && (
            <div className="space-y-2 pt-2 border-t border-border-subtle">
              <input
                type="password"
                value={apiKey}
                onChange={e => { setApiKey(e.target.value); setError(null) }}
                placeholder={c.provider === 'anthropic' ? 'sk-ant-...' : 'sk-...'}
                className="block w-full text-sm rounded border border-border-default bg-surface-0 text-text-primary px-3 py-1.5 focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand placeholder:text-text-tertiary"
              />
              <div className="flex items-center gap-2">
                <button
                  type="button"
                  onClick={() => { if (!apiKey) { setError('API key is required'); return } setMut.mutate({ provider: c.provider, key: apiKey }) }}
                  disabled={setMut.isPending || !apiKey}
                  className="px-3 py-1 text-xs rounded bg-brand text-surface-0 hover:bg-brand-strong disabled:opacity-50"
                >
                  {setMut.isPending ? 'Saving…' : 'Save'}
                </button>
                <button
                  type="button"
                  onClick={() => { setEditingProvider(null); setApiKey(''); setError(null) }}
                  className="text-xs text-text-tertiary hover:text-text-primary"
                >
                  Cancel
                </button>
              </div>
            </div>
          )}
        </div>
      ))}
    </div>
  )
}
