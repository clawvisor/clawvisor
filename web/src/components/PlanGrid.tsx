import { useQuery, useMutation } from '@tanstack/react-query'
import { useNavigate } from 'react-router-dom'
import { useState } from 'react'
import { api } from '../api/client'
import type { BillingPlan } from '../api/client'
import { useAuth } from '../hooks/useAuth'

const checkIcon = (
  <svg className="w-4 h-4 text-success shrink-0 mt-0.5" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24">
    <path d="M20 6L9 17l-5-5" />
  </svg>
)

function formatPrice(cents: number): string {
  return `$${(cents / 100).toFixed(0)}`
}

function PlanCard({ plan, isCurrent, onSelect, loading, interval, listRate, isAuthenticated }: {
  plan: BillingPlan
  isCurrent: boolean
  onSelect: () => void
  loading: boolean
  interval: 'month' | 'year'
  listRate?: number
  isAuthenticated: boolean
}) {
  const isPro = plan.name === 'pro'
  // Only plans with a real price can go to Stripe Checkout — the API rejects
  // anything but 'pro' with INVALID_PLAN. Free has no price, so an enabled
  // "Get Free" button was a guaranteed "Failed to start checkout".
  const purchasable = !plan.contact_us && ((plan.prices?.length ?? 0) > 0 || (plan.monthly_price ?? 0) > 0)
  // A non-purchasable plan is still actionable for a logged-out visitor: Free
  // is the signup CTA on the public pricing page, and handleSelect routes them
  // to /register without touching Stripe. Disabling it outright killed that
  // path.
  const actionable = purchasable || !isAuthenticated
  const option = plan.prices?.find((o) => o.interval === interval)
  // Fall back to monthly_price, which the API guarantees matches the default
  // checkout cadence.
  const perMonthCents = option ? option.per_month_cents : plan.monthly_price ?? 0

  // Frame annual as what the customer saves, not what they're charged up
  // front. "Billed $1,440 once a year" reads as a bigger number than the $120
  // beside it and buries the reason to choose it.
  const monthlyOption = plan.prices?.find((o) => o.interval === 'month')
  const annualOption = plan.prices?.find((o) => o.interval === 'year')
  const annualSavingCents =
    monthlyOption && annualOption
      ? monthlyOption.per_month_cents * 12 - annualOption.amount_cents
      : 0
  const savingNote =
    interval === 'year' && annualSavingCents > 0
      ? `${formatPrice(annualSavingCents)} saved a year`
      : null

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
            <span className="text-3xl font-bold text-text-primary">{formatPrice(perMonthCents)}</span>
            <span className="text-text-tertiary text-sm">/month</span>
            {savingNote && (
              <p className="text-xs text-success mt-1.5">{savingNote}</p>
            )}
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
          </span>
        </li>
        {plan.included_requests > 0 && (
          <li className="flex items-start gap-2">
            {checkIcon}
            <span>Need more? Top up with request packs{listRate ? ` from $${listRate}/request` : ''}.</span>
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
          disabled={isCurrent || loading || !actionable}
          className={`py-2.5 px-4 rounded-md text-sm font-medium transition-colors ${
            isCurrent || !actionable
              ? 'bg-surface-2 text-text-tertiary cursor-default'
              : isPro
                ? 'bg-brand text-surface-0 hover:bg-brand-strong'
                : 'bg-surface-2 text-text-primary hover:bg-surface-3'
          }`}
        >
          {isCurrent
            ? 'Current plan'
            : loading
              ? 'Redirecting...'
              : !purchasable
                ? (isAuthenticated ? 'Included with every account' : 'Get started free')
                : `Get ${plan.display_name}`}
        </button>
      )}
    </div>
  )
}


/**
 * The plan grid, cadence toggle and checkout flow.
 *
 * Shared by the public pricing page and the billing page's empty state. A user
 * with no plan should be able to pick one where they are, rather than being
 * sent somewhere else to do it — and keeping one implementation means the two
 * surfaces cannot disagree about prices, the selected cadence, or which plan
 * is already active.
 */
