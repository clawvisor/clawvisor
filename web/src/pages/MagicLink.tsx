import { useEffect, useRef, useState } from 'react'
import { Navigate, useSearchParams } from 'react-router-dom'
import { useAuth } from '../hooks/useAuth'
import { api, setAccessToken } from '../api/client'

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
        window.location.href = '/dashboard'
      })
      .catch((e) => {
        console.error('MagicLink: token exchange failed', e)
        setError('Link expired or already used. Restart the server for a new one.')
        setExchanging(false)
      })
  }, [magicToken])

  if (isLoading || exchanging) {
    return (
      <div className="min-h-screen dev-workspace flex items-center justify-center p-6">
        <div className="dev-terminal">
          <div className="dev-terminal-header">
            <span className="dev-terminal-dot bg-danger/80" />
            <span className="dev-terminal-dot bg-warning/80" />
            <span className="dev-terminal-dot bg-success/80" />
            <span className="ml-2">clawvisor — auth</span>
          </div>
          <div className="dev-terminal-body text-text-secondary">
            <p><span className="dev-terminal-prompt">$</span> exchanging magic link token…</p>
            <span className="dev-terminal-cursor" />
          </div>
        </div>
      </div>
    )
  }
  if (isAuthenticated) return <Navigate to="/dashboard" replace />

  return (
    <div className="min-h-screen dev-workspace flex items-center justify-center p-6">
      <div className="dev-terminal">
        <div className="dev-terminal-header">
          <span className="dev-terminal-dot bg-danger/80" />
          <span className="dev-terminal-dot bg-warning/80" />
          <span className="dev-terminal-dot bg-success/80" />
          <span className="ml-2">clawvisor — auth</span>
        </div>
        <div className="dev-terminal-body">
          {error ? (
            <p className="text-danger">{error}</p>
          ) : (
            <>
              <p className="text-text-primary">
                <span className="dev-terminal-prompt">$</span> clawvisor auth --magic-link
              </p>
              <p className="text-text-secondary text-sm leading-relaxed">
                Paste the one-time URL printed in your terminal on server startup.
              </p>
              <p className="text-text-tertiary text-xs">
                waiting for token…
                <span className="dev-terminal-cursor" />
              </p>
            </>
          )}
        </div>
      </div>
    </div>
  )
}
