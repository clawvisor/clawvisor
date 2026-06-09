import { IconBrain, IconCloud } from '@tabler/icons-react'
import { useState, type FormEvent, type ReactNode } from 'react'
import { createPortal } from 'react-dom'
import { Link } from 'react-router-dom'
import { type Agent } from '../api/client'
import { resolveAgentTab } from './AgentListCard'
import {
  AGENT_META,
  PROXY_LITE_AGENT_TABS,
  agentSetupPath,
  type AgentPrimitive,
  type AgentTab,
} from '../constants/agentTabs'

function agentPickerSubtitle(primitive: AgentPrimitive): string {
  switch (primitive) {
    case 'Skill':
      return 'Copy the installer to get started'
    case 'Configuration profile':
      return 'Follow the setup guide to get started'
    case 'Manual':
      return 'Follow the setup instructions to get started'
  }
}

type ConnectAgentStepCardProps = {
  title?: string
  stepNum?: number
  done?: boolean
  loading?: boolean
  id?: string
  /** Compact header for the post-setup "connect another" state */
  variant?: 'setup' | 'another'
  connectedAgents?: Agent[]
  hideHeader?: boolean
}

export function ConnectAgentStepCard({
  title = 'Connect an agent',
  stepNum,
  done = false,
  loading = false,
  id,
  variant = 'setup',
  connectedAgents,
  hideHeader = false,
}: ConnectAgentStepCardProps) {
  const isAnother = variant === 'another'

  return (
    <div
      id={id}
      className={`${hideHeader ? '' : 'dev-step-card scroll-mt-24'} ${done && !isAnother ? 'dev-step-card--done' : ''}`}
    >
      {!hideHeader && (
        <div className="flex items-center gap-3 mb-4">
          <div className={done ? 'dev-step-num--done' : 'dev-step-num--pending'}>
            {done ? (
              <svg className="w-3.5 h-3.5" fill="none" stroke="currentColor" strokeWidth="2.5" viewBox="0 0 24 24">
                <path d="M5 13l4 4L19 7" />
              </svg>
            ) : (
              <span>{stepNum}</span>
            )}
          </div>
          <h3 className="page-section-title text-text-primary normal-case tracking-normal text-sm mb-0 flex-1">
            {title}
          </h3>
          {loading ? (
            <span className="font-mono text-xs text-text-tertiary">checking…</span>
          ) : done ? (
            <span className="dev-badge--success">done</span>
          ) : null}
        </div>
      )}
      <AgentPickerContent connectedAgents={connectedAgents} />
    </div>
  )
}

const AGENT_REQUEST_ISSUE_URL = 'https://github.com/clawvisor/clawvisor/issues/new'

function LiveAgentChip({ agent }: { agent: Agent }) {
  const tab = resolveAgentTab(agent)
  const meta = AGENT_META[tab]

  return (
    <Link
      to={`/dashboard/agents/${encodeURIComponent(agent.id)}`}
      className="dev-pick-row group w-fit no-underline text-inherit"
    >
      <div className="flex items-center gap-3 min-w-0">
        <div className="dev-pick-icon group-hover:border-brand/30">
          <AgentHarnessIcon tab={tab} />
        </div>
        <div className="min-w-0 flex flex-col">
          <p className="dev-pick-title">{meta.label}</p>
          <p className="dev-pick-desc mt-0 leading-snug line-clamp-1">
            Connected Today
          </p>
        </div>
        <svg
          className="w-3.5 h-3.5 text-text-tertiary shrink-0 ml-2 group-hover:text-text-secondary transition-colors"
          fill="none"
          stroke="currentColor"
          strokeWidth="2"
          viewBox="0 0 24 24"
          aria-hidden
        >
          <path strokeLinecap="round" strokeLinejoin="round" d="M5 12h14M13 6l6 6-6 6" />
        </svg>
      </div>
    </Link>
  )
}

function LiveAgentsSummary({ agents }: { agents: Agent[] }) {
  if (agents.length === 0) return null

  return (
    <div className="mt-4 space-y-2">
      <p className="text-sm font-medium text-text-secondary">Connected Agents</p>
      {agents.length === 1 ? (
        <LiveAgentChip agent={agents[0]} />
      ) : (
        <div className="flex flex-wrap items-stretch gap-3">
          {agents.map((agent) => (
            <LiveAgentChip key={agent.id} agent={agent} />
          ))}
        </div>
      )}
    </div>
  )
}

