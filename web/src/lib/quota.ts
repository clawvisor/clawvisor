import type { BillingStatus, PackStatus } from '../api/client'

/**
 * Warn once remaining capacity drops below this share of the monthly
 * allowance — but only for accounts with no cushion at all (no pack balance,
 * auto-refill off). Those are the only ones that hit a hard stop, and a
 * quarter of a month's allowance is roughly a week of runway at a steady rate:
 * enough time to buy a pack before anything breaks.
 */
export const LOW_CAPACITY_THRESHOLD = 0.25

export type QuotaLevel = 'ok' | 'low' | 'refill-broken' | 'blocked'

export interface QuotaState {
  level: QuotaLevel
  used: number
  limit: number
  /** Requests left in the monthly allowance. Infinity on unmetered plans. */
  remaining: number
  packBalance: number
  unlimited: boolean
}

/**
 * The single source of truth for how close an account is to stopping.
 *
 * Both the app-wide banner and the billing page read this, so they can't
 * disagree about who is in trouble — the same class of drift that let the
 * pricing page advertise a rate checkout didn't charge.
 *
 * Ordered by severity: a blocked account is not also "low", and an account
 * whose auto-refill can't charge is a bigger problem than one that simply
 * hasn't set it up.
 */
export function quotaState(
  status: BillingStatus | undefined,
  packs?: PackStatus,
): QuotaState {
  const limit = status?.usage?.requests?.limit ?? 0
  const used = status?.usage?.requests?.used ?? 0
  const packBalance = packs?.balance ?? status?.usage?.pack_balance ?? 0
  const unlimited = limit < 0

  const base = {
    used,
    limit,
    packBalance,
    unlimited,
    remaining: unlimited ? Infinity : Math.max(0, limit - used),
  }

  if (unlimited || limit === 0) return { ...base, level: 'ok' }

  // A dead subscription is a different problem with a different fix, and the
  // plan card already says so — calling it "out of requests" would send the
  // customer to buy a pack that would not help.
  if (status?.status && status.status !== 'active' && status.status !== 'past_due') {
    return { ...base, level: 'ok' }
  }

  // Nothing is being refused when the kill switch is off, so do not announce
  // that agents are blocked.
  if (status?.enforcement?.enabled === false) return { ...base, level: 'ok' }

  const autoRefillOn = !!(packs?.auto_refill ?? status?.auto_refill)?.enabled
  const hasCard = !!(packs?.payment_method ?? status?.payment_method)?.present
  const allowanceSpent = used >= limit

  // A working auto-refill means the next request tops the balance up, so the
  // account is not blocked — the server's own gate applies the same rule, and
  // disagreeing here would announce a wall that never arrives.
  const refillWillWork = autoRefillOn && hasCard
  if (allowanceSpent && packBalance <= 0 && !refillWillWork) {
    return { ...base, level: 'blocked' }
  }

  // Auto-refill that can't charge anything is a promise guaranteed to fail at
  // the moment it matters, so it outranks simply running low.
  if (autoRefillOn && !hasCard) return { ...base, level: 'refill-broken' }

  // Running low only matters when nothing would catch them: a pack balance or
  // a working auto-refill both mean the wall isn't real.
  // Strictly below the threshold. An account passes through exactly 25% for
  // the width of a single request, so `<=` would buy no real warning time —
  // it would only make the boundary disagree with the stated rule.
  const noCushion = packBalance <= 0 && !autoRefillOn
  if (noCushion && base.remaining < limit * LOW_CAPACITY_THRESHOLD) {
    return { ...base, level: 'low' }
  }

  return { ...base, level: 'ok' }
}
