import { useEffect, useMemo, useState } from 'react'
import { Link, useParams, useSearchParams } from 'react-router-dom'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '../api/client'

// KeyVault is a focused, deep-link page for vaulting ONE upstream LLM API
// key (Anthropic or OpenAI). The one-paste install skill links here when
// $ANTHROPIC_API_KEY / $OPENAI_API_KEY isn't set in the user's shell — the
// agent prints the URL, the user opens it, pastes the key into a single
// textbox, and the skill (polling /api/runtime/llm-credentials) detects the
// save and resumes setup.
//
// Strictly deep-link — not in the nav. The skill is the primary referrer;
// users may also land here from Agent settings.
//
// Per design: defaults to user-scope (applies to every agent). When
// `?for=<agent-id>` is present, the agent-scope toggle becomes visible
// so the user can override per-agent if they want. The value of `for`
// is the agent's UUID (the API-stable identifier); we resolve it to a
// friendly name via the agents list for display.

type ProviderSpec = {
  id: 'anthropic' | 'openai'
  label: string
  keyPrefix: string
  consoleURL: string
  // Plain-language description of where the key swap happens. Same goal as
  // the install-skill copy: explain that the agent never holds this key.
  description: string
}

const PROVIDER_SPECS: Record<string, ProviderSpec> = {
  anthropic: {
    id: 'anthropic',
    label: 'Anthropic',
    keyPrefix: 'sk-ant-',
    consoleURL: 'https://console.anthropic.com/settings/keys',
    description:
      'Clawvisor swaps the agent token for this key when forwarding Anthropic Messages API calls upstream. Agents see only the cvis_… token — never this key.',
  },
  openai: {
    id: 'openai',
    label: 'OpenAI',
    keyPrefix: 'sk-',
    consoleURL: 'https://platform.openai.com/api-keys',
    description:
      'Clawvisor swaps the agent token for this key when forwarding OpenAI Chat Completions and Responses API calls upstream. Agents see only the cvis_… token — never this key.',
  },
}

