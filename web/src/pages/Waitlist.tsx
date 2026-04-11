import { useState, FormEvent } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import { api, APIError } from '../api/client'

export default function Waitlist() {
  const [searchParams] = useSearchParams()
  const [email, setEmail] = useState(searchParams.get('email') ?? '')
  const [error, setError] = useState<string | null>(null)
  const [isSubmitting, setIsSubmitting] = useState(false)
  const [joined, setJoined] = useState(false)

  async function handleSubmit(e: FormEvent) {
    e.preventDefault()
    setError(null)
    setIsSubmitting(true)
    try {
      await api.auth.joinWaitlist(email)
      setJoined(true)
    } catch (err) {
      if (err instanceof APIError) {
        setError(err.message)
      } else {
        setError('An unexpected error occurred')
      }
    } finally {
      setIsSubmitting(false)
    }
  }

  if (joined) {
    return (
      <div className="min-h-screen flex items-center justify-center bg-surface-0">
        <div className="max-w-md w-full p-8 bg-surface-1 border border-border-default rounded-md text-center space-y-4">
          <h1 className="text-3xl font-bold text-text-primary">Clawvisor</h1>
          <h2 className="text-lg text-text-secondary">You're on the waitlist!</h2>
          <p className="text-sm text-text-tertiary">
            We'll let you know when your account is ready. Keep an eye on <strong>{email}</strong> for updates.
          </p>
          <p className="text-sm text-text-tertiary">
            In the meantime, <a href="https://discord.gg/sZAV4xfAKd" target="_blank" rel="noopener noreferrer" className="text-brand hover:underline">join our Discord</a> to connect with the community, or get started right away with the <a href="https://github.com/clawvisor/clawvisor" target="_blank" rel="noopener noreferrer" className="text-brand hover:underline">self-hosted version</a>.
          </p>
          <div className="flex gap-3 justify-center">
            <a href="https://discord.gg/sZAV4xfAKd" target="_blank" rel="noopener noreferrer" className="py-2 px-4 bg-brand text-surface-0 rounded font-medium hover:bg-brand-strong">
              Join Discord
            </a>
            <Link to="/login" className="py-2 px-4 border border-border-default text-text-secondary rounded font-medium hover:bg-surface-2">
              Back to login
            </Link>
          </div>
        </div>
      </div>
    )
  }

  return (
    <div className="min-h-screen flex items-center justify-center bg-surface-0">
      <div className="max-w-md w-full space-y-6 p-8 bg-surface-1 border border-border-default rounded-md">
        <div>
          <h1 className="text-3xl font-bold text-text-primary">Clawvisor</h1>
          <h2 className="mt-2 text-lg text-text-secondary">Join the waitlist</h2>
          <p className="mt-2 text-sm text-text-tertiary">
            Registration is currently closed. Leave your email and we'll notify you when a spot opens up.
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
            {isSubmitting ? 'Joining...' : 'Join the waitlist'}
          </button>
        </form>

        <p className="text-center text-sm text-text-secondary">
          Already have an account?{' '}
          <Link to="/login" className="text-brand hover:underline">
            Sign in
          </Link>
        </p>
      </div>
    </div>
  )
}
