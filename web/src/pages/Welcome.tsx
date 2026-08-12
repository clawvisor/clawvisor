import { Navigate, useNavigate } from 'react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '../api/client'

const checkIcon = (
  <svg className="w-4 h-4 text-success shrink-0" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24">
    <path d="M20 6L9 17l-5-5" />
  </svg>
)

export default function Welcome() {
  const navigate = useNavigate()
  const qc = useQueryClient()

  const { data: billingStatus, isLoading } = useQuery({
    queryKey: ['billing-status'],
    queryFn: () => api.billing.status(),
  })

  // Every number on this page comes from the pricing API. Hardcoding them is
  // how a "$120 advertised / $150 charged" mismatch shipped once already —
  // this screen and /pricing must never be able to disagree.
  const { data: plansData, isLoading: plansLoading } = useQuery({
    queryKey: ['billing-plans'],
    queryFn: () => api.billing.plans(),
  })

  // Org members shouldn't see the personal free-tier explainer — they're
  // already on the org's billing, so bounce them back to the dashboard.
  const { data: memberships, isLoading: membershipsLoading } = useQuery({
    queryKey: ['orgs'],
    queryFn: () => api.orgs.list(),
    staleTime: 60_000,
  })
  const hasOrg = (memberships?.length ?? 0) > 0

  const dismissMut = useMutation({
    mutationFn: () => api.billing.markSplashSeen(),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: ['billing-status'] })
      navigate('/dashboard')
    },
  })

  // Hold the page until billing status and memberships resolve, so an org
  // member never sees a personal plan screen flash by. Plans are allowed to
  // still be in flight — the copy below degrades to figure-free wording
  // rather than blocking the whole screen on a cacheable public endpoint.
  if (isLoading || membershipsLoading) {
    return (
      <div className="min-h-screen bg-surface-0 flex items-center justify-center">
        <div
          role="status"
          aria-label="Loading"
          className="w-6 h-6 border-2 border-border-default border-t-brand rounded-full animate-spin"
        />
      </div>
    )
  }
  // Strictly `!== null`: an already-dismissed account has a timestamp, and a
  // deployment that doesn't track the stamp reports undefined. Neither should
  // be held on this screen.
  if (billingStatus && billingStatus.splash_seen_at !== null) {
    return <Navigate to="/dashboard" replace />
  }
  if (hasOrg) {
    return <Navigate to="/dashboard" replace />
  }

  const freePlan = plansData?.plans.find((p) => p.name === 'free')
  const proPlan = plansData?.plans.find((p) => p.name === 'pro')
  // Read Pro's price off the monthly option, the cadence the upgrade button
  // defaults to. The annual option's per_month_cents is a different (lower)
  // number and quoting it here would under-advertise the default purchase.
  const proMonthlyCents =
    proPlan?.prices?.find((o) => o.interval === 'month')?.per_month_cents ?? proPlan?.monthly_price

  const includedRequests = freePlan?.included_requests
  const requestsLine =
    includedRequests === undefined
      ? 'A monthly request allowance, then top up with request packs'
      : includedRequests < 0
        ? 'Unlimited requests'
        : `${includedRequests.toLocaleString()} requests/month, then top up with request packs`

  return (
    <div className="min-h-screen bg-surface-0 flex items-center justify-center">
      <div className="max-w-md w-full mx-4">
        <div className="text-center mb-8">
          <div className="flex justify-center mb-4">
            <img src="/favicon.svg" alt="" className="w-10 h-10" />
          </div>
          <h1 className="text-2xl font-bold text-text-primary">Welcome to Clawvisor</h1>
          <p className="text-text-secondary mt-2">
            You're on the free plan. No credit card required.
          </p>
        </div>

        <div className="bg-surface-1 border border-border-default rounded-lg p-6 space-y-5">
          <div className="space-y-3">
            <h3 className="text-sm font-semibold text-text-primary">Your free plan includes:</h3>
            <ul className="space-y-2 text-sm text-text-secondary">
              <li className="flex items-center gap-2">
                {checkIcon}
                Unlimited connections
              </li>
              <li className="flex items-center gap-2">
                {checkIcon}
                {plansLoading ? (
                  <span className="inline-block h-4 w-56 rounded bg-surface-2 animate-pulse" aria-hidden="true" />
                ) : (
                  requestsLine
                )}
              </li>
              <li className="flex items-center gap-2">
                {checkIcon}
                Free forever, upgrade anytime
              </li>
            </ul>
          </div>

          <button
            onClick={() => dismissMut.mutate()}
            disabled={dismissMut.isPending}
            className="w-full py-2.5 px-4 rounded-md bg-brand text-surface-0 text-sm font-medium hover:bg-brand-strong transition-colors disabled:opacity-70"
          >
            {dismissMut.isPending ? 'One moment...' : 'Get started'}
          </button>

          {dismissMut.isError && (
            <p className="text-xs text-danger text-center">Something went wrong. Please try again.</p>
          )}
        </div>

        {proMonthlyCents !== undefined && (
          <p className="text-center text-xs text-text-tertiary mt-4">
            Need more? Upgrade to Pro from ${(proMonthlyCents / 100).toFixed(0)}/month.
          </p>
        )}
      </div>
    </div>
  )
}
