export type AgentTab =
  | 'openclaw'
  | 'hermes'
  | 'claude-code'
  | 'codex'
  | 'claude-desktop'
  | 'gbrain'
  | 'cloud-agent'
  | 'other'

export type AgentPrimitive = 'Skill' | 'Configuration profile' | 'Manual'

export interface AgentMeta {
  label: string
  tagline: string
  primitive: AgentPrimitive
}

export const PROXY_LITE_AGENT_TABS: AgentTab[] = [
  'openclaw',
  'hermes',
  'claude-code',
  'codex',
  'claude-desktop',
  'gbrain',
  'cloud-agent',
  'other',
]

export const LEGACY_AGENT_TABS: AgentTab[] = ['openclaw', 'claude-code', 'claude-desktop', 'other']

export const KNOWN_AGENT_TABS: AgentTab[] = [
  'openclaw',
  'hermes',
  'claude-code',
  'codex',
  'claude-desktop',
  'gbrain',
  'cloud-agent',
]

export const AGENT_META: Record<AgentTab, AgentMeta> = {
  'claude-code': {
    label: 'Claude Code',
    tagline: "Anthropic's CLI coding agent",
    primitive: 'Skill',
  },
  codex: {
    label: 'Codex',
    tagline: "OpenAI's CLI coding agent",
    primitive: 'Skill',
  },
  hermes: {
    label: 'Hermes',
    tagline: 'Nous Research general-purpose agent',
    primitive: 'Skill',
  },
  openclaw: {
    label: 'OpenClaw',
    tagline: 'Open-source Claude Code workspace',
    primitive: 'Skill',
  },
  'claude-desktop': {
    label: 'Claude Desktop',
    tagline: 'Anthropic desktop app (macOS)',
    primitive: 'Configuration profile',
  },
  gbrain: {
    label: 'GBrain',
    tagline: 'Personal-brain data pipeline',
    primitive: 'Skill',
  },
  'cloud-agent': {
    label: 'Cloud agent (Computer, ChatGPT, Claude)',
    tagline: 'Perplexity Computer, hosted ChatGPT',
    primitive: 'Manual',
  },
  other: {
    label: 'Other agent',
    tagline: 'Custom HTTP clients & Harness',
    primitive: 'Manual',
  },
}

export function agentSetupPath(harness: AgentTab) {
  return `/dashboard/agents/setup/${encodeURIComponent(harness)}`
}

export function agentMatchesTab(agent: { name: string; install_context?: { harness?: string } | null }, tab: AgentTab): boolean {
  const harness = agent.install_context?.harness
  if (tab === 'other') {
    if (harness && KNOWN_AGENT_TABS.includes(harness as AgentTab)) return false
    return !KNOWN_AGENT_TABS.some(t => agent.name === t || agent.name.startsWith(`${t}-`))
  }
  return harness === tab || (!harness && (agent.name === tab || agent.name.startsWith(`${tab}-`)))
}
