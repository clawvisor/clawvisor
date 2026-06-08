export type LLMProvider = 'anthropic' | 'openai'

export type LLMCredentialsStatus = {
  credentials: {
    provider: string
    stored: boolean
    agent_stored?: boolean
    agent_id?: string
  }[]
}

export type CredentialScope = 'user' | 'agent'

export function providerLabel(provider: LLMProvider): string {
  return provider === 'anthropic' ? 'Anthropic' : 'OpenAI'
}

export function hasAnyUpstreamKey(creds: LLMCredentialsStatus | undefined): boolean {
  if (!creds) return false
  return creds.credentials.some(c => c.stored || c.agent_stored)
}

export function hasProviderUpstreamKey(creds: LLMCredentialsStatus | undefined, provider: LLMProvider): boolean {
  if (!creds) return false
  return creds.credentials.some(c => c.provider === provider && (c.stored || c.agent_stored))
}

export function hasProviderAgentKey(creds: LLMCredentialsStatus | undefined, provider: LLMProvider): boolean {
  if (!creds) return false
  return creds.credentials.some(c => c.provider === provider && c.agent_stored)
}
