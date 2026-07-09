import { useState, FormEvent } from 'react'
import { Link } from 'react-router-dom'

// Dedicated SSO sign-in page: collects the user's work email, discovers
// whether their domain has SSO configured, and hands off to the IdP. Kept
// separate from the username/password form so SSO users never touch it.
export default function SSOLogin() {
  const [email, setEmail] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [isSubmitting, setIsSubmitting] = useState(false)

  async function handleSubmit(e: FormEvent) {
    e.preventDefault()
    setError(null)
    setIsSubmitting(true)
    try {
      const res = await fetch(`/api/auth/sso/discover?email=${encodeURIComponent(email)}`)
      // Discovery returns 200 with sso_available=false for the
      // anti-enumeration "not configured" path. Anything other than 2xx is
      // an actual transport/server failure and must not be conflated with
      // "no SSO for this domain".
      if (!res.ok) {
        setError('SSO discovery is temporarily unavailable. Please try again.')
        return
      }
      const data = await res.json()
      if (data.sso_available && data.kickoff_url) {
        // Any pending invite token is already persisted in localStorage by
        // AcceptInvite, so it survives the IdP round-trip and SSOComplete
        // picks it up via nextAfterAuth().
        window.location.href = data.kickoff_url
        return
      }
      if (data.sso_available) {
        // The server says SSO is available but didn't return a kickoff URL —
        // a server-side inconsistency, not a "not configured" domain. Surface
        // it as a transient error so the user retries rather than giving up.
        setError('Single sign-on is temporarily unavailable for your organization. Please try again.')
        return
      }
      setError(
        'Single sign-on is not configured for that email domain. Check the address, or sign in with your password instead.',
      )
    } catch (err: any) {
      setError(err?.message ?? 'SSO discovery failed.')
    } finally {
      setIsSubmitting(false)
    }
  }

  return (
    <div className="min-h-screen flex items-center justify-center bg-surface-0">
      <div className="max-w-md w-full space-y-8 p-8 bg-surface-1 border border-border-default rounded-md">
        <div>
          <h1 className="text-3xl font-bold text-text-primary">Clawvisor</h1>
          <h2 className="mt-2 text-lg text-text-secondary">Sign in with SSO</h2>
          <p className="mt-1 text-sm text-text-tertiary">
            Enter your work email and we&apos;ll redirect you to your organization&apos;s identity provider.
          </p>
        </div>

        {error && <div className="p-3 bg-danger/10 text-danger rounded text-sm">{error}</div>}

        <form className="space-y-4" onSubmit={handleSubmit}>
          <div>
            <label htmlFor="sso-email" className="block text-sm font-medium text-text-secondary">
              Work email
            </label>
            <input
              id="sso-email"
              type="email"
              required
              autoFocus
              placeholder="you@company.com"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              className="mt-1 block w-full rounded border border-border-default bg-surface-0 text-text-primary px-3 py-2 focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand"
            />
          </div>

          <button
            type="submit"
            disabled={isSubmitting || !email}
            className="w-full py-2 px-4 bg-brand text-surface-0 rounded font-medium hover:bg-brand-strong disabled:opacity-50"
          >
            {isSubmitting ? 'Looking up your organization...' : 'Continue'}
          </button>
        </form>

        <div className="text-center text-sm text-text-secondary">
          <Link to="/login" className="text-brand hover:underline">
            Back to sign in
          </Link>
        </div>
      </div>
    </div>
  )
}
