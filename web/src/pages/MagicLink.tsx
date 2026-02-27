import { useEffect, useRef, useState } from 'react'
import { Navigate } from 'react-router-dom'
import { useAuth } from '../hooks/useAuth'
import { api, setAccessToken } from '../api/client'

const REFRESH_TOKEN_KEY = 'clawvisor_refresh_token'

export default function MagicLink() {
  const { isAuthenticated, isLoading } = useAuth()
  const [error, setError] = useState<string | null>(null)
  const [exchanging, setExchanging] = useState(false)
  const didExchange = useRef(false)

  useEffect(() => {
    if (didExchange.current) return
    const hash = window.location.hash
    if (!hash.startsWith('#token=')) return
    didExchange.current = true

    const magicToken = hash.slice('#token='.length)
    window.history.replaceState(null, '', window.location.pathname)
    if (!magicToken) return

    setExchanging(true)
    api.auth.magic(magicToken)
      .then((resp) => {
        setAccessToken(resp.access_token)
        localStorage.setItem(REFRESH_TOKEN_KEY, resp.refresh_token)
        // Force a page reload so AuthProvider picks up the new session cleanly.
        window.location.href = '/dashboard'
      })
      .catch(() => {
        setError('Link expired or already used. Restart the server for a new one.')
        setExchanging(false)
      })
  }, [])

  if (isLoading || exchanging) {
    return <div className="min-h-screen flex items-center justify-center">Signing in...</div>
  }
  if (isAuthenticated) return <Navigate to="/dashboard" replace />

  return (
    <div className="min-h-screen flex items-center justify-center bg-gray-50">
      <div className="max-w-md w-full space-y-4 p-8 bg-white rounded-lg shadow text-center">
        <h1 className="text-3xl font-bold text-gray-900">Clawvisor</h1>
        {error ? (
          <p className="text-red-600">{error}</p>
        ) : (
          <>
            <p className="text-gray-600">
              Use the magic link from your terminal to sign in.
            </p>
            <p className="text-sm text-gray-500">
              The server prints a one-time URL on startup. Paste it in your browser to get started.
            </p>
          </>
        )}
      </div>
    </div>
  )
}