export function AgentPickerContent({ connectedAgents = [] }: { connectedAgents?: Agent[] }) {
  const [requestOpen, setRequestOpen] = useState(false)

  return (
    <>
      <p className="text-sm text-text-secondary mb-5 leading-relaxed">
        Pair an AI agent with Clawvisor so it can create tasks on your behalf.
        Pick the harness that matches how you run your agent.
      </p>
      <div className="space-y-4">
        <div className="grid grid-cols-1 md:grid-cols-2 gap-3 items-stretch">
          {PROXY_LITE_AGENT_TABS.map(tab => {
            const meta = AGENT_META[tab]
            return (
              <AgentRow
                key={tab}
                tab={tab}
                label={meta.label}
                primitive={meta.primitive}
                icon={<AgentHarnessIcon tab={tab} />}
              />
            )
          })}
        </div>
        <button
          type="button"
          onClick={() => setRequestOpen(true)}
          className="dev-btn-ghost gap-1.5"
        >
          <PlusCircleIcon className="w-3.5 h-3.5" />
          Request an agent framework
        </button>
        <LiveAgentsSummary agents={connectedAgents} />
      </div>
      {requestOpen && (
        <RequestAgentModal onClose={() => setRequestOpen(false)} />
      )}
    </>
  )
}

function RequestAgentModal({ onClose }: { onClose: () => void }) {
  const [framework, setFramework] = useState('')
  const [details, setDetails] = useState('')
  const [error, setError] = useState('')
  const [submitted, setSubmitted] = useState(false)

  const handleSubmit = (e: FormEvent) => {
    e.preventDefault()
    const name = framework.trim()
    if (!name) {
      setError('Enter the agent framework name.')
      return
    }

    const title = encodeURIComponent(`Agent framework request: ${name}`)
    const body = encodeURIComponent(
      [
        '## Agent framework',
        name,
        '',
        '## How you run it',
        details.trim() || '(not provided)',
        '',
        '## Context',
        'Submitted from the Clawvisor dashboard agent picker.',
      ].join('\n'),
    )

    window.open(
      `${AGENT_REQUEST_ISSUE_URL}?title=${title}&body=${body}`,
      '_blank',
      'noopener,noreferrer',
    )
    setSubmitted(true)
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center p-4">
      <div className="absolute inset-0 bg-black/60" onClick={onClose} aria-hidden />
      <div
        role="dialog"
        aria-labelledby="request-agent-title"
        className="relative dev-panel rounded-lg w-full max-w-md shadow-lg"
      >
        <div className="flex items-center justify-between px-5 py-3 border-b border-border-default">
          <h2 id="request-agent-title" className="page-section-title mb-0 normal-case tracking-tight text-sm text-text-primary">
            Request an agent
          </h2>
          <button
            type="button"
            onClick={onClose}
            className="text-text-tertiary hover:text-text-primary text-xl leading-none"
            aria-label="Close"
          >
            &times;
          </button>
        </div>

        {submitted ? (
          <div className="px-5 py-4 space-y-4">
            <p className="text-sm text-text-secondary leading-relaxed">
              We opened a GitHub issue draft with your request. Submit it there and we&apos;ll track support for that agent framework.
            </p>
            <button type="button" onClick={onClose} className="dev-btn-primary w-full">
              Done
            </button>
          </div>
        ) : (
          <form onSubmit={handleSubmit} className="px-5 py-4 space-y-4">
            <p className="text-sm text-text-secondary leading-relaxed">
              Tell us which agent framework you&apos;d like Clawvisor to support.
            </p>
            <div className="space-y-1.5">
              <label htmlFor="request-agent-framework" className="text-sm font-medium text-text-primary">
                Agent framework
              </label>
              <input
                id="request-agent-framework"
                type="text"
                value={framework}
                onChange={e => {
                  setFramework(e.target.value)
                  if (error) setError('')
                }}
                placeholder="e.g. Windsurf, Aider, Cursor CLI"
                className="ds-input"
                autoFocus
              />
            </div>
            <div className="space-y-1.5">
              <label htmlFor="request-agent-details" className="text-sm font-medium text-text-primary">
                How do you run it? <span className="font-normal text-text-tertiary">(optional)</span>
              </label>
              <textarea
                id="request-agent-details"
                value={details}
                onChange={e => setDetails(e.target.value)}
                placeholder="CLI tool, desktop app, hosted service, etc."
                rows={3}
                className="w-full font-sans text-sm px-3 py-2 border border-border-default bg-surface-1 text-text-primary rounded-md placeholder:text-text-tertiary resize-y min-h-[88px] focus:outline-2 focus:outline-offset-2 focus:outline-[rgb(var(--color-ring))]"
              />
            </div>
            {error && <p className="text-sm text-danger">{error}</p>}
            <div className="flex items-center justify-end gap-2 pt-1">
              <button type="button" onClick={onClose} className="dev-btn-ghost">
                Cancel
              </button>
              <button type="submit" className="dev-btn-primary">
                Submit request
              </button>
            </div>
          </form>
        )}
      </div>
    </div>
  )
}

