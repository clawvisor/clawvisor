import { Navigate, useNavigate } from 'react-router-dom'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '../api/client'

export default function Welcome() {
  const navigate = useNavigate()
  const qc = useQueryClient()

  // If the user already has an active plan (e.g. grandfathered), skip welcome.
  const { data: billingStatus, isLoading } = useQuery({
    queryKey: ['billing-status'],
    queryFn: () => api.billing.status(),
  })

  if (!isLoading && billingStatus && billingStatus.status !== 'none') {
    return <Navigate to="/dashboard" replace />
  }

  const activateMut = useMutation({
    mutationFn: () => api.billing.activateFreeTier(),
    onSuccess: async () => {
      await qc.invalidateQueries({ queryKey: ['billing-status'] })
      navigate('/dashboard')
    },
  })

  return (
    <div className="min-h-screen bg-surface-0 flex items-center justify-center">
      <div className="max-w-md w-full mx-4">
        <div className="text-center mb-8">
          <div className="flex justify-center mb-4">
            <img src="/favicon.svg" alt="" className="w-10 h-10" />
          </div>
          <h1 className="text-2xl font-bold text-text-primary">Welcome to Clawvisor</h1>
          <p className="text-text-secondary mt-2">
            Get started for free. No credit card required.
          </p>
        </div>

        <div className="bg-surface-1 border border-border-default rounded-lg p-6 space-y-5">
          <div className="space-y-3">
            <h3 className="text-sm font-semibold text-text-primary">Your free plan includes:</h3>
            <ul className="space-y-2 text-sm text-text-secondary">
              <li className="flex items-center gap-2">
                <svg className="w-4 h-4 text-success shrink-0" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M20 6L9 17l-5-5" /></svg>
                Unlimited connections
              </li>
              <li className="flex items-center gap-2">
                <svg className="w-4 h-4 text-success shrink-0" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M20 6L9 17l-5-5" /></svg>
                <span className="line-through text-text-tertiary">1,000 gateway requests/month</span>{' '}
                <span className="text-brand font-medium">Uncapped during early access</span>
              </li>
              <li className="flex items-center gap-2">
                <svg className="w-4 h-4 text-success shrink-0" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M20 6L9 17l-5-5" /></svg>
                Free forever, upgrade anytime
              </li>
            </ul>
          </div>

          <button
            onClick={() => activateMut.mutate()}
            disabled={activateMut.isPending}
            className="w-full py-2.5 px-4 rounded-md bg-brand text-surface-0 text-sm font-medium hover:bg-brand-strong transition-colors disabled:opacity-70"
          >
            {activateMut.isPending ? 'Activating...' : 'Get started'}
          </button>

          {activateMut.isError && (
            <p className="text-xs text-danger text-center">Something went wrong. Please try again.</p>
          )}
        </div>

        <p className="text-center text-xs text-text-tertiary mt-4">
          Need more? Upgrade to Pro for $199/month.
        </p>
      </div>
    </div>
  )
}
