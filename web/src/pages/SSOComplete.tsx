import { useEffect, useRef, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { setAccessToken, api } from '../api/client'
// SSO complete reads tokens from the URL fragment set by the ACS handler,
// then fetches the current user via api.auth.me() (the typed wrapper for
// GET /api/me) so we can hydrate the session with the user record.
import { useAuth } from '../hooks/useAuth'
import { nextAfterAuth } from '../lib/nextAfterAuth'

// SSOComplete is the landing page the ACS endpoint redirects to after
// a successful SAML assertion. The backend appends tokens to the URL
// fragment so they don't appear in server logs / browser history.
// We parse, persist via the existing setSession flow, and route to
// /dashboard (or back to /accept-invite if there's a pending invite).
export default function SSOComplete() {
  const navigate = useNavigate()
  const { setSession } = useAuth()
  const [error, setError] = useState<string | null>(null)
  // StrictMode double-invokes the effect in dev. The first pass parses + wipes
  // the fragment; the second pass would then see no hash and surface a
  // bogus "No session tokens received" error. Guard the one-shot exchange
  // with a ref like OAuthCallback does.
  const didExchange = useRef(false)

  useEffect(() => {
    if (didExchange.current) return
    didExchange.current = true

    const hash = window.location.hash.replace(/^#/, '')
    if (!hash) {
      setError('No session tokens received from SSO redirect.')
      return
    }
    const params = new URLSearchParams(hash)
    const accessToken = params.get('access_token')
    const refreshToken = params.get('refresh_token')
    if (!accessToken) {
      setError('Missing access_token in SSO redirect.')
      return
    }
    // Wipe the fragment so a refresh doesn't replay the tokens.
    window.history.replaceState(null, '', window.location.pathname)
    setAccessToken(accessToken)
    api.auth.me()
      .then((u) => {
        setSession(accessToken, refreshToken ?? undefined, u)
        // Honor any pending invite token so SSO sign-ins consume the
        // invite and join the org, instead of landing on /dashboard with
        // the user still outside the inviting org.
        navigate(nextAfterAuth(), { replace: true })
      })
      .catch((err) => {
        setError(err?.message ?? 'Failed to complete SSO sign-in.')
      })
  }, [navigate, setSession])

  if (error) {
    return (
      <div className="min-h-screen flex items-center justify-center bg-surface-0">
        <div className="max-w-md w-full p-8 bg-surface-1 border border-border-default rounded-md text-center space-y-3">
          <h2 className="text-lg font-semibold text-text-primary">SSO sign-in failed</h2>
          <p className="text-sm text-text-secondary">{error}</p>
          <button onClick={() => navigate('/login')} className="py-2 px-4 bg-brand text-surface-0 rounded font-medium hover:bg-brand-strong">
            Back to login
          </button>
        </div>
      </div>
    )
  }
  return (
    <div className="min-h-screen flex items-center justify-center bg-surface-0">
      <div className="text-text-secondary">Completing SSO sign-in...</div>
    </div>
  )
}
