import { useEffect, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { setAccessToken, api } from '../api/client'
// SSO complete reads tokens from the URL fragment set by the ACS handler.
// We can't use the typed api.auth.* since /api/me lives on the top-level
// api object (api.me).
import { useAuth } from '../hooks/useAuth'

// SSOComplete is the landing page the ACS endpoint redirects to after
// a successful SAML assertion. The backend appends tokens to the URL
// fragment so they don't appear in server logs / browser history.
// We parse, persist via the existing setSession flow, and route to
// /dashboard.
export default function SSOComplete() {
  const navigate = useNavigate()
  const { setSession } = useAuth()
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
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
        navigate('/dashboard', { replace: true })
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
