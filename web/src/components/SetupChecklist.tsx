import { NavLink } from 'react-router-dom'
import { useAuth } from '../hooks/useAuth'
import { useSetupProgress, type SetupStep } from '../hooks/useSetupProgress'

interface Props {
  variant?: 'full' | 'compact'
}

// PRD Epic C — onboarding checklist. Full variant lives on Quickstart for
// first-run users; compact variant lives on Home as a reminder strip when
// agents exist but other steps remain. The checklist is hidden in org
// context (per the team-admin persona scenario) and when everything's done.
export default function SetupChecklist({ variant = 'full' }: Props) {
  const { currentOrg } = useAuth()
  const { steps, isLoading, isComplete } = useSetupProgress()

  if (currentOrg) return null
  if (isLoading) return null
  if (isComplete) return null

  return variant === 'compact' ? (
    <CompactChecklist steps={steps} />
  ) : (
    <FullChecklist steps={steps} />
  )
}

function FullChecklist({ steps }: { steps: SetupStep[] }) {
  const required = steps.filter(s => !s.optional)
  const done = required.filter(s => s.complete).length
  return (
    <section className="rounded-md border border-border-default bg-surface-1 p-5">
      <div className="flex items-baseline justify-between mb-3">
        <h2 className="text-lg font-semibold text-text-primary">Setup</h2>
        <span className="text-xs text-text-tertiary">
          {done}/{required.length} required complete
        </span>
      </div>
      <ul className="space-y-2">
        {steps.map(step => (
          <StepRow key={step.id} step={step} />
        ))}
      </ul>
    </section>
  )
}

function CompactChecklist({ steps }: { steps: SetupStep[] }) {
  const incomplete = steps.filter(s => !s.complete && !s.optional)
  if (incomplete.length === 0) return null
  const next = incomplete[0]
  const remaining = incomplete.length - 1
  return (
    <div className="rounded-md border border-brand/30 bg-brand-muted px-4 py-3 flex items-center gap-3 text-sm">
      <span className="shrink-0 w-2 h-2 rounded-full bg-brand" />
      <span className="text-text-primary flex-1">
        Next: <span className="font-medium">{next.label}</span>
        {remaining > 0 && (
          <span className="text-text-tertiary"> · {remaining} more step{remaining === 1 ? '' : 's'}</span>
        )}
      </span>
      <NavLink
        to={next.to}
        className="text-brand font-medium hover:opacity-80 transition-opacity"
      >
        Open →
      </NavLink>
    </div>
  )
}

function StepRow({ step }: { step: SetupStep }) {
  return (
    <li className="flex items-center gap-3">
      <StepIcon complete={step.complete} />
      {step.complete ? (
        <span className="text-text-tertiary line-through">{step.label}</span>
      ) : (
        <NavLink to={step.to} className="text-brand font-medium hover:underline">
          {step.label}
          {step.optional && <span className="ml-2 text-xs text-text-tertiary">(optional)</span>}
        </NavLink>
      )}
    </li>
  )
}

function StepIcon({ complete }: { complete: boolean }) {
  if (complete) {
    return (
      <span className="shrink-0 w-5 h-5 rounded-full bg-success/20 text-success flex items-center justify-center">
        <svg className="w-3 h-3" fill="none" stroke="currentColor" strokeWidth="3" viewBox="0 0 24 24">
          <path d="M5 13l4 4L19 7" />
        </svg>
      </span>
    )
  }
  return (
    <span className="shrink-0 w-5 h-5 rounded-full border-2 border-border-default" />
  )
}