export default function KeyVault() {
  const { provider: providerParam } = useParams<{ provider: string }>()
  const [searchParams] = useSearchParams()
  const qc = useQueryClient()

  const spec = providerParam ? PROVIDER_SPECS[providerParam.toLowerCase()] : undefined
  // forAgentId is the UUID the install skill passes via ?for=… so the
  // per-agent vault write hits a valid agent_id (the server's ownership
  // check rejects anything else). Display label is resolved from the
  // agents list below.
  const forAgentId = searchParams.get('for') || ''

  // Resolve a friendly name for the for-agent UUID. This is a deep link
  // from the install skill, so we only fetch the agents list when needed
  // for the scope label. If the lookup misses (agent was deleted, list
  // is loading, etc.) we render a short prefix of the UUID instead so
  // the label is never empty.
  const { data: agents } = useQuery({
    queryKey: ['agents', 'for-keyvault'],
    queryFn: () => api.agents.list(),
    enabled: !!forAgentId,
  })
  const forAgentLabel = useMemo(() => {
    if (!forAgentId) return ''
    const match = agents?.find(a => a.id === forAgentId)
    return match?.name ?? `${forAgentId.slice(0, 8)}…`
  }, [agents, forAgentId])

  // Scope only matters when ?for=<agent-id> is in the URL — without an
  // agent hint, the page silently behaves as user-scope (the dominant case).
  const [scope, setScope] = useState<'user' | 'agent'>('user')
  const [keyInput, setKeyInput] = useState('')
  const [showKey, setShowKey] = useState(false)

  // Fetch the current vault state so we can show "already saved" when the
  // user lands here with a key already in place — and so the install skill,
  // which polls the same endpoint, sees identical truth.
  const { data: creds } = useQuery({
    queryKey: ['llm-credentials', scope === 'agent' && forAgentId ? forAgentId : 'user'],
    queryFn: () => api.llmCredentials.list(scope === 'agent' && forAgentId ? forAgentId : undefined),
    enabled: !!spec,
  })

  const isStored = useMemo(() => {
    if (!spec || !creds) return false
    const row = creds.credentials.find(c => c.provider === spec.id)
    if (!row) return false
    return scope === 'agent' ? !!row.agent_stored : !!row.stored
  }, [creds, scope, spec])

  const setMutation = useMutation({
    mutationFn: async () => {
      if (!spec) throw new Error('unknown provider')
      const trimmed = keyInput.trim()
      if (!trimmed) throw new Error('Enter your API key first.')
      return api.llmCredentials.set(spec.id, trimmed, scope === 'agent' && forAgentId ? forAgentId : undefined)
    },
    onSuccess: () => {
      setKeyInput('')
      qc.invalidateQueries({ queryKey: ['llm-credentials'] })
    },
  })

  // Reset the success/error UI when the user changes scope so the message
  // shown reflects the action they're about to take, not the prior one.
  useEffect(() => {
    setMutation.reset()
  }, [scope]) // eslint-disable-line react-hooks/exhaustive-deps

  if (!spec) {
    return (
      <div className="min-h-screen flex items-center justify-center bg-surface-0 px-4">
        <div className="max-w-md w-full rounded-md border border-border-default bg-surface-1 px-6 py-8 space-y-3">
          <h1 className="text-lg font-semibold text-text-primary">Unknown provider</h1>
          <p className="text-sm text-text-secondary">
            This page accepts <code className="font-mono">anthropic</code> or{' '}
            <code className="font-mono">openai</code>. Got <code className="font-mono">{providerParam ?? '(empty)'}</code>.
          </p>
          <Link to="/dashboard" className="text-sm text-brand hover:underline">
            ← Back to dashboard
          </Link>
        </div>
      </div>
    )
  }

  const succeeded = setMutation.isSuccess
  const failed = setMutation.isError
  const errorMessage = failed ? (setMutation.error as Error).message : ''

  return (
    <div className="min-h-screen bg-surface-0 px-4 py-10 sm:py-16">
      <div className="max-w-xl mx-auto space-y-6">
        <div>
          <Link
            to="/dashboard/agents"
            className="text-xs text-text-tertiary hover:text-text-primary"
          >
            ← Back to Agents
          </Link>
          <h1 className="text-xl font-semibold text-text-primary mt-2">
            Add your {spec.label} API key
          </h1>
          <p className="text-sm text-text-secondary mt-2 leading-relaxed">
            {spec.description}
          </p>
        </div>

        {isStored && !succeeded && (
          <div className="rounded-md border border-success/30 bg-success/10 px-4 py-3">
            <p className="text-sm font-medium text-success">
              {spec.label} key is already vaulted{scope === 'agent' && forAgentLabel ? ` for ${forAgentLabel}` : ''}.
            </p>
            <p className="text-xs text-text-secondary mt-1">
              Replace it below if you need to rotate. Your agents will use the new key on
              their next request — no restart needed.
            </p>
          </div>
        )}

        <div className="rounded-md border border-border-default bg-surface-1 px-5 py-5 space-y-4">
          <div>
            <label className="block text-sm font-medium text-text-primary mb-1.5">
              {spec.label} API key
            </label>
            <div className="flex gap-2">
              <input
                type={showKey ? 'text' : 'password'}
                autoComplete="off"
                spellCheck={false}
                value={keyInput}
                onChange={e => setKeyInput(e.target.value)}
                placeholder={`${spec.keyPrefix}…`}
                className="flex-1 px-3 py-2 text-sm font-mono rounded border border-border-default bg-surface-0 text-text-primary placeholder-text-tertiary focus:outline-none focus:border-brand"
              />
              <button
                type="button"
                onClick={() => setShowKey(s => !s)}
                className="text-xs px-2 py-1 rounded border border-border-default text-text-tertiary hover:text-text-primary hover:bg-surface-0"
              >
                {showKey ? 'Hide' : 'Show'}
              </button>
            </div>
            <p className="text-xs text-text-tertiary mt-1.5">
              Get a key:{' '}
              <a
                href={spec.consoleURL}
                target="_blank"
                rel="noopener noreferrer"
                className="text-brand hover:underline"
              >
                {spec.consoleURL.replace(/^https?:\/\//, '')}
              </a>
            </p>
          </div>

          {forAgentId && (
            <div>
              <p className="text-xs text-text-tertiary mb-1.5">Scope</p>
              <div className="inline-flex rounded border border-border-default overflow-hidden">
                <button
                  type="button"
                  onClick={() => setScope('user')}
                  className={`px-3 py-1.5 text-xs ${scope === 'user' ? 'bg-brand text-surface-0' : 'bg-surface-0 text-text-secondary hover:text-text-primary'}`}
                >
                  All my agents
                </button>
                <button
                  type="button"
                  onClick={() => setScope('agent')}
                  className={`px-3 py-1.5 text-xs border-l border-border-default ${scope === 'agent' ? 'bg-brand text-surface-0' : 'bg-surface-0 text-text-secondary hover:text-text-primary'}`}
                >
                  Only {forAgentLabel}
                </button>
              </div>
              <p className="text-xs text-text-tertiary mt-1.5">
                {scope === 'user'
                  ? 'This key applies to every agent unless an agent has its own override.'
                  : `This key applies only to ${forAgentLabel}. Other agents keep using your user-level key.`}
              </p>
            </div>
          )}

          {failed && (
            <div className="rounded border border-danger/30 bg-danger/10 px-3 py-2.5 text-xs text-danger">
              {errorMessage}
            </div>
          )}

          {succeeded && (
            <div className="rounded border border-success/30 bg-success/10 px-3 py-2.5 space-y-1">
              <p className="text-sm font-medium text-success">
                ✓ Key saved.
              </p>
              <p className="text-xs text-text-secondary">
                Return to your agent — setup will continue automatically. (The
                install skill polls for the key and resumes within a couple seconds.)
              </p>
            </div>
          )}

          <div className="flex items-center gap-3 pt-1">
            <button
              type="button"
              onClick={() => setMutation.mutate()}
              disabled={setMutation.isPending || !keyInput.trim()}
              className="px-4 py-2 text-sm font-medium rounded bg-brand text-surface-0 hover:bg-brand-strong disabled:opacity-50 disabled:cursor-not-allowed"
            >
              {setMutation.isPending ? 'Saving…' : isStored ? 'Replace key' : 'Save key'}
            </button>
            {succeeded && (
              <button
                type="button"
                onClick={() => {
                  setMutation.reset()
                  setKeyInput('')
                }}
                className="text-xs text-text-tertiary hover:text-text-primary"
              >
                Replace it again
              </button>
            )}
          </div>
        </div>

        <details className="group">
          <summary className="text-sm text-text-tertiary cursor-pointer hover:text-text-primary select-none">
            Why does Clawvisor need this?
          </summary>
          <div className="mt-3 text-xs text-text-secondary space-y-2 leading-relaxed">
            <p>
              When an agent talks to Anthropic/OpenAI through Clawvisor's lite-proxy, the
              agent sends its <code className="font-mono">cvis_…</code> token in the
              Authorization header. Clawvisor swaps that for your real upstream API key
              before forwarding the request to{' '}
              <code className="font-mono">{spec.id === 'anthropic' ? 'api.anthropic.com' : 'api.openai.com'}</code>.
            </p>
            <p>
              The swap is what gives Clawvisor the visibility to mediate tool calls + inject
              vaulted credentials, and what keeps your real API key out of every agent's
              context. Your agents only ever see the <code className="font-mono">cvis_…</code>{' '}
              token — they can't make calls to{' '}
              {spec.id === 'anthropic' ? 'Anthropic' : 'OpenAI'} without going through this
              proxy.
            </p>
          </div>
        </details>
      </div>
    </div>
  )
}
