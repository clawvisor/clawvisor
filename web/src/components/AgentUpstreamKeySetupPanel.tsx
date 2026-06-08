import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '../api/client'
import QuestionToggleGroup from './QuestionToggleGroup'
import {
  type CredentialScope,
  type LLMProvider,
  hasProviderAgentKey,
  hasProviderUpstreamKey,
  providerLabel,
} from '../utils/llmCredentials'

export function useUpstreamKeyReadiness(
  provider: LLMProvider,
  credentialScope: CredentialScope,
  agentId?: string,
) {
  const { data: userCreds } = useQuery({
    queryKey: ['llm-credentials', 'user'],
    queryFn: () => api.llmCredentials.list(),
  })
  const { data: agentCreds } = useQuery({
    queryKey: ['llm-credentials', agentId ?? 'pending-agent'],
    queryFn: () => api.llmCredentials.list(agentId!),
    enabled: credentialScope === 'agent' && !!agentId,
  })

  const userKeyReady = hasProviderUpstreamKey(userCreds, provider)
  const agentKeyReady = hasProviderAgentKey(agentCreds, provider)
  const apiKeyReady = credentialScope === 'user' ? userKeyReady : agentKeyReady

  return { userKeyReady, agentKeyReady, apiKeyReady }
}

export function apiKeyContinueHint(
  provider: LLMProvider,
  credentialScope: CredentialScope,
  apiKeyReady: boolean,
): string | undefined {
  if (apiKeyReady) return undefined
  return credentialScope === 'agent'
    ? `Save an agent-specific ${providerLabel(provider)} key above to continue`
    : `Add a user-level ${providerLabel(provider)} key above to continue`
}

const PROTOTYPE_SAVE_MS = 900

function InlineVaultKeyForm({
  scope,
  agentName,
  agentId,
  provider,
  prototypeSaveUnlock = false,
  onKeySaved,
}: {
  scope: CredentialScope
  agentName: string
  agentId?: string
  provider: LLMProvider
  prototypeSaveUnlock?: boolean
  onKeySaved?: () => void
}) {
  const qc = useQueryClient()
  const isUserScope = scope === 'user'
  const [apiKey, setApiKey] = useState('')
  const [revealed, setRevealed] = useState(false)
  const [awaitingAgent, setAwaitingAgent] = useState(false)
  const [prototypeSaving, setPrototypeSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const setMut = useMutation({
    mutationFn: (key: string) => {
      if (isUserScope) return api.llmCredentials.set(provider, key)
      if (!agentId) return Promise.reject(new Error('Agent not registered yet'))
      return api.llmCredentials.set(provider, key, agentId)
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['llm-credentials', agentId ?? 'user'] })
      qc.invalidateQueries({ queryKey: ['llm-credentials', 'user'] })
      if (agentId) qc.invalidateQueries({ queryKey: ['llm-credentials', agentId] })
      setApiKey('')
      setAwaitingAgent(false)
      setError(null)
    },
    onError: (err: Error) => {
      setAwaitingAgent(false)
      setError(err.message)
    },
  })

  useEffect(() => {
    if (isUserScope || !agentId || !awaitingAgent || setMut.isPending) return
    const key = apiKey.trim()
    if (!key) return
    setMut.mutate(key)
  }, [isUserScope, agentId, awaitingAgent, apiKey, setMut.isPending, setMut.mutate])

  const hasValue = apiKey.trim().length > 0
  const isSaving = setMut.isPending || prototypeSaving || (!isUserScope && awaitingAgent && !prototypeSaveUnlock)
  const saveActive = hasValue && !isSaving

  const handleSave = () => {
    const key = apiKey.trim()
    if (!key) return
    setError(null)

    if (prototypeSaveUnlock) {
      setPrototypeSaving(true)
      if (isUserScope || agentId) setMut.mutate(key)
      else setAwaitingAgent(true)
      window.setTimeout(() => {
        setPrototypeSaving(false)
        onKeySaved?.()
      }, PROTOTYPE_SAVE_MS)
      return
    }

    if (isUserScope || agentId) setMut.mutate(key)
    else setAwaitingAgent(true)
  }

  const title = isUserScope
    ? `Use user-level ${providerLabel(provider)} key`
    : `Set agent-specific ${providerLabel(provider)} key`
  const description = isUserScope
    ? `Clawvisor will use your user-level ${providerLabel(provider)} key for ${agentName}. You can override with an agent-specific key later from the agent settings page.`
    : `Saved directly to ${agentName}. Only this agent uses this key — other agents fall through to your user-level vault.`

  return (
    <div className="rounded border border-border-default bg-surface-1 p-3 space-y-3">
      <div>
        <p className="text-sm font-medium text-text-primary">{title}</p>
        <p className="text-sm text-text-secondary mt-1">{description}</p>
      </div>
      <div className="space-y-2">
        <div className="flex items-center gap-2">
          <div className="relative min-w-0 flex-1">
            <input
              id={`inline-vault-key-${scope}-${provider}`}
              type={revealed ? 'text' : 'password'}
              aria-label={`${providerLabel(provider)} API key`}
              value={apiKey}
              onChange={e => { setApiKey(e.target.value); setError(null) }}
              placeholder={provider === 'anthropic' ? 'sk-ant-...' : 'sk-...'}
              autoComplete="off"
              disabled={isSaving}
              className="block w-full text-sm rounded border border-border-default bg-surface-0 text-text-primary pl-3 pr-10 py-1.5 focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand placeholder:text-text-tertiary disabled:opacity-60"
            />
            <button
              type="button"
              onClick={() => setRevealed(v => !v)}
              aria-label={revealed ? 'Hide API key' : 'Show API key'}
              className="absolute right-2 top-1/2 -translate-y-1/2 rounded p-1 text-text-tertiary hover:text-text-primary focus:outline-none focus-visible:ring-1 focus-visible:ring-brand/30"
            >
              {revealed ? (
                <svg className="h-4 w-4" fill="none" stroke="currentColor" strokeWidth="1.5" viewBox="0 0 24 24" aria-hidden>
                  <path strokeLinecap="round" strokeLinejoin="round" d="M3.98 8.223A10.477 10.477 0 001.934 12C3.226 16.338 7.244 19.5 12 19.5c.993 0 1.953-.138 2.863-.395M6.228 6.228A10.45 10.45 0 0112 4.5c4.756 0 8.773 3.162 10.065 7.498a10.523 10.523 0 01-4.293 5.774M6.228 6.228L3 3m3.228 3.228l3.65 3.65m7.894 7.894L21 21m-3.228-3.228l-3.65-3.65m0 0a3 3 0 10-4.243-4.243m4.242 4.242L9.88 9.88" />
                </svg>
              ) : (
                <svg className="h-4 w-4" fill="none" stroke="currentColor" strokeWidth="1.5" viewBox="0 0 24 24" aria-hidden>
                  <path strokeLinecap="round" strokeLinejoin="round" d="M2.036 12.322a1.012 1.012 0 010-.639C3.423 7.51 7.36 4.5 12 4.5c4.638 0 8.573 3.007 9.963 7.178.07.207.07.431 0 .639C20.577 16.49 16.64 19.5 12 19.5c-4.638 0-8.573-3.007-9.963-7.178z" />
                  <path strokeLinecap="round" strokeLinejoin="round" d="M15 12a3 3 0 11-6 0 3 3 0 016 0z" />
                </svg>
              )}
            </button>
          </div>
          <button
            type="button"
            disabled={!hasValue || isSaving}
            onClick={handleSave}
            className={`inline-flex shrink-0 items-center gap-1.5 rounded px-4 py-1.5 text-sm font-medium transition-colors ${
              saveActive
                ? 'bg-brand text-surface-0 hover:bg-brand-strong'
                : 'bg-brand/40 text-surface-0/80'
            } disabled:cursor-not-allowed disabled:opacity-50`}
          >
            {isSaving ? (
              <>
                <svg className="h-3.5 w-3.5 animate-spin" fill="none" viewBox="0 0 24 24" aria-hidden>
                  <circle className="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" strokeWidth="4" />
                  <path className="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4z" />
                </svg>
                Saving…
              </>
            ) : (
              'Save'
            )}
          </button>
        </div>
        {error && <p className="text-xs text-danger">{error}</p>}
      </div>
    </div>
  )
}


