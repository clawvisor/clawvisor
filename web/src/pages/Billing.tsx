import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { useNavigate } from 'react-router-dom'
import { api } from '../api/client'
import { useState } from 'react'

export default function Billing() {
  const navigate = useNavigate()
  const qc = useQueryClient()
  const [promoCode, setPromoCode] = useState('')
  const [promoError, setPromoError] = useState<string | null>(null)
  const [promoSuccess, setPromoSuccess] = useState(false)

  const { data: status, isLoading } = useQuery({
    queryKey: ['billing-status'],
    queryFn: () => api.billing.status(),
    refetchInterval: 60_000,
  })

  const portalMut = useMutation({
    mutationFn: () => api.billing.portal(window.location.origin + '/dashboard/billing'),
    onSuccess: (data) => { window.location.href = data.url },
  })

  const checkoutMut = useMutation({
    mutationFn: (plan: string) =>
      api.billing.checkout(plan, window.location.origin + '/dashboard/billing?checkout=success', window.location.origin + '/dashboard/billing?checkout=canceled'),
    onSuccess: (data) => { window.location.href = data.url },
  })

  const promoMut = useMutation({
    mutationFn: (code: string) => api.billing.applyPromo(code),
    onSuccess: () => {
      setPromoSuccess(true)
      setPromoCode('')
      setPromoError(null)
      qc.invalidateQueries({ queryKey: ['billing-status'] })
      setTimeout(() => setPromoSuccess(false), 5000)
    },
    onError: (err: Error) => {
      setPromoError(err.message)
      setPromoSuccess(false)
    },
  })

  if (isLoading) {
    return (
      <div className="p-4 sm:p-8">
        <h1 className="text-2xl font-bold text-text-primary mb-6">Billing</h1>
        <div className="text-text-tertiary">Loading billing info...</div>
      </div>
    )
  }

  const plan = status?.plan ?? 'none'
  const planDisplay = status?.plan_display_name ?? plan
  const subStatus = status?.status ?? 'none'
  const isGrandfathered = plan === 'grandfathered'
  const isTrialing = subStatus === 'trialing'
  const isActive = subStatus === 'active' || subStatus === 'trialing'
  const isPastDue = subStatus === 'past_due'
  const isCanceled = subStatus === 'canceled' || subStatus === 'unpaid'
  const isCanceling = !!(status?.cancel_at_period_end && isActive)
  const hasSubscription = plan !== 'none' && subStatus !== 'none'

  const requestsUsed = status?.usage?.requests?.used ?? 0
  const requestsLimit = status?.usage?.requests?.limit ?? 0
  const connectionsLimit = status?.usage?.connections?.limit ?? 0
  const requestsPct = requestsLimit > 0 ? Math.min(100, (requestsUsed / requestsLimit) * 100) : 0

  return (
    <div className="p-4 sm:p-8 space-y-10">
      <h1 className="text-2xl font-bold text-text-primary">Billing</h1>

      {/* Plan Overview */}
      <section className="space-y-4">
        <div>
          <h2 className="text-lg font-semibold text-text-primary">Current plan</h2>
          <p className="text-sm text-text-tertiary mt-0.5">Your subscription and billing details.</p>
        </div>

        <div className="bg-surface-1 border border-border-default rounded-md p-5 max-w-lg space-y-4">
          <div className="flex items-center justify-between">
            <div>
              <span className="text-lg font-semibold text-text-primary">{planDisplay}</span>
              <StatusBadge status={subStatus} />
            </div>
          </div>

          {isTrialing && status?.trial_days_remaining != null && !status?.discount && (
            <div className="text-sm text-text-secondary">
              Trial ends in <span className="font-medium text-text-primary">{status.trial_days_remaining} day{status.trial_days_remaining !== 1 ? 's' : ''}</span>
            </div>
          )}

          {isPastDue && (
            <div className="text-sm text-warning">
              Your payment is past due. Please update your payment method to avoid service interruption.
            </div>
          )}

          {isCanceling && (
            <div className="text-sm text-warning">
              Your subscription will cancel at the end of the current billing period. You can undo this via "Manage subscription" below.
            </div>
          )}

          {status?.discount && (
            <div className="text-sm text-success">
              <span className="font-medium">{status.discount.name || 'Discount applied'}</span>
              {status.discount.percent_off === 100 ? ' — 100% off' : status.discount.percent_off ? ` — ${status.discount.percent_off}% off` : ''}
              {status.discount.ends_at && (
                <span className="text-text-tertiary"> until {new Date(status.discount.ends_at).toLocaleDateString()}</span>
              )}
            </div>
          )}

          {status?.current_period_end && isActive && (
            <div className="text-xs text-text-tertiary">
              Current period ends {new Date(status.current_period_end).toLocaleDateString()}
            </div>
          )}

          {!isGrandfathered && (
            <div className="flex flex-wrap gap-2 pt-1">
              {hasSubscription && !isCanceled && (
                <button
                  onClick={() => portalMut.mutate()}
                  disabled={portalMut.isPending}
                  className="px-3 py-1.5 text-sm font-medium rounded-md bg-surface-2 text-text-primary hover:bg-surface-3 transition-colors"
                >
                  {portalMut.isPending ? 'Opening...' : 'Manage subscription'}
                </button>
              )}
              {(!hasSubscription || (isTrialing && !status?.discount) || isCanceled) && (
                <button
                  onClick={() => navigate('/pricing')}
                  className="px-3 py-1.5 text-sm font-medium rounded-md bg-brand text-surface-0 hover:bg-brand-strong transition-colors"
                >
                  {isCanceled ? 'Resubscribe' : hasSubscription ? 'Choose a plan' : 'Get started'}
                </button>
              )}
            </div>
          )}
        </div>
      </section>

      {/* Usage */}
      {hasSubscription && !isCanceled && (
        <section className="space-y-4">
          <div>
            <h2 className="text-lg font-semibold text-text-primary">Usage</h2>
            <p className="text-sm text-text-tertiary mt-0.5">Current billing period usage.</p>
          </div>

          <div className="bg-surface-1 border border-border-default rounded-md p-5 max-w-lg space-y-5">
            {/* Requests */}
            <div className="space-y-2">
              <div className="flex items-center justify-between text-sm">
                <span className="text-text-secondary">Gateway requests</span>
                <span className="text-text-primary font-medium">
                  {requestsUsed.toLocaleString()} / {requestsLimit < 0 ? 'Unlimited' : requestsLimit.toLocaleString()}
                </span>
              </div>
              {requestsLimit > 0 && (
                <div className="h-2 bg-surface-2 rounded-full overflow-hidden">
                  <div
                    className={`h-full rounded-full transition-all ${
                      requestsPct > 90 ? 'bg-warning' : 'bg-brand'
                    }`}
                    style={{ width: `${requestsPct}%` }}
                  />
                </div>
              )}
              {requestsLimit > 0 && requestsUsed > requestsLimit && (
                <p className="text-xs text-text-tertiary">
                  {(requestsUsed - requestsLimit).toLocaleString()} overage requests will be billed at your plan rate.
                </p>
              )}
            </div>

            {/* Connections */}
            <div className="space-y-1">
              <div className="flex items-center justify-between text-sm">
                <span className="text-text-secondary">Connections</span>
                <span className="text-text-primary font-medium">
                  {connectionsLimit < 0 ? 'Unlimited' : `${connectionsLimit} max`}
                </span>
              </div>
            </div>
          </div>
        </section>
      )}

      {/* Promo Code */}
      {hasSubscription && !isCanceled && !isGrandfathered && (
        <section className="space-y-4">
          <div>
            <h2 className="text-lg font-semibold text-text-primary">Promotion code</h2>
            <p className="text-sm text-text-tertiary mt-0.5">Apply a promotion code to your subscription.</p>
          </div>

          <div className="bg-surface-1 border border-border-default rounded-md p-5 max-w-lg">
            <form
              onSubmit={(e) => {
                e.preventDefault()
                if (promoCode.trim()) promoMut.mutate(promoCode.trim())
              }}
              className="flex gap-2"
            >
              <input
                type="text"
                value={promoCode}
                onChange={(e) => setPromoCode(e.target.value)}
                placeholder="Enter promo code"
                className="flex-1 px-3 py-1.5 text-sm rounded-md border border-border-default bg-surface-0 text-text-primary placeholder:text-text-tertiary focus:outline-none focus:border-brand"
              />
              <button
                type="submit"
                disabled={!promoCode.trim() || promoMut.isPending}
                className="px-3 py-1.5 text-sm font-medium rounded-md bg-surface-2 text-text-primary hover:bg-surface-3 transition-colors disabled:opacity-50"
              >
                {promoMut.isPending ? 'Applying...' : 'Apply'}
              </button>
            </form>
            {promoError && <p className="text-sm text-danger mt-2">{promoError}</p>}
            {promoSuccess && <p className="text-sm text-success mt-2">Promotion code applied!</p>}
          </div>
        </section>
      )}

      {/* Quick Upgrade */}
      {plan === 'starter' && (
        <section className="space-y-4">
          <div>
            <h2 className="text-lg font-semibold text-text-primary">Upgrade</h2>
            <p className="text-sm text-text-tertiary mt-0.5">Get unlimited connections and more requests.</p>
          </div>

          <div className="bg-surface-1 border border-brand/30 rounded-md p-5 max-w-lg">
            <div className="flex items-center justify-between">
              <div>
                <p className="text-sm font-medium text-text-primary">Pro plan - $99/month</p>
                <p className="text-xs text-text-tertiary mt-0.5">Unlimited connections, 10,000 requests/month</p>
              </div>
              <button
                onClick={() => checkoutMut.mutate('pro')}
                disabled={checkoutMut.isPending}
                className="px-3 py-1.5 text-sm font-medium rounded-md bg-brand text-surface-0 hover:bg-brand-strong transition-colors"
              >
                {checkoutMut.isPending ? 'Redirecting...' : 'Upgrade'}
              </button>
            </div>
          </div>
        </section>
      )}
    </div>
  )
}

function StatusBadge({ status }: { status: string }) {
  const colors: Record<string, string> = {
    active: 'bg-success/10 text-success',
    trialing: 'bg-brand-muted text-brand',
    past_due: 'bg-warning/10 text-warning',
    canceled: 'bg-danger/10 text-danger',
    unpaid: 'bg-danger/10 text-danger',
  }
  const labels: Record<string, string> = {
    active: 'Active',
    trialing: 'Trial',
    past_due: 'Past due',
    canceled: 'Canceled',
    unpaid: 'Unpaid',
    none: 'No plan',
  }

  return (
    <span className={`ml-2 inline-flex text-xs font-medium px-2 py-0.5 rounded-full ${colors[status] ?? 'bg-surface-2 text-text-tertiary'}`}>
      {labels[status] ?? status}
    </span>
  )
}
