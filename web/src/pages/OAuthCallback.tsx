import { useEffect, useState, useRef } from 'react'
import { useNavigate, useSearchParams } from 'react-router-dom'
import { api, APIError } from '../api/client'
import { useAuth } from '../hooks/useAuth'

export default function OAuthCallback() {
  const { setSession, isAuthenticated } = useAuth()
  const navigate = useNavigate()
  const [searchParams] = useSearchParams()
  const [error, setError] = useState<string | null>(null)
  const [destination, setDestination] = useState<string | null>(null)
  const didExchange = useRef(false)

  // Step 1: Exchange the code with the backend.
  useEffect(() => {
    if (didExchange.current) return
    didExchange.current = true

    const code = searchParams.get('code')
    const errorParam = searchParams.get('error')

    if (errorParam) {
      setError(errorParam === 'access_denied' ? 'Sign-in was cancelled' : `OAuth error: ${errorParam}`)
      return
    }

    if (!code) {
      setError('No authorization code received')
      return
    }

    const redirectUri = `${window.location.origin}/login/oauth/callback`
    api.auth.google.exchange(code, redirectUri)
      .then((resp) => {
        if (resp.status === 'requires_mfa') {
          navigate('/mfa-verify', {
            state: {
              pending_token: resp.pending_token,
              mfa_methods: resp.mfa_methods,
            },
            replace: true,
          })
          return
        }
        // Set session — navigate after isAuthenticated becomes true.
        setSession(resp.access_token!, resp.refresh_token!, resp.user!)
        setDestination('/onboarding')
      })
      .catch((err) => {
        if (err instanceof APIError && err.waitlistAvailable) {
          const email = err.extra?.email as string | undefined
          const params = email ? `?email=${encodeURIComponent(email)}` : ''
          navigate(`/waitlist${params}`, { replace: true })
          return
        }
        setError(err instanceof APIError ? err.message : 'Failed to sign in')
      })
  }, [searchParams, setSession, navigate])

  // Step 2: Navigate once React state has settled and user is authenticated.
  useEffect(() => {
    if (destination && isAuthenticated) {
      navigate(destination, { replace: true })
    }
  }, [destination, isAuthenticated, navigate])

  if (error) {
    return (
      <div className="min-h-screen flex items-center justify-center bg-surface-0">
        <div className="max-w-md w-full p-8 bg-surface-1 border border-border-default rounded-md text-center space-y-4">
          <h2 className="text-lg font-semibold text-text-primary">Sign-in failed</h2>
          <div className="p-3 bg-danger/10 text-danger rounded text-sm">{error}</div>
          <button
            onClick={() => navigate('/login')}
            className="py-2 px-4 bg-brand text-surface-0 rounded font-medium hover:bg-brand-strong"
          >
            Back to login
          </button>
        </div>
      </div>
    )
  }

  return (
    <div className="min-h-screen flex items-center justify-center bg-surface-0">
      <div className="text-text-secondary">Signing in...</div>
    </div>
  )
}