type AgentUpstreamKeySetupPanelProps = {
  agentName: string
  agentId?: string
  provider: LLMProvider
  credentialScope: CredentialScope
  onCredentialScopeChange: (scope: CredentialScope) => void
  prototypeSaveUnlock?: boolean
  onKeySaved?: () => void
}

export default function AgentUpstreamKeySetupPanel({
  agentName,
  agentId,
  provider,
  credentialScope,
  onCredentialScopeChange,
  prototypeSaveUnlock,
  onKeySaved,
}: AgentUpstreamKeySetupPanelProps) {
  return (
    <div className="space-y-4">
      <div>
        <p className="text-sm font-medium text-text-primary">
          Set the {providerLabel(provider)} key for {agentName}
        </p>
        <p className="text-xs text-text-tertiary mt-1 leading-relaxed">
          Clawvisor swaps your <code className="font-mono">cvis_…</code> token for an upstream
          {' '}{providerLabel(provider)} key on each call. Pick a scope and save the key
          — it's written directly against this agent, not held in the browser.
        </p>
      </div>
      <QuestionToggleGroup
        label="Which API key should Clawvisor use for this agent?"
        value={credentialScope}
        onChange={value => onCredentialScopeChange(value as CredentialScope)}
        options={[
          ['user', `Use user-level ${providerLabel(provider)} key`],
          ['agent', `Set agent-specific ${providerLabel(provider)} key`],
        ]}
      />
      <InlineVaultKeyForm
        scope={credentialScope}
        agentName={agentName}
        agentId={credentialScope === 'agent' ? agentId : undefined}
        provider={provider}
        prototypeSaveUnlock={prototypeSaveUnlock}
        onKeySaved={onKeySaved}
      />
    </div>
  )
}
