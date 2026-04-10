import { useEffect, useRef, useState } from 'react'
import { Navigate, useSearchParams } from 'react-router-dom'
import { useAuth } from '../hooks/useAuth'
import { api, setAccessToken } from '../api/client'

const REFRESH_TOKEN_KEY = 'clawvisor_refresh_token'

export default function MagicLink() {
  const { isAuthenticated, isLoading } = useAuth()
  const [searchParams] = useSearchParams()
  const magicToken = searchParams.get('token')
  const [error, setError] = useState<string | null>(null)
  const [exchanging, setExchanging] = useState(!!magicToken)
  const didExchange = useRef(false)

  useEffect(() => {
    if (didExchange.current || !magicToken) return
    didExchange.current = true

    api.auth.magic(magicToken)
      .then((resp) => {
        setAccessToken(resp.access_token)
        localStorage.setItem(REFRESH_TOKEN_KEY, resp.refresh_token)
        window.location.href = '/dashboard'
      })
      .catch((e) => {
        console.error('MagicLink: token exchange failed', e)
        setError('Link expired or already used. Restart the server for a new one.')
        setExchanging(false)
      })
  }, [magicToken])

  if (isLoading || exchanging) {
    return <div className="min-h-screen flex items-center justify-center text-text-primary">Signing in...</div>
  }
  if (isAuthenticated) return <Navigate to="/dashboard" replace />

  return (
    <div className="min-h-screen flex items-center justify-center bg-surface-0">
      <div className="max-w-md w-full space-y-4 p-8 bg-surface-1 border border-border-default rounded-md text-center">
        <h1 className="text-3xl font-bold text-text-primary">Clawvisor</h1>
        {error ? (
          <p className="text-danger">{error}</p>
        ) : (
          <>
            <p className="text-text-secondary">
              Use the magic link from your terminal to sign in.
            </p>
            <p className="text-sm text-text-tertiary">
              The server prints a one-time URL on startup. Paste it in your browser to get started.
            </p>
          </>
        )}
      </div>
    </div>
  )
}
