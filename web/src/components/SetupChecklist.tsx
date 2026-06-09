import type { ReactNode } from 'react'
import { Link } from 'react-router-dom'
import ConnectAccountPicker from './ConnectAccountPicker'
import { ConnectAgentStepCard } from './ConnectAgentPicker'
import ConnectedServicesStrip from './ConnectedServicesStrip'
import ResolveFirstTaskPanel from './ResolveFirstTaskPanel'
import ThreeLayersOfControlCallout from './ThreeLayersOfControlCallout'
import { useSetupProgress } from '../hooks/useSetupProgress'

export default function SetupChecklist({ compact = false }: { compact?: boolean }) {
  const {
    steps,
    agents,
    connectedServices,
    incompleteRequired,
    isComplete,
    isLoading,
    progress,
  } = useSetupProgress()

  if (isLoading || isComplete) return null

  const requiredSteps = steps.filter(s => !s.optional)
  const completedSteps = requiredSteps.filter(s => s.complete).length
  const totalSteps = requiredSteps.length

  if (compact) {
    const next = incompleteRequired[0]
    return (
      <div className="dev-panel px-4 py-3 flex flex-wrap items-center justify-between gap-3">
        <div className="flex items-center gap-3 min-w-0">
          <span className="dev-badge--brand shrink-0">setup</span>
          <span className="text-xs text-text-secondary truncate">
            {incompleteRequired.length} step{incompleteRequired.length === 1 ? '' : 's'} remaining
            {next && <> · next: {next.label.toLowerCase()}</>}
          </span>
        </div>
        <div className="flex items-center gap-3 shrink-0">
          <div className="w-20 h-1 bg-surface-2 rounded-full overflow-hidden">
            <div className="h-full bg-primary transition-all" style={{ width: `${Math.round(progress * 100)}%` }} />
          </div>
          <Link to="/dashboard/home" className="ds-link text-xs">
            setup →
          </Link>
        </div>
      </div>
    )
  }

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <p className="text-body-sm text-text-secondary">
            Complete these steps to get agents running with approvals.
          </p>
        </div>
        <span className="text-sm font-medium text-text-secondary tabular-nums shrink-0">
          <span className="text-text-primary font-semibold">{completedSteps}</span>
          {' / '}
          <span className="text-text-primary font-semibold">{totalSteps}</span>
          {' steps complete'}
        </span>
      </div>
      <ol className="space-y-2">
        {steps.map((step, i) => (
          <li key={step.id} id={stepAnchorId(step.id)}>
            {step.id === 'account' && !step.complete ? (
              <ExpandedChecklistStep index={i + 1} label={step.label}>
                <ConnectAccountPicker />
              </ExpandedChecklistStep>
            ) : step.id === 'agent' && !step.complete ? (
              <ConnectAgentStepCard
                id="connect-agent"
                title="Connect an agent"
                stepNum={i + 1}
                connectedAgents={agents}
              />
            ) : step.id === 'approval' && !step.complete ? (
              <ExpandedChecklistStep index={i + 1} label="See Clawvisor in action">
                <ResolveFirstTaskPanel />
              </ExpandedChecklistStep>
            ) : step.id === 'account' && step.complete && connectedServices.length > 0 ? (
              <div className="flex flex-wrap items-center gap-2 px-3 py-2 rounded-md bg-surface-2/50">
                <StepIcon done />
                <ConnectedServicesStrip services={connectedServices} />
              </div>
            ) : step.id === 'agent' && step.complete && agents.length > 0 ? (
              <ExpandedChecklistStep index={i + 1} label="Connect another agent" done>
                <ConnectAgentStepCard
                  id="connect-agent"
                  variant="another"
                  connectedAgents={agents}
                  hideHeader
                />
              </ExpandedChecklistStep>
            ) : step.complete ? (
              <div className="flex items-center gap-3 px-3 py-2 rounded-md bg-surface-2/50 text-text-tertiary">
                <StepIcon done />
                <span className="text-sm line-through">{step.label}</span>
                {step.optional && <span className="ds-overline normal-case tracking-normal">optional</span>}
              </div>
            ) : (
              <Link
                to={step.to}
                className="flex items-center gap-3 px-3 py-2 rounded-md border border-border-default bg-surface-1 hover:border-border-strong hover:bg-surface-2 transition-colors group"
              >
                <StepIcon done={false} index={i + 1} />
                <span className="text-sm text-text-primary group-hover:text-text-primary flex-1">{step.label}</span>
                {step.optional && (
                  <span className="ds-overline normal-case tracking-normal">optional</span>
                )}
                <svg className="w-3.5 h-3.5 text-text-tertiary shrink-0" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24">
                  <path d="M9 5l7 7-7 7" />
                </svg>
              </Link>
            )}
          </li>
        ))}
      </ol>
      <ThreeLayersOfControlCallout />
    </div>
  )
}

function stepAnchorId(id: string): string | undefined {
  if (id === 'account') return 'connect-account'
  if (id === 'agent') return 'connect-agent'
  if (id === 'approval') return 'resolve-first-task'
  return undefined
}

function ExpandedChecklistStep({
  index,
  label,
  done = false,
  children,
}: {
  index: number
  label: string
  done?: boolean
  children: ReactNode
}) {
  return (
    <div className="rounded-md border border-border-default bg-surface-1 px-5 py-4">
      <div className="flex items-center gap-3">
        <StepIcon done={done} index={index} large />
        <span className="text-base font-medium text-text-primary">{label}</span>
      </div>
      <div className="pl-9 mt-1">{children}</div>
    </div>
  )
}

function StepIcon({ done, index, large = false }: { done: boolean; index?: number; large?: boolean }) {
  const size = large ? 'w-6 h-6' : 'w-5 h-5'
  const checkSize = large ? 'w-3.5 h-3.5' : 'w-3 h-3'
  const numSize = large ? 'text-sm' : 'text-2xs'

  if (done) {
    return (
      <span className={`${size} rounded-md bg-success/15 border border-success/30 flex items-center justify-center shrink-0`}>
        <svg className={`${checkSize} text-success`} fill="none" stroke="currentColor" strokeWidth="2.5" viewBox="0 0 24 24">
          <path d="M5 13l4 4L19 7" />
        </svg>
      </span>
    )
  }
  return (
    <span className={`${size} rounded-md bg-surface-2 border border-border-default flex items-center justify-center ${numSize} text-text-tertiary shrink-0 tabular-nums`}>
      {index}
    </span>
  )
}
