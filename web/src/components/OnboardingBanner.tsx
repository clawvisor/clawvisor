import { useQuery } from '@tanstack/react-query'
import { NavLink } from 'react-router-dom'
import { useState } from 'react'
import { api } from '../api/client'

const DISMISS_KEY = 'clawvisor_onboarding_dismissed'

interface Step {
  label: string
  to: string
  done: boolean
}

export default function OnboardingBanner() {
  const [dismissed, setDismissed] = useState(() => localStorage.getItem(DISMISS_KEY) === '1')

  const { data: services } = useQuery({
    queryKey: ['services'],
    queryFn: () => api.services.list(),
  })

  const { data: agents } = useQuery({
    queryKey: ['agents'],
    queryFn: () => api.agents.list(),
  })

  if (dismissed) return null
  // Wait for data before deciding
  if (services === undefined || agents === undefined) return null

  const hasService = (services.services ?? []).some(
    (s: { status: string; requires_activation?: boolean }) =>
      s.status === 'activated' && (s.requires_activation ?? true),
  )
  const hasAgent = (agents ?? []).length > 0

  // All done — don't show
  if (hasService && hasAgent) return null

  const steps: Step[] = [
    { label: 'Connect a service', to: '/dashboard/services', done: hasService },
    { label: 'Connect an agent', to: '/dashboard/agents', done: hasAgent },
  ]

  const currentIdx = steps.findIndex(s => !s.done)
  const current = steps[currentIdx]!
  const completedCount = steps.filter(s => s.done).length

  function handleDismiss() {
    localStorage.setItem(DISMISS_KEY, '1')
    setDismissed(true)
  }

  return (
    <div className="mx-4 mt-3 px-4 py-2.5 rounded-md bg-brand-muted border border-brand/30 flex items-center justify-between text-sm">
      <div className="flex items-center gap-3">
        {/* Progress dots */}
        <div className="flex items-center gap-1.5">
          {steps.map((s, i) => (
            <div
              key={i}
              className={`w-2 h-2 rounded-full ${
                s.done
                  ? 'bg-brand'
                  : i === currentIdx
                    ? 'bg-brand animate-pulse'
                    : 'bg-border-default'
              }`}
            />
          ))}
        </div>

        <span className="text-text-secondary">
          Step {completedCount + 1} of {steps.length}:
        </span>
        <NavLink
          to={current.to}
          className="text-brand font-medium hover:text-brand-strong transition-colors"
        >
          {current.label}
          <svg className="inline w-3.5 h-3.5 ml-1 -mt-px" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24">
            <path d="M9 5l7 7-7 7" />
          </svg>
        </NavLink>
      </div>

      <button
        onClick={handleDismiss}
        className="text-text-tertiary hover:text-text-primary transition-colors ml-3"
        title="Dismiss"
      >
        <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24">
          <path d="M18 6L6 18M6 6l12 12" />
        </svg>
      </button>
    </div>
  )
}
