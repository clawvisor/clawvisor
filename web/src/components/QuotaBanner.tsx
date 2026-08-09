import { useQuery } from '@tanstack/react-query'
import { useLocation, useNavigate } from 'react-router-dom'
import { api } from '../api/client'
import { quotaState } from '../lib/quota'

/**
 * App-wide warning when an account is out of requests, or heading there with
 * nothing to catch it.
 *
 * The billing page already says this, but by definition an affected customer
 * is somewhere else — watching an agent run stall, or staring at a failed
 * task. Without a global surface the only signal is a 402 buried in a log.
 *
 * Deliberately not dismissible: unlike onboarding, this is not a nudge toward
 * something optional. It reports that the product has stopped working, or is
 * about to. It disappears on its own once the customer buys requests or turns
 * on auto-refill.
 */
export default function QuotaBanner() {
  const navigate = useNavigate()
  const location = useLocation()

  const { data: status } = useQuery({
    queryKey: ['billing-status'],
    queryFn: () => api.billing.status(),
    refetchInterval: 60_000,
  })

  // The billing page carries its own, richer version of this.
  if (location.pathname.startsWith('/dashboard/billing')) return null
  if (!status) return null

  const quota = quotaState(status)
  if (quota.level === 'ok') return null

  const copy = {
    blocked: {
      title: 'Your agents are blocked',
      body: "You're out of requests. New gateway requests and task creation are being refused until you top up.",
      action: 'Buy requests',
      danger: true,
    },
    'refill-broken': {
      title: "Auto-refill can't run",
      body: "Auto-refill is on, but there's no payment method saved. Your agents will stop when your balance runs out.",
      action: 'Add payment method',
      danger: false,
    },
    low: {
      title: 'Running low on requests',
      body: `${quota.remaining.toLocaleString()} of your ${quota.limit.toLocaleString()} monthly requests are left, with no request packs and auto-refill off. Your agents will stop when these run out.`,
      action: 'Buy requests',
      danger: false,
    },
  }[quota.level]

  const tone = copy.danger
    ? { border: 'border-danger/40', bg: 'bg-danger/[0.06]', text: 'text-danger' }
    : { border: 'border-warning/45', bg: 'bg-warning/[0.07]', text: 'text-warning' }

  return (
    <div className={`mb-6 rounded-lg border ${tone.border} ${tone.bg} px-4 py-3.5`}>
      <div className="flex items-start gap-3 flex-wrap">
        <div className="flex-1 min-w-[260px]">
          <p className={`text-sm font-semibold ${tone.text}`}>{copy.title}</p>
          <p className="text-sm text-text-secondary mt-0.5">{copy.body}</p>
        </div>
        <button
          onClick={() => navigate('/dashboard/billing')}
          className={`px-3 py-1.5 text-sm font-medium rounded-md transition-colors ${
            copy.danger
              ? 'bg-brand text-surface-0 hover:bg-brand-strong'
              : 'bg-surface-2 text-text-primary hover:bg-surface-3'
          }`}
        >
          {copy.action}
        </button>
      </div>
    </div>
  )
}
