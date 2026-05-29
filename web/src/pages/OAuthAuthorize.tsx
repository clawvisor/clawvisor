import { useState, useEffect } from 'react'
import { useSearchParams } from 'react-router-dom'
import { api } from '../api/client'

export default function OAuthAuthorize() {
  const [searchParams] = useSearchParams()
  const [error, setError] = useState<string | null>(null)
  const [redirecting, setRedirecting] = useState(false)

  const clientId = searchParams.get('client_id') ?? ''
  const redirectUri = searchParams.get('redirect_uri') ?? ''
  const state = searchParams.get('state') ?? ''
  const codeChallenge = searchParams.get('code_challenge') ?? ''
  const scope = searchParams.get('scope') ?? ''

  const [showClose, setShowClose] = useState(false)
  useEffect(() => {
    if (!redirecting) return
    const timer = setTimeout(() => setShowClose(true), 5000)
    return () => clearTimeout(timer)
  }, [redirecting])

  if (!clientId || !redirectUri || !codeChallenge) {
    return (
      <div className="min-h-screen flex items-center justify-center bg-surface-0">
        <div className="bg-surface-1 border border-border-default rounded-md p-8 max-w-md w-full">
          <h1 className="text-xl font-semibold text-danger mb-2">Authorization Error</h1>
          <p className="text-text-secondary">Missing required OAuth parameters.</p>
        </div>
      </div>
    )
  }

  async function handleApprove() {
    try {
      const result = await api.oauthApprove({
        client_id: clientId,
        redirect_uri: redirectUri,
        state,
        code_challenge: codeChallenge,
        scope,
      })
      setRedirecting(true)
      window.location.href = result.redirect_uri
    } catch (err: any) {
      setError(err.message ?? 'Authorization failed')
    }
  }

  async function handleDeny() {
    try {
      const result = await api.oauthDeny({
        client_id: clientId,
        redirect_uri: redirectUri,
        state,
      })
      setRedirecting(true)
      window.location.href = result.redirect_uri
    } catch (err: any) {
      setError(err.message ?? 'Failed to deny authorization')
    }
  }

  if (redirecting) {
    return (
      <div className="min-h-screen flex items-center justify-center bg-surface-0">
        <div className="bg-surface-1 border border-border-default rounded-md p-8 max-w-md w-full text-center">
          {showClose ? (
            <>
              <h1 className="text-xl font-semibold text-text-primary mb-2">Authorization Complete</h1>
              <p className="text-text-secondary text-sm">You can close this page now.</p>
            </>
          ) : (
            <>
              <h1 className="text-xl font-semibold text-text-primary mb-2">Redirecting...</h1>
              <p className="text-text-secondary text-sm">Completing authorization.</p>
            </>
          )}
        </div>
      </div>
    )
  }

  if (error) {
    return (
      <div className="min-h-screen flex items-center justify-center bg-surface-0">
        <div className="bg-surface-1 border border-border-default rounded-md p-8 max-w-md w-full">
          <h1 className="text-xl font-semibold text-danger mb-2">Authorization Error</h1>
          <p className="text-text-secondary">{error}</p>
        </div>
      </div>
    )
  }

  return (
    <div className="min-h-screen flex items-center justify-center bg-surface-0">
      <div className="bg-surface-1 border border-border-default rounded-md p-8 max-w-md w-full">
        <div className="text-center mb-6">
          <h1 className="text-xl font-semibold text-text-primary mb-2">Authorize Connection</h1>
          <p className="text-text-secondary text-sm">
            An application wants to connect as an agent to your Clawvisor instance.
          </p>
        </div>

        <div className="bg-surface-2 border border-border-strong rounded-md p-4 mb-6">
          <p className="text-sm text-text-secondary mb-2">This will allow the application to:</p>
          <ul className="text-sm text-text-secondary space-y-1">
            <li>View your account catalog</li>
            <li>Create and manage tasks</li>
            <li>Make gateway requests (subject to your approval settings)</li>
          </ul>
        </div>

        <div className="flex gap-3">
          <button
            onClick={handleDeny}
            className="flex-1 px-4 py-2 bg-surface-2 hover:bg-surface-3 text-text-secondary rounded border border-border-strong text-sm font-medium transition-colors"
          >
            Deny
          </button>
          <button
            onClick={handleApprove}
            className="flex-1 px-4 py-2 bg-brand hover:bg-brand-strong text-surface-0 rounded text-sm font-medium transition-colors"
          >
            Authorize
          </button>
        </div>
      </div>
    </div>
  )
}
