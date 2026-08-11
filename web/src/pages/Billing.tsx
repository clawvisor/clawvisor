import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { useNavigate } from 'react-router-dom'
import { api } from '../api/client'
import type { AutoRefillSettings, LastAutoRefill, RequestPack } from '../api/client'
import { quotaState } from '../lib/quota'
import { useState } from 'react'

export default function Billing() {
  const navigate = useNavigate()
  const qc = useQueryClient()
  const [promoCode, setPromoCode] = useState('')
  const [promoError, setPromoError] = useState<string | null>(null)
  const [promoSuccess, setPromoSuccess] = useState(false)
  const [packError, setPackError] = useState<string | null>(null)
  const [draftPack, setDraftPack] = useState<string | null>(null)
  const [draftThreshold, setDraftThreshold] = useState<string | null>(null)

  const { data: status, isLoading } = useQuery({
    queryKey: ['billing-status'],
    queryFn: () => api.billing.status(),
    refetchInterval: 60_000,
  })

  const { data: packs } = useQuery({
    queryKey: ['billing-packs'],
    queryFn: () => api.billing.packs(),
    refetchInterval: 60_000,
  })

  const here = window.location.origin + '/dashboard/billing'
  // Which pack is being bought. buyPackMut.isPending is true for the whole
  // mutation regardless of argument, so keying the label off it alone made
  // every card claim it was opening checkout.
  const [buyingPack, setBuyingPack] = useState<string | null>(null)
  const buyPackMut = useMutation({
    mutationFn: (pack: string) => api.billing.buyPack(pack, here + '?purchase=success', here),
    onSuccess: (data) => { window.location.href = data.url },
    onError: (err: Error) => { setPackError(err.message); setBuyingPack(null) },
  })

  const autoRefillMut = useMutation({
    mutationFn: (v: { enabled: boolean; pack?: string; threshold?: number }) =>
      api.billing.setAutoRefill(v.enabled, v.pack, v.threshold),
    onSuccess: () => {
      setPackError(null)
      qc.invalidateQueries({ queryKey: ['billing-packs'] })
      qc.invalidateQueries({ queryKey: ['billing-status'] })
    },
    onError: (err: Error) => setPackError(err.message),
  })

  const portalMut = useMutation({
    mutationFn: () => api.billing.portal(window.location.origin + '/dashboard/billing'),
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
  const isActive = subStatus === 'active'
  const isPastDue = subStatus === 'past_due'
  const isCanceled = subStatus === 'canceled' || subStatus === 'unpaid'
  const isCanceling = !!(status?.cancel_at_period_end && isActive)
  const hasSubscription = plan !== 'none' && subStatus !== 'none'

  // Billing controls key off what Stripe actually charges for, never off the
  // entitlement: a grandfathered account can still hold a paid subscription,
  // and hiding these would leave them unable to update a card or cancel.
  const billedPlan = status?.billed_plan ?? plan
  const isPaying = billedPlan === 'pro' || billedPlan === 'enterprise'

  const requestsUsed = status?.usage?.requests?.used ?? 0
  const requestsLimit = status?.usage?.requests?.limit ?? 0
  const connectionsLimit = status?.usage?.connections?.limit ?? 0
  const requestsPct = requestsLimit > 0 ? Math.min(100, (requestsUsed / requestsLimit) * 100) : 0

  const quota = quotaState(status, packs)
  const unlimited = quota.unlimited
  const allowanceSpent = !unlimited && requestsUsed >= requestsLimit
  const packBalance = quota.packBalance
  const isBlocked = quota.level === 'blocked'
  const isLow = quota.level === 'low'
  const hasCard = !!(packs?.payment_method ?? status?.payment_method)?.present

  return (
    <div className="p-4 sm:p-8 space-y-10">
      <h1 className="text-2xl font-bold text-text-primary">Billing</h1>

      {/* Plan Overview */}
      {hasSubscription && (
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

          {(isPaying || !isGrandfathered) && (
            <div className="flex flex-wrap gap-2 pt-1">
              {isPaying && !isCanceled && (
                <button
                  onClick={() => portalMut.mutate()}
                  disabled={portalMut.isPending}
                  className="px-3 py-1.5 text-sm font-medium rounded-md bg-surface-2 text-text-primary hover:bg-surface-3 transition-colors"
                >
                  {portalMut.isPending ? 'Opening...' : 'Manage subscription'}
                </button>
              )}
              {/* Already inside the hasSubscription branch, so there is no
                  "Get started" state to reach from here. */}
              {(isCanceled || (!isPaying && billedPlan === 'free')) && (
                <button
                  onClick={() => navigate('/pricing')}
                  className="px-3 py-1.5 text-sm font-medium rounded-md bg-brand text-surface-0 hover:bg-brand-strong transition-colors"
                >
                  {isCanceled ? 'Resubscribe' : 'Choose a plan'}
                </button>
              )}
            </div>
          )}
        </div>
      </section>
      )}

      {/* Out of requests */}
      {hasSubscription && !isCanceled && isBlocked && (
        <section>
          <div className="bg-surface-1 border border-danger/40 rounded-md p-5 max-w-lg flex flex-wrap gap-4 items-start">
            <div className="flex-1 min-w-[240px]">
              <h2 className="text-base font-semibold text-danger">Your agents are blocked</h2>
              <p className="text-sm text-text-secondary mt-1">
                You've used all {requestsLimit.toLocaleString()} requests included with {planDisplay} and your
                pack balance is empty. New gateway requests and task creation are being refused.
              </p>
            </div>
            <a
              href="#request-packs"
              className="px-3 py-1.5 text-sm font-medium rounded-md bg-brand text-surface-0 hover:bg-brand-strong transition-colors"
            >
              Buy requests
            </a>
          </div>
        </section>
      )}

      {/* Running low, with nothing to catch them */}
      {hasSubscription && !isCanceled && isLow && (
        <section>
          <div className="bg-surface-1 border border-warning/45 rounded-md p-5 max-w-lg flex flex-wrap gap-4 items-start">
            <div className="flex-1 min-w-[240px]">
              <h2 className="text-base font-semibold text-warning">Running low on requests</h2>
              <p className="text-sm text-text-secondary mt-1">
                {quota.remaining.toLocaleString()} of your {quota.limit.toLocaleString()} monthly requests
                are left, with no request packs and auto-refill off. Buy a pack or turn on auto-refill and
                your agents will keep running.
              </p>
            </div>
            <a
              href="#request-packs"
              className="px-3 py-1.5 text-sm font-medium rounded-md bg-surface-2 text-text-primary hover:bg-surface-3 transition-colors"
            >
              Buy requests
            </a>
          </div>
        </section>
      )}

      {/* Usage */}
      {hasSubscription && !isCanceled && (
        <section className="space-y-4">
          <div>
            <h2 className="text-lg font-semibold text-text-primary">Usage</h2>
            <p className="text-sm text-text-tertiary mt-0.5">Current billing period usage.</p>
          </div>

          <div className="bg-surface-1 border border-border-default rounded-md p-5 max-w-lg space-y-5">
            {/* Requests — the bar measures the monthly allowance and nothing
                else, so a full bar always means the same thing. */}
            <div className="space-y-2">
              <div className="flex items-center justify-between text-sm">
                <span className="text-text-secondary">Requests</span>
                <span className="text-text-primary font-medium tabular-nums">
                  {requestsUsed.toLocaleString()} / {unlimited ? 'Unlimited' : requestsLimit.toLocaleString()}
                </span>
              </div>
              {requestsLimit > 0 && (
                <div className="h-2 bg-surface-2 rounded-full overflow-hidden">
                  <div
                    className={`h-full rounded-full transition-all ${
                      allowanceSpent ? 'bg-danger' : requestsPct > 90 ? 'bg-warning' : 'bg-brand'
                    }`}
                    style={{ width: `${requestsPct}%` }}
                  />
                </div>
              )}
            </div>

            {/* Request packs — a separate reserve: bought, not granted, and
                never reset with the month. */}
            {!unlimited && (
              <div className="pt-4 border-t border-border-default flex items-center justify-between gap-3 flex-wrap">
                <span className="text-sm text-text-secondary">Request packs</span>
                <div className="flex items-center gap-3">
                  <span
                    className={`text-sm font-medium tabular-nums ${
                      packBalance > 0 ? 'text-success' : isBlocked ? 'text-danger' : 'text-text-tertiary'
                    }`}
                  >
                    {packBalance > 0
                      ? `${packBalance.toLocaleString()} remaining`
                      : isBlocked ? '0 remaining' : 'None'}
                  </span>
                  <a
                    href="#request-packs"
                    className={`px-2.5 py-1 text-xs font-medium rounded-md transition-colors ${
                      isBlocked
                        ? 'bg-brand text-surface-0 hover:bg-brand-strong'
                        : 'bg-surface-2 text-text-primary hover:bg-surface-3'
                    }`}
                  >
                    {packBalance > 0 ? 'Buy more' : 'Buy requests'}
                  </a>
                </div>
              </div>
            )}

            {/* Connections */}
            <div className="pt-4 border-t border-border-default flex items-center justify-between text-sm">
              <span className="text-text-secondary">Connections</span>
              <span className="text-text-primary font-medium">
                {connectionsLimit < 0 ? 'Unlimited' : `${connectionsLimit} max`}
              </span>
            </div>
          </div>
        </section>
      )}

      {/* Request packs — meaningless on an unlimited plan. */}
      {hasSubscription && !isCanceled && !unlimited && (
        <section id="request-packs" className="space-y-4 scroll-mt-6">
          <div>
            <h2 className="text-lg font-semibold text-text-primary">Request packs</h2>
            <p className="text-sm text-text-tertiary mt-0.5">
              One-off purchase. Never expires. Used automatically once your monthly requests run out.
            </p>
          </div>

          {packError && <p className="text-sm text-danger">{packError}</p>}

          <div className="grid gap-3 max-w-3xl" style={{ gridTemplateColumns: 'repeat(auto-fit, minmax(178px, 1fr))' }}>
            {(packs?.packs ?? []).map((pk) => {
              return (
                <div
                  key={pk.name}
                  className="relative bg-surface-1 border border-border-default rounded-md p-4 flex flex-col"
                >
                  {pk.discount_pct > 0 && (
                    <span className="absolute -top-2 right-3 text-[10px] font-semibold px-2 py-0.5 rounded-full bg-success text-surface-0">
                      Save {Math.round(pk.discount_pct)}%
                    </span>
                  )}
                  <div className="text-lg font-semibold text-text-primary tabular-nums">
                    {pk.requests.toLocaleString()}{' '}
                    <span className="text-xs font-normal text-text-tertiary">requests</span>
                  </div>
                  <div className="text-sm mt-1.5 tabular-nums">
                    {pk.discount_pct > 0 && (
                      <span className="text-text-tertiary line-through mr-1.5">${(pk.list_cents / 100).toLocaleString()}</span>
                    )}
                    <span className="text-text-primary">${(pk.price_cents / 100).toLocaleString()}</span>
                  </div>
                  <button
                    onClick={() => { setBuyingPack(pk.name); buyPackMut.mutate(pk.name) }}
                    // Every button disables while a purchase is in flight —
                    // two concurrent checkouts is never what the user meant —
                    // but only the clicked one reports progress.
                    disabled={buyPackMut.isPending}
                    className="mt-3 w-full px-3 py-1.5 text-sm font-medium rounded-md transition-colors disabled:opacity-50 bg-surface-2 text-text-primary hover:bg-surface-3"
                  >
                    {buyingPack === pk.name && buyPackMut.isPending ? 'Opening...' : 'Buy'}
                  </button>
                </div>
              )
            })}
          </div>
        </section>
      )}

      {/* Auto-refill */}
      {hasSubscription && !isCanceled && !unlimited && packs && (
        <section className="space-y-4">
          <div>
            <h2 className="text-lg font-semibold text-text-primary">Auto-refill</h2>
            <p className="text-sm text-text-tertiary mt-0.5">
              Buy another pack automatically before your balance runs out.
            </p>
          </div>

          <AutoRefillCard
            packs={packs.packs}
            settings={packs.auto_refill}
            lastRefill={packs.last_auto_refill ?? null}
            hasCard={hasCard}
            saving={autoRefillMut.isPending}
            draftPack={draftPack}
            draftThreshold={draftThreshold}
            onDraftPack={setDraftPack}
            onDraftThreshold={setDraftThreshold}
            onSave={(v) => autoRefillMut.mutate(v)}
            onManageCard={() => portalMut.mutate()}
          />
        </section>
      )}

      {/* Promo Code */}
      {isPaying && !isCanceled && (
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
    </div>
  )
}

function StatusBadge({ status }: { status: string }) {
  const colors: Record<string, string> = {
    active: 'bg-success/10 text-success',
    past_due: 'bg-warning/10 text-warning',
    canceled: 'bg-danger/10 text-danger',
    unpaid: 'bg-danger/10 text-danger',
  }
  const labels: Record<string, string> = {
    active: 'Active',
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

/**
 * Auto-refill settings.
 *
 * Three shapes, because "no saved card" is not the same as "switched off":
 *  - no card, off      -> unavailable, offer to add one
 *  - no card, still on -> a promise we cannot keep; warn, don't prompt
 *  - card present      -> the normal control
 *
 * The threshold must stay strictly below the selected pack's size. At or above
 * it, a completed refill still leaves the balance under the trigger, so every
 * subsequent request buys another pack. The server rejects it too; this
 * explains it in the customer's terms first.
 */
function AutoRefillCard({
  packs, settings, hasCard, saving, draftPack, draftThreshold, lastRefill,
  onDraftPack, onDraftThreshold, onSave, onManageCard,
}: {
  packs: RequestPack[]
  settings: AutoRefillSettings
  lastRefill: LastAutoRefill | null
  hasCard: boolean
  saving: boolean
  draftPack: string | null
  draftThreshold: string | null
  onDraftPack: (v: string) => void
  onDraftThreshold: (v: string) => void
  onSave: (v: { enabled: boolean; pack?: string; threshold?: number }) => void
  onManageCard: () => void
}) {
  const enabled = settings.enabled
  const packName = draftPack ?? settings.pack
  const selected = packs.find((p) => p.name === packName) ?? packs[0]
  const rawThreshold = draftThreshold ?? String(settings.threshold)
  const parsed = parseInt(rawThreshold.replace(/[^0-9]/g, ''), 10)
  const max = selected ? selected.requests - 1 : 0
  const invalid = !Number.isFinite(parsed) || parsed < 1 || parsed > max

  const lapsed = !hasCard && enabled
  const dirty = draftPack !== null || draftThreshold !== null

  return (
    <div
      className={`bg-surface-1 border rounded-md p-5 max-w-lg ${
        lapsed ? 'border-warning/45' : 'border-border-default'
      }`}
    >
      <div className="flex items-start gap-3">
        <button
          role="switch"
          aria-checked={enabled && hasCard}
          aria-label="Auto-refill"
          disabled={saving || (!hasCard && !enabled)}
          onClick={() => onSave({ enabled: !enabled, pack: packName, threshold: parsed })}
          className={`relative w-10 h-[22px] rounded-full flex-none mt-0.5 transition-colors disabled:opacity-45 disabled:cursor-not-allowed ${
            enabled && hasCard ? 'bg-brand' : 'bg-surface-3'
          }`}
        >
          <span
            className={`absolute top-[2.5px] left-[2.5px] w-[17px] h-[17px] rounded-full bg-surface-0 shadow transition-transform ${
              enabled && hasCard ? 'translate-x-4' : ''
            }`}
          />
        </button>
        <div>
          <p className={`text-sm font-semibold ${lapsed ? 'text-warning' : 'text-text-primary'}`}>
            {!hasCard
              ? lapsed ? "Auto-refill can't run" : 'Auto-refill is unavailable'
              : enabled ? 'Auto-refill is on' : 'Auto-refill is off'}
          </p>
          <p className="text-sm text-text-secondary mt-0.5">
            {!hasCard
              ? lapsed
                ? "Auto-refill is on, but there's no payment method saved. Your agents will stop when the balance runs out."
                : 'Add a payment method to turn this on.'
              : enabled && selected && !invalid
                ? `When your balance drops to ${parsed.toLocaleString()} requests, we'll charge your card $${(selected.price_cents / 100).toLocaleString()} for the ${selected.requests.toLocaleString()} pack.`
                : enabled
                  ? 'Fix the refill threshold below to activate.'
                  : 'When your pack balance runs out, agents stop until you buy more.'}
          </p>
        </div>
      </div>

      {lastRefill && (
        <p className="mt-3 text-xs text-text-tertiary">
          Last auto-refill: {lastRefill.requests.toLocaleString()} requests, $
          {(lastRefill.price_cents / 100).toLocaleString()} on{' '}
          {new Date(lastRefill.at).toLocaleDateString()}. A receipt was emailed to you.
        </p>
      )}

      {!hasCard && (
        <div className="mt-4 pt-3.5 border-t border-border-default flex items-center justify-between gap-3 flex-wrap">
          <span className="text-sm text-text-secondary">
            {lapsed
              ? 'Add a card to restore auto-refill.'
              : 'Buying a pack saves your card, which lets you turn this on.'}
          </span>
          <button
            onClick={onManageCard}
            className="px-2.5 py-1 text-xs font-medium rounded-md bg-surface-2 text-text-primary hover:bg-surface-3 transition-colors"
          >
            Add payment method
          </button>
        </div>
      )}

      {hasCard && enabled && (
        <div className="mt-4 pt-4 border-t border-border-default flex flex-wrap gap-5">
          <div className="flex flex-col gap-1.5">
            <label htmlFor="ar-pack" className="text-xs font-semibold text-text-secondary">Pack to buy</label>
            <select
              id="ar-pack"
              value={packName}
              onChange={(e) => onDraftPack(e.target.value)}
              className="px-3 py-1.5 text-sm rounded-md border border-border-default bg-surface-0 text-text-primary focus:outline-none focus:border-brand min-w-[210px]"
            >
              {packs.map((p) => (
                <option key={p.name} value={p.name}>
                  {p.requests.toLocaleString()} requests — ${(p.price_cents / 100).toLocaleString()}
                </option>
              ))}
            </select>
          </div>

          <div className="flex flex-col gap-1.5">
            <label htmlFor="ar-threshold" className="text-xs font-semibold text-text-secondary">
              Refill when balance drops to
            </label>
            <input
              id="ar-threshold"
              inputMode="numeric"
              value={rawThreshold}
              aria-invalid={invalid}
              onChange={(e) => onDraftThreshold(e.target.value)}
              className={`px-3 py-1.5 text-sm rounded-md border bg-surface-0 text-text-primary focus:outline-none min-w-[210px] ${
                invalid ? 'border-danger' : 'border-border-default focus:border-brand'
              }`}
            />
            <span className={`text-xs max-w-[34ch] ${invalid ? 'text-danger' : 'text-text-tertiary'}`}>
              {invalid && selected
                ? `A ${selected.requests.toLocaleString()} pack can't lift your balance above ${(parsed || 0).toLocaleString()}, so this would buy a pack on every request. Use ${max.toLocaleString()} or lower, or choose a larger pack.`
                : selected
                  ? `Must be below ${selected.requests.toLocaleString()} — the size of the pack you're buying.`
                  : ''}
            </span>
          </div>

          {dirty && (
            <div className="flex items-end gap-2">
              <button
                disabled={invalid || saving}
                onClick={() => onSave({ enabled: true, pack: packName, threshold: parsed })}
                className="px-3 py-1.5 text-sm font-medium rounded-md bg-brand text-surface-0 hover:bg-brand-strong transition-colors disabled:opacity-50"
              >
                {saving ? 'Saving...' : 'Save'}
              </button>
              <button
                onClick={() => { onDraftPack(settings.pack); onDraftThreshold(String(settings.threshold)) }}
                className="px-3 py-1.5 text-sm font-medium rounded-md bg-surface-2 text-text-primary hover:bg-surface-3 transition-colors"
              >
                Reset
              </button>
            </div>
          )}
        </div>
      )}
    </div>
  )
}
