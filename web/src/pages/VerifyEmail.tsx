import { useEffect, useRef, useState } from 'react'
import { Link, useNavigate, useSearchParams } from 'react-router-dom'
import { api, APIError } from '../api/client'
import { useAuth } from '../hooks/useAuth'

export default function VerifyEmail() {
  const [searchParams] = useSearchParams()
  const navigate = useNavigate()
  const { setSession } = useAuth()
  const token = searchParams.get('token') ?? ''
  const [error, setError] = useState<string | null>(null)
  const [verifying, setVerifying] = useState(true)
  const [destination, setDestination] = useState<string | null>(null)
  const { isAuthenticated } = useAuth()
  // Prevents React StrictMode's double-invoke from firing two concurrent
  // verify requests for the same single-use token.
  const didVerify = useRef(false)

  useEffect(() => {
    if (!token) {
      setError('Missing verification token')
      setVerifying(false)
      return
    }
    if (didVerify.current) return
    didVerify.current = true

    let cancelled = false
    async function verify() {
      try {
        const result = await api.auth.verifyEmail(token)
        if (!cancelled) {
          setSession(result.access_token, result.refresh_token, result.user)
          setDestination('/onboarding')
        }
      } catch (err) {
        if (!cancelled) {
          if (err instanceof APIError) {
            setError(err.message)
          } else {
            setError('Verification failed. The link may have expired.')
          }
          setVerifying(false)
        }
      }
    }
    verify()
    return () => { cancelled = true }
  }, [token, setSession])

  useEffect(() => {
    if (destination && isAuthenticated) navigate(destination, { replace: true })
  }, [destination, isAuthenticated, navigate])

  return (
    <div className="min-h-screen flex items-center justify-center bg-surface-0">
      <div className="max-w-md w-full space-y-6 p-8 bg-surface-1 border border-border-default rounded-md text-center">
        {verifying ? (
          <>
            <h1 className="text-2xl font-bold text-text-primary">Verifying your email...</h1>
            <p className="text-text-tertiary">Please wait a moment.</p>
          </>
        ) : error ? (
          <>
            <h1 className="text-2xl font-bold text-text-primary">Verification failed</h1>
            <div className="p-3 bg-danger/10 text-danger rounded text-sm">{error}</div>
            <p className="text-sm text-text-tertiary">
              The link may have expired or already been used.
            </p>
            <Link
              to="/register"
              className="inline-block mt-2 text-brand hover:underline text-sm"
            >
              Register again
            </Link>
          </>
        ) : null}
      </div>
    </div>
  )
}
