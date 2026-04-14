import { useQuery, useMutation } from '@tanstack/react-query'
import { useNavigate } from 'react-router-dom'
import { api, BillingPlan } from '../api/client'
import { useAuth } from '../hooks/useAuth'
import { useState } from 'react'

const checkIcon = (
  <svg className="w-4 h-4 text-success shrink-0 mt-0.5" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24">
    <path d="M20 6L9 17l-5-5" />
  </svg>
)

function formatPrice(cents: number): string {
  return `$${(cents / 100).toFixed(0)}`
}

function PlanCard({ plan, isCurrent, onSelect, loading }: {
  plan: BillingPlan
  isCurrent: boolean
  onSelect: () => void
  loading: boolean
}) {
  const isPro = plan.name === 'pro'

  return (
    <div className={`relative flex flex-col rounded-lg border p-6 ${
      isPro
        ? 'border-brand bg-surface-1 shadow-md'
        : 'border-border-default bg-surface-1'
    }`}>
      {isPro && (
        <span className="absolute -top-3 left-1/2 -translate-x-1/2 bg-brand text-surface-0 text-xs font-semibold px-3 py-1 rounded-full">
          Most popular
        </span>
      )}
      <h3 className="text-lg font-semibold text-text-primary">{plan.display_name}</h3>
      <div className="mt-3 mb-5">
        {plan.contact_us ? (
          <span className="text-2xl font-bold text-text-primary">Custom</span>
        ) : (
          <>
            <span className="text-3xl font-bold text-text-primary">{formatPrice(plan.monthly_price!)}</span>
            <span className="text-text-tertiary text-sm">/month</span>
          </>
        )}
      </div>
      <ul className="space-y-2.5 text-sm text-text-secondary flex-1 mb-6">
        <li className="flex items-start gap-2">
          {checkIcon}
          <span>{plan.max_connections < 0 ? 'Unlimited' : plan.max_connections} connections</span>
        </li>
        <li className="flex items-start gap-2">
          {checkIcon}
          <span>
            {plan.included_requests < 0 ? 'Unlimited' : plan.included_requests.toLocaleString()} requests/month
            {plan.soft_cap_note && <span className="block text-xs text-text-tertiary mt-0.5">{plan.soft_cap_note}</span>}
          </span>
        </li>
        {plan.overage_per_request != null && plan.overage_per_request > 0 && (
          <li className="flex items-start gap-2">
            {checkIcon}
            <span>${plan.overage_per_request}/request overage</span>
          </li>
        )}
        {plan.contact_us && (
          <>
            <li className="flex items-start gap-2">
              {checkIcon}
              <span>Dedicated support</span>
            </li>
            <li className="flex items-start gap-2">
              {checkIcon}
              <span>Custom integrations</span>
            </li>
          </>
        )}
      </ul>
      {plan.contact_us ? (
        <a
          href="mailto:sales@clawvisor.com"
          className="block text-center py-2.5 px-4 rounded-md border border-border-default text-text-primary text-sm font-medium hover:bg-surface-2 transition-colors"
        >
          Contact us
        </a>
      ) : (
        <button
          onClick={onSelect}
          disabled={isCurrent || loading}
          className={`py-2.5 px-4 rounded-md text-sm font-medium transition-colors ${
            isCurrent
              ? 'bg-surface-2 text-text-tertiary cursor-default'
              : isPro
                ? 'bg-brand text-surface-0 hover:bg-brand-strong'
                : 'bg-surface-2 text-text-primary hover:bg-surface-3'
          }`}
        >
          {isCurrent ? 'Current plan' : loading ? 'Redirecting...' : `Get ${plan.display_name}`}
        </button>
      )}
    </div>
  )
}

export default function Pricing() {
  const navigate = useNavigate()
  const { isAuthenticated } = useAuth()
  const [checkoutPlan, setCheckoutPlan] = useState<string | null>(null)

  const { data: plansData, isLoading } = useQuery({
    queryKey: ['billing-plans'],
    queryFn: () => api.billing.plans(),
  })

  const { data: billingStatus } = useQuery({
    queryKey: ['billing-status'],
    queryFn: () => api.billing.status(),
    enabled: isAuthenticated,
  })

  const checkoutMut = useMutation({
    mutationFn: (plan: string) =>
      api.billing.checkout(plan, window.location.origin + '/dashboard/billing?checkout=success', window.location.origin + '/pricing?checkout=canceled'),
    onSuccess: (data) => {
      window.location.href = data.url
    },
    onError: () => {
      setCheckoutPlan(null)
    },
  })

  const handleSelect = (plan: string) => {
    if (!isAuthenticated) {
      navigate('/register')
      return
    }
    setCheckoutPlan(plan)
    checkoutMut.mutate(plan)
  }

  const currentPlan = billingStatus?.plan

  return (
    <div className="min-h-screen bg-surface-0">
      <div className="max-w-4xl mx-auto px-6 py-16">
        <div className="text-center mb-12">
          <h1 className="text-3xl font-bold text-text-primary mb-3">Choose your plan</h1>
          <p className="text-text-secondary max-w-lg mx-auto">
            Get started for free. Upgrade when you need more.
          </p>
        </div>

        {isLoading ? (
          <div className="text-center text-text-tertiary py-12">Loading plans...</div>
        ) : (
          <div className="grid grid-cols-1 md:grid-cols-3 gap-6">
            {plansData?.plans.map((plan) => (
              <PlanCard
                key={plan.name}
                plan={plan}
                isCurrent={currentPlan === plan.name}
                onSelect={() => handleSelect(plan.name)}
                loading={checkoutPlan === plan.name && checkoutMut.isPending}
              />
            ))}
          </div>
        )}

        {checkoutMut.isError && (
          <p className="text-center text-danger text-sm mt-4">
            Failed to start checkout. Please try again.
          </p>
        )}

        {/* Back to dashboard link */}
        {isAuthenticated && (
          <div className="text-center mt-10">
            <button
              onClick={() => navigate('/dashboard')}
              className="text-sm text-text-tertiary hover:text-text-primary transition-colors"
            >
              Back to dashboard
            </button>
          </div>
        )}
      </div>
    </div>
  )
}