export function AgentHarnessIcon({ tab }: { tab: AgentTab }) {
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
      return <AgentIcon className="w-5 h-5 text-text-tertiary" />
    default:
      return <GenericAgentIcon className="w-5 h-5 text-text-tertiary" />
  }
}

function AgentRow({
  tab,
  label,
  primitive,
  icon,
}: {
  tab: AgentTab
  label: string
  primitive: AgentPrimitive
  icon?: ReactNode
}) {
  const setupLink = `${window.location.origin}${agentSetupPath(tab)}`

  return (
    <div className="dev-pick-row group w-full h-full">
      <Link
        to={agentSetupPath(tab)}
        className="flex items-center gap-3 flex-1 min-w-0 no-underline text-inherit"
      >
        {icon && (
          <div className="dev-pick-icon group-hover:border-brand/30">
            {icon}
          </div>
        )}
        <div className="flex-1 min-w-0 w-full flex flex-col">
          <p className="dev-pick-title">{label}</p>
          <p className="dev-pick-desc mt-0 leading-snug line-clamp-1">
            {agentPickerSubtitle(primitive)}
          </p>
        </div>
        {primitive !== 'Skill' && (
          <svg
            className="w-3.5 h-3.5 text-text-tertiary shrink-0 group-hover:text-text-secondary transition-colors"
            fill="none"
            stroke="currentColor"
            strokeWidth="2"
            viewBox="0 0 24 24"
            aria-hidden
          >
            <path d="M9 5l7 7-7 7" />
          </svg>
        )}
      </Link>
      {primitive === 'Skill' && (
        <PickRowCopyHandle value={setupLink} label={label} />
      )}
    </div>
  )
}

function PickRowCopyHandle({ value, label }: { value: string; label: string }) {
  const [copied, setCopied] = useState(false)
  const [toastPhase, setToastPhase] = useState<'idle' | 'in' | 'out'>('idle')
  const toastMessage = `${label} helper skill copied`

  function copy(e: React.MouseEvent) {
    e.preventDefault()
    e.stopPropagation()
    navigator.clipboard.writeText(value).then(() => {
      setCopied(true)
      setToastPhase('in')
      window.setTimeout(() => setCopied(false), 1500)
      window.setTimeout(() => setToastPhase('out'), 2200)
    })
  }

  return (
    <>
      <button
        type="button"
        onClick={copy}
        title={copied ? 'Copied' : 'Copy skill'}
        aria-label={copied ? 'Copied skill' : 'Copy skill'}
        className="dev-pick-copy shrink-0 self-center"
      >
        {copied ? (
          <svg className="w-3.5 h-3.5 text-success" fill="none" stroke="currentColor" strokeWidth="2.5" viewBox="0 0 24 24" aria-hidden>
            <path d="M5 13l4 4L19 7" />
          </svg>
        ) : (
          <svg className="w-3.5 h-3.5" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24" aria-hidden>
            <rect x="9" y="9" width="13" height="13" rx="2" ry="2" />
            <path d="M5 15H4a2 2 0 01-2-2V4a2 2 0 012-2h9a2 2 0 012 2v1" />
          </svg>
        )}
      </button>
      {toastPhase !== 'idle' && createPortal(
        <div
          className={`dev-toast ${toastPhase === 'out' ? 'dev-toast--out' : 'dev-toast--in'}`}
          role="status"
          aria-live="polite"
          onAnimationEnd={() => {
            if (toastPhase === 'out') setToastPhase('idle')
          }}
        >
          {toastMessage}
        </div>,
        document.body,
      )}
    </>
  )
}

function AgentIcon({ className }: { className?: string }) {
  return (
    <svg className={className} fill="none" stroke="currentColor" strokeWidth="1.5" viewBox="0 0 24 24">
      <path strokeLinecap="round" strokeLinejoin="round" d="M16 21v-2a4 4 0 00-4-4H5a4 4 0 00-4 4v2" />
      <circle cx="8.5" cy="7" r="4" />
      <path strokeLinecap="round" strokeLinejoin="round" d="M20 8v6M23 11h-6" />
    </svg>
  )
}

function GenericAgentIcon({ className }: { className?: string }) {
  return (
    <svg className={className} fill="none" stroke="currentColor" strokeWidth="1.5" viewBox="0 0 24 24">
      <rect x="4" y="4" width="16" height="16" rx="3" />
      <path strokeLinecap="round" d="M9 9h6M9 12h6M9 15h4" />
    </svg>
  )
}

function PlusCircleIcon({ className }: { className?: string }) {
  return (
    <svg className={className} viewBox="0 0 24 24" fill="none" xmlns="http://www.w3.org/2000/svg">
      <circle cx="12" cy="12" r="10" stroke="currentColor" strokeWidth="2" />
      <path d="M12 8v8M8 12h8" stroke="currentColor" strokeWidth="2" strokeLinecap="round" />
    </svg>
  )
}
