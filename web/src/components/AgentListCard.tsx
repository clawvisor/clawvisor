import { IconBrain, IconCloud } from '@tabler/icons-react'
import type { ReactNode } from 'react'
import { formatDistanceToNow } from 'date-fns'
import type { Agent } from '../api/client'
import {
  PROXY_LITE_AGENT_TABS,
  agentMatchesTab,
  type AgentTab,
} from '../constants/agentTabs'

type AgentListCardProps = {
  agent: Agent
  live?: boolean
  taskCount?: number
  lastTaskPurpose?: string
  onClick: () => void
}

export function resolveAgentTab(agent: Agent): AgentTab {
  for (const tab of PROXY_LITE_AGENT_TABS) {
    if (agentMatchesTab(agent, tab)) return tab
  }
  return 'other'
}

function agentHarnessIcon(tab: AgentTab): ReactNode {
  switch (tab) {
    case 'claude-code':
    case 'claude-desktop':
      return <img src="/logos/claude-color.svg" alt="" className="w-5 h-5 object-contain" />
    case 'codex':
      return <img src="/logos/openai.svg" alt="" className="w-5 h-5 object-contain dark:invert" />
    case 'hermes':
      return <img src="/logos/hermes.svg" alt="" className="w-5 h-5 object-contain dark:invert" />
    case 'openclaw':
      return <img src="/logos/openclaw.svg" alt="" className="w-5 h-5 object-contain" />
    case 'cloud-agent':
      return <IconCloud className="w-5 h-5 text-text-tertiary" stroke={1.5} />
    case 'gbrain':
      return <IconBrain className="w-5 h-5 text-text-tertiary" stroke={1.5} />
    case 'other':
      return (
        <svg className="w-5 h-5 text-text-tertiary" fill="none" stroke="currentColor" strokeWidth="1.5" viewBox="0 0 24 24">
          <path strokeLinecap="round" strokeLinejoin="round" d="M16 21v-2a4 4 0 00-4-4H5a4 4 0 00-4 4v2" />
          <circle cx="8.5" cy="7" r="4" />
          <path strokeLinecap="round" strokeLinejoin="round" d="M20 8v6M23 11h-6" />
        </svg>
      )
    default:
      return (
        <svg className="w-5 h-5 text-text-tertiary" fill="none" stroke="currentColor" strokeWidth="1.5" viewBox="0 0 24 24">
          <rect x="4" y="4" width="16" height="16" rx="3" />
          <path strokeLinecap="round" d="M9 9h6M9 12h6M9 15h4" />
        </svg>
      )
  }
}

export function AgentListCard({
  agent,
  live = false,
  taskCount = 0,
  lastTaskPurpose,
  onClick,
}: AgentListCardProps) {
  const tab = resolveAgentTab(agent)
  const lastTaskLabel = lastTaskPurpose
    ?? (agent.last_task_at
      ? `Last task ${formatDistanceToNow(new Date(agent.last_task_at), { addSuffix: true })}`
      : 'No tasks yet')

  return (
    <div
      role="link"
      tabIndex={0}
      onClick={onClick}
      onKeyDown={e => {
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault()
          onClick()
        }
      }}
      className="dev-pick-row group items-start cursor-pointer focus:outline-none focus:ring-2 focus:ring-brand/30"
    >
      <div className="dev-pick-icon group-hover:border-brand/30 shrink-0">
        {agentHarnessIcon(tab)}
      </div>
      <div className="flex-1 min-w-0 flex flex-col gap-1">
        <div className="flex items-center gap-2 min-w-0">
          <p className="dev-pick-title truncate flex-1">{agent.name}</p>
          {live && (
            <span className="dev-badge--success normal-case tracking-normal shrink-0">live</span>
          )}
        </div>
        <p className="dev-pick-desc line-clamp-2 leading-snug">{lastTaskLabel}</p>
        <p className="text-xs text-text-tertiary">
          {taskCount} {taskCount === 1 ? 'task' : 'tasks'}
        </p>
      </div>
    </div>
  )
}