export default function PlanGrid() {
  const navigate = useNavigate()
  const { isAuthenticated } = useAuth()
  const [checkoutPlan, setCheckoutPlan] = useState<string | null>(null)

  const { data: plansData, isLoading } = useQuery({
    queryKey: ['billing-plans'],
    queryFn: () => api.billing.plans(),
  })

  // Annual is preselected because it's the better deal — and the card's price
  // and the checkout button always use this same value, so the advertised
  // rate is always the charged rate.
  const [interval, setInterval] = useState<'month' | 'year'>('year')

  const { data: billingStatus } = useQuery({
    queryKey: ['billing-status'],
    queryFn: () => api.billing.status(),
    enabled: isAuthenticated,
  })

  const planSupportsInterval = (planName: string) =>
    !!plansData?.plans
      .find((p) => p.name === planName)
      ?.prices?.some((o) => o.interval === interval)

  const checkoutMut = useMutation({
    mutationFn: (plan: string) =>
      api.billing.checkout(
        plan,
        window.location.origin + '/dashboard/billing?checkout=success',
        window.location.origin + '/pricing?checkout=canceled',
        // Only send the cadence when this plan actually offers it. A plan with
        // no annual price falls back to displaying its monthly rate, so
        // sending interval:'year' anyway would reject at checkout — or worse,
        // bill a cadence the card never showed.
        planSupportsInterval(plan) ? interval : undefined,
      ),
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
    // The API only accepts 'pro'; anything else 400s. The button is already
    // disabled for signed-in users, so this is the second line of defence.
    const target = plansData?.plans.find((p) => p.name === plan)
    if (target && !((target.prices?.length ?? 0) > 0 || (target.monthly_price ?? 0) > 0)) return
    setCheckoutPlan(plan)
    checkoutMut.mutate(plan)
  }

  // Compare against what Stripe bills, not the entitlement: a grandfathered
  // account reports plan "grandfathered", which matches no purchasable plan
  // and would show an existing subscriber an enabled "Get Pro" button.
  const currentPlan = billingStatus?.billed_plan ?? billingStatus?.plan

  // Only offer the toggle when some plan actually has both cadences.
  const withPrices = plansData?.plans.find((p) => (p.prices?.length ?? 0) > 1)
  const hasIntervals = !!withPrices
  const monthlyOpt = withPrices?.prices?.find((o) => o.interval === 'month')
  const annualOpt = withPrices?.prices?.find((o) => o.interval === 'year')
  const annualSavingPct =
    monthlyOpt && annualOpt
      ? Math.round((1 - annualOpt.per_month_cents / monthlyOpt.per_month_cents) * 100)
      : 0

  if (isLoading) {
    return <div className="text-center text-text-tertiary py-12">Loading plans...</div>
  }

  return (
    <>
        {hasIntervals && (
          // Annual sits first and owns the saving badge: after "Monthly" it
          // reads as though monthly is the discounted one. The badge hangs
          // below rather than inline so both buttons stay the same width.
          //
          // pb reserves real flow space for that absolutely-positioned badge;
          // mb is then the actual gap to the cards. Don't collapse these into
          // one margin — the Pro card's "Most popular" pill is itself -top-3,
          // so it reaches up into this gap and lands on the badge.
          <div className="flex justify-center pb-10 mb-10">
            <div className="inline-flex items-center gap-1 rounded-full border border-border-default bg-surface-0 p-1">
              {(['year', 'month'] as const).map((iv) => (
                <span key={iv} className="relative">
                  <button
                    type="button"
                    aria-pressed={interval === iv}
                    onClick={() => setInterval(iv)}
                    className={`rounded-full px-4 py-1.5 text-sm font-medium transition-colors ${
                      interval === iv
                        ? 'bg-brand text-surface-0'
                        : 'text-text-secondary hover:text-text-primary'
                    }`}
                  >
                    {iv === 'month' ? 'Monthly' : 'Annual'}
                  </button>
                  {iv === 'year' && annualSavingPct > 0 && (
                    <span className="pointer-events-none absolute left-1/2 top-full mt-2.5 -translate-x-1/2 whitespace-nowrap text-xs font-medium text-brand">
                      Save {annualSavingPct}%
                    </span>
                  )}
                </span>
              ))}
            </div>
          </div>
        )}

        <div className="grid grid-cols-1 md:grid-cols-3 gap-6">
            {plansData?.plans.map((plan) => (
              <PlanCard
                key={plan.name}
                plan={plan}
                interval={interval}
                listRate={plansData?.list_rate_per_call}
                isAuthenticated={isAuthenticated}
                isCurrent={currentPlan === plan.name}
                onSelect={() => handleSelect(plan.name)}
                loading={checkoutPlan === plan.name && checkoutMut.isPending}
            />
          ))}
        </div>

      {checkoutMut.isError && (
        <p className="text-center text-danger text-sm mt-4">
          Failed to start checkout. Please try again.
        </p>
      )}
    </>
  )
}
