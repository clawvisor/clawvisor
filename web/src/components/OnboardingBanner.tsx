import { useQuery } from '@tanstack/react-query'
import { NavLink } from 'react-router-dom'
import { useState } from 'react'
import { api } from '../api/client'
import { useAuth } from '../hooks/useAuth'

const DISMISS_KEY = 'clawvisor_onboarding_dismissed'

export default function OnboardingBanner() {
  const { features } = useAuth()
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

  function handleDismiss() {
    localStorage.setItem(DISMISS_KEY, '1')
    setDismissed(true)
  }

  // Proxy-lite onboarding flow: agent first, then accounts.
  if (features?.proxy_lite) {
    const { title, body, cta } = !hasAgent
      ? {
          title: 'Connect an agent to get started',
          body: 'Hook up an AI agent so Clawvisor can sit between it and the services it uses.',
          cta: { to: '/dashboard/agents', label: 'Connect an agent' },
        }
      : {
          title: 'Connect accounts to level up your agents',
          body: 'Give your agents managed access to tools like Gmail, GitHub, and Slack — without handing over secrets.',
          cta: { to: '/dashboard/accounts', label: 'Connect an account' },
        }

    return <BannerCard title={title} body={body} cta={cta} onDismiss={handleDismiss} />
  }

  // Pre-proxy fallback: original "Finish setting up" copy.
  const missing: string[] = []
  if (!hasService) missing.push('a service')
  if (!hasAgent) missing.push('an agent')
  const missingText = missing.join(' and ')

  return (
    <BannerCard
      title="Finish setting up Clawvisor"
      body={`Connect ${missingText} to get task approvals and personalized suggestions.`}
      cta={{ to: '/dashboard/get-started', label: 'Open Get Started' }}
      onDismiss={handleDismiss}
    />
  )
}

function BannerCard({
  title,
  body,
  cta,
  onDismiss,
}: {
  title: string
  body: string
  cta: { to: string; label: string }
  onDismiss: () => void
}) {
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
            <div className="font-medium text-text-primary">{title}</div>
            <p className="text-text-secondary mt-0.5">{body}</p>
            <NavLink
              to={cta.to}
              className="inline-flex items-center gap-1 text-brand font-medium hover:text-brand-strong transition-colors mt-1.5"
            >
              {cta.label}
              <svg className="w-3.5 h-3.5" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24">
                <path d="M9 5l7 7-7 7" />
              </svg>
            </NavLink>
          </div>
        </div>

        <button
          onClick={onDismiss}
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
