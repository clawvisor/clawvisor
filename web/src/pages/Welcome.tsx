import { useState } from 'react'
import { Navigate, useNavigate } from 'react-router-dom'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '../api/client'

export default function Welcome() {
  const navigate = useNavigate()
  const qc = useQueryClient()
  const [promoCode, setPromoCode] = useState('')
  const [promoError, setPromoError] = useState<string | null>(null)
  const [promoApplied] = useState(false)

  // If the user already has an active plan (e.g. grandfathered), skip welcome.
  const { data: billingStatus, isLoading } = useQuery({
    queryKey: ['billing-status'],
    queryFn: () => api.billing.status(),
  })

  if (!isLoading && billingStatus && billingStatus.status !== 'none') {
    return <Navigate to="/dashboard" replace />
  }

  const trialMut = useMutation({
    mutationFn: () => api.billing.startTrial(),
    onSuccess: async () => {
      // If a promo code was entered, apply it after trial starts.
      if (promoCode.trim() && !promoApplied) {
        try {
          await api.billing.applyPromo(promoCode.trim())
        } catch {
          // Non-blocking — trial started, promo can be applied later.
        }
      }
      // Invalidate cached billing status so Dashboard sees the new trial.
      await qc.invalidateQueries({ queryKey: ['billing-status'] })
      navigate('/dashboard')
    },
  })

  const handleStart = () => {
    trialMut.mutate()
  }

  return (
    <div className="min-h-screen bg-surface-0 flex items-center justify-center">
      <div className="max-w-md w-full mx-4">
        <div className="text-center mb-8">
          <div className="flex justify-center mb-4">
            <img src="/favicon.svg" alt="" className="w-10 h-10" />
          </div>
          <h1 className="text-2xl font-bold text-text-primary">Welcome to Clawvisor</h1>
          <p className="text-text-secondary mt-2">
            Start your free 7-day trial with full Pro plan features. No credit card required.
          </p>
        </div>

        <div className="bg-surface-1 border border-border-default rounded-lg p-6 space-y-5">
          <div className="space-y-3">
            <h3 className="text-sm font-semibold text-text-primary">Your trial includes:</h3>
            <ul className="space-y-2 text-sm text-text-secondary">
              <li className="flex items-center gap-2">
                <svg className="w-4 h-4 text-success shrink-0" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M20 6L9 17l-5-5" /></svg>
                Unlimited connections
              </li>
              <li className="flex items-center gap-2">
                <svg className="w-4 h-4 text-success shrink-0" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M20 6L9 17l-5-5" /></svg>
                10,000 gateway requests/month
              </li>
              <li className="flex items-center gap-2">
                <svg className="w-4 h-4 text-success shrink-0" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M20 6L9 17l-5-5" /></svg>
                7 days free, cancel anytime
              </li>
            </ul>
          </div>

          <div className="space-y-2">
            <label className="text-sm font-medium text-text-primary">Promo code <span className="text-text-tertiary font-normal">(optional)</span></label>
            <input
              type="text"
              value={promoCode}
              onChange={(e) => { setPromoCode(e.target.value); setPromoError(null) }}
              placeholder="Enter promo code"
              className="w-full px-3 py-2 text-sm rounded-md border border-border-default bg-surface-0 text-text-primary placeholder:text-text-tertiary focus:outline-none focus:border-brand"
            />
            {promoError && <p className="text-xs text-danger">{promoError}</p>}
          </div>

          <button
            onClick={handleStart}
            disabled={trialMut.isPending}
            className="w-full py-2.5 px-4 rounded-md bg-brand text-surface-0 text-sm font-medium hover:bg-brand-strong transition-colors disabled:opacity-70"
          >
            {trialMut.isPending ? 'Starting trial...' : 'Start free trial'}
          </button>

          {trialMut.isError && (
            <p className="text-xs text-danger text-center">Failed to start trial. Please try again.</p>
          )}
        </div>

        <p className="text-center text-xs text-text-tertiary mt-4">
          After your trial ends, choose a plan starting at $19/month.
        </p>
      </div>
    </div>
  )
}
