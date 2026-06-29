import { useState, FormEvent } from 'react'
import { useNavigate, useLocation } from 'react-router-dom'
import { api } from '../api/client'
import { useAuth } from '../hooks/useAuth'
import { peekPendingInviteToken } from '../lib/pendingInvite'

function nextAfterAuth(): string {
  const token = peekPendingInviteToken()
  return token ? `/accept-invite?token=${encodeURIComponent(token)}` : '/dashboard'
}

export default function TOTPVerify() {
  const { setSession } = useAuth()
  const navigate = useNavigate()
  const location = useLocation()
  const pendingToken: string = (location.state as any)?.pending_token ?? ''

  const [code, setCode] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [isSubmitting, setIsSubmitting] = useState(false)

  async function handleSubmit(e: FormEvent) {
    e.preventDefault()
    setError(null)
    setIsSubmitting(true)
    try {
      const resp = await api.auth.totp.verify(pendingToken, code)
      setSession(resp.access_token, resp.refresh_token, resp.user)
      navigate(nextAfterAuth())
    } catch (err: any) {
      setError(err.message ?? 'Invalid code')
    } finally {
      setIsSubmitting(false)
    }
  }

  if (!pendingToken) {
    return (
      <div className="min-h-screen flex items-center justify-center bg-surface-0">
        <div className="max-w-md w-full p-8 bg-surface-1 border border-border-default rounded-md text-center">
          <h2 className="text-lg font-semibold text-text-primary mb-2">Session expired</h2>
          <p className="text-sm text-text-secondary mb-4">Please sign in again.</p>
          <button
            onClick={() => navigate('/login')}
            className="py-2 px-4 bg-brand text-surface-0 rounded font-medium hover:bg-brand-strong"
          >
            Go to login
          </button>
        </div>
      </div>
    )
  }

  return (
    <div className="min-h-screen flex items-center justify-center bg-surface-0">
      <div className="max-w-md w-full space-y-6 p-8 bg-surface-1 border border-border-default rounded-md">
        <div>
          <h1 className="text-3xl font-bold text-text-primary">Clawvisor</h1>
          <h2 className="mt-2 text-lg text-text-secondary">Two-factor authentication</h2>
          <p className="mt-1 text-sm text-text-tertiary">
            Enter the 6-digit code from your authenticator app.
          </p>
        </div>

        <form className="space-y-4" onSubmit={handleSubmit}>
          {error && (
            <div className="p-3 bg-danger/10 text-danger rounded text-sm">{error}</div>
          )}

          <div>
            <input
              type="text"
              inputMode="numeric"
              pattern="[0-9]{6}"
              maxLength={6}
              required
              value={code}
              onChange={(e) => setCode(e.target.value.replace(/\D/g, ''))}
              placeholder="000000"
              className="block w-full rounded border border-border-default bg-surface-0 text-text-primary px-3 py-3 text-center text-2xl tracking-widest focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand placeholder:text-text-tertiary"
              autoFocus
              autoComplete="one-time-code"
            />
          </div>

          <button
            type="submit"
            disabled={isSubmitting || code.length !== 6}
            className="w-full py-2 px-4 bg-brand text-surface-0 rounded font-medium hover:bg-brand-strong disabled:opacity-50"
          >
            {isSubmitting ? 'Verifying...' : 'Verify'}
          </button>
        </form>

        <button
          onClick={() => navigate('/login')}
          className="w-full py-2 text-sm text-text-tertiary hover:text-text-primary"
        >
          Back to login
        </button>
      </div>
    </div>
  )
}
