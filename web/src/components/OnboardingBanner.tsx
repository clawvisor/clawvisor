import { useQuery } from '@tanstack/react-query'
import { NavLink } from 'react-router-dom'
import { useState } from 'react'
import { api } from '../api/client'

const DISMISS_KEY = 'clawvisor_onboarding_dismissed'

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
  if (services === undefined || agents === undefined) return null

  const hasService = (services.services ?? []).some(
    (s: { status: string; requires_activation?: boolean }) =>
      s.status === 'activated' && (s.requires_activation ?? true),
  )
  const hasAgent = (agents ?? []).length > 0

  if (hasService && hasAgent) return null

  const missing: string[] = []
  if (!hasService) missing.push('a service')
  if (!hasAgent) missing.push('an agent')
  const missingText = missing.join(' and ')

  function handleDismiss() {
    localStorage.setItem(DISMISS_KEY, '1')
    setDismissed(true)
  }

  return (
    <div className="mx-4 mt-3 px-4 py-3.5 rounded-md bg-brand-muted border border-brand/30 text-sm">
      <div className="flex items-start justify-between gap-4">
        <div className="flex items-start gap-3 min-w-0">
          <div className="shrink-0 w-8 h-8 rounded-full bg-brand/15 text-brand flex items-center justify-center mt-0.5">
            <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" viewBox="0 0 24 24">
              <path d="M4.5 16.5c-1.5 1.26-2 5-2 5s3.74-.5 5-2c.71-.84.7-2.13-.09-2.91a2.18 2.18 0 0 0-2.91-.09z" />
              <path d="m12 15-3-3a22 22 0 0 1 2-3.95A12.88 12.88 0 0 1 22 2c0 2.72-.78 7.5-6 11a22.35 22.35 0 0 1-4 2z" />
              <path d="M9 12H4s.55-3.03 2-4c1.62-1.08 5 0 5 0" />
              <path d="M12 15v5s3.03-.55 4-2c1.08-1.62 0-5 0-5" />
            </svg>
          </div>

          <div className="min-w-0">
            <div className="font-medium text-text-primary">Finish setting up Clawvisor</div>
            <p className="text-text-secondary mt-0.5">
              Connect {missingText} to unlock task approvals and personalized suggestions.
            </p>
            <NavLink
              to="/dashboard/get-started"
              className="inline-flex items-center gap-1 text-brand font-medium hover:text-brand-strong transition-colors mt-1.5"
            >
              Open Get Started
              <svg className="w-3.5 h-3.5" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24">
                <path d="M9 5l7 7-7 7" />
              </svg>
            </NavLink>
          </div>
        </div>

        <button
          onClick={handleDismiss}
          className="text-text-tertiary hover:text-text-primary transition-colors shrink-0 mt-0.5"
          title="Dismiss"
        >
          <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24">
            <path d="M18 6L6 18M6 6l12 12" />
          </svg>
        </button>
      </div>
    </div>
  )
}
