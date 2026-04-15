import { useState, FormEvent } from 'react'
import { Link } from 'react-router-dom'
import { api, APIError } from '../api/client'

export default function ForgotPassword() {
  const [email, setEmail] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [isSubmitting, setIsSubmitting] = useState(false)
  const [sent, setSent] = useState(false)

  async function handleSubmit(e: FormEvent) {
    e.preventDefault()
    setError(null)
    setIsSubmitting(true)
    try {
      await api.auth.resetPassword.forgot(email)
      setSent(true)
    } catch (err: any) {
      setError(err instanceof APIError ? err.message : 'An unexpected error occurred')
    } finally {
      setIsSubmitting(false)
    }
  }

  if (sent) {
    return (
      <div className="min-h-screen flex items-center justify-center bg-surface-0">
        <div className="max-w-md w-full space-y-6 p-8 bg-surface-1 border border-border-default rounded-md">
          <div>
            <h1 className="text-3xl font-bold text-text-primary">Clawvisor</h1>
            <h2 className="mt-2 text-lg text-text-secondary">Check your email</h2>
          </div>
          <p className="text-sm text-text-secondary">
            If an account exists for <span className="font-medium text-text-primary">{email}</span>,
            we've sent a password reset link. The link expires in 1 hour.
          </p>
          <p className="text-sm text-text-tertiary">
            You'll need to verify your identity with a backup code or authenticator app before setting a new password.
          </p>
          <Link
            to="/login"
            className="block w-full py-2 px-4 bg-surface-2 text-text-primary rounded font-medium hover:bg-surface-3 text-center"
          >
            Back to login
          </Link>
        </div>
      </div>
    )
  }

  return (
    <div className="min-h-screen flex items-center justify-center bg-surface-0">
      <div className="max-w-md w-full space-y-8 p-8 bg-surface-1 border border-border-default rounded-md">
        <div>
          <h1 className="text-3xl font-bold text-text-primary">Clawvisor</h1>
          <h2 className="mt-2 text-lg text-text-secondary">Reset your password</h2>
          <p className="mt-1 text-sm text-text-tertiary">
            Enter your email address and we'll send you a link to reset your password.
          </p>
        </div>

        {error && (
          <div className="p-3 bg-danger/10 text-danger rounded text-sm">{error}</div>
        )}

        <form className="space-y-4" onSubmit={handleSubmit}>
          <div>
            <label htmlFor="email" className="block text-sm font-medium text-text-secondary">
              Email
            </label>
            <input
              id="email"
              type="email"
              required
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              className="mt-1 block w-full rounded border border-border-default bg-surface-0 text-text-primary px-3 py-2 focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand"
            />
          </div>

          <button
            type="submit"
            disabled={isSubmitting}
            className="w-full py-2 px-4 bg-brand text-surface-0 rounded font-medium hover:bg-brand-strong disabled:opacity-50"
          >
            {isSubmitting ? 'Sending...' : 'Send reset link'}
          </button>
        </form>

        <p className="text-center text-sm text-text-secondary">
          Remember your password?{' '}
          <Link to="/login" className="text-brand hover:underline">
            Sign in
          </Link>
        </p>
      </div>
    </div>
  )
}
