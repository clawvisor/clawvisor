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
  // Wait for features to load before branching, so we don't flash the
  // legacy CTA in proxy-lite deployments and let a dismiss-during-flash
  // strand the user on the wrong onboarding state.
  if (features === null) return null

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
      cta={{ to: '/dashboard/home', label: 'Open Home' }}
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
    <div className="dev-banner--info">
      <div className="flex items-start justify-between gap-4 w-full">
        <div className="flex items-start gap-3 min-w-0">
          <span className="dev-badge--brand shrink-0 mt-0.5">setup</span>

          <div className="min-w-0">
            <div className="font-mono text-sm font-medium text-text-primary">{title}</div>
            <p className="text-text-secondary mt-0.5">{body}</p>
            <NavLink
              to={cta.to}
              className="inline-flex items-center gap-1 font-mono text-xs text-brand hover:text-brand-strong transition-colors mt-1.5"
            >
              {cta.label} →
            </NavLink>
          </div>
        </div>

        <button
          onClick={onDismiss}
          className="text-text-tertiary hover:text-text-primary transition-colors shrink-0 mt-0.5 font-mono text-xs"
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
