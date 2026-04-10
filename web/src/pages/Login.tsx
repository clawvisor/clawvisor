import { useState, FormEvent } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { useAuth } from '../hooks/useAuth'
import { api, APIError } from '../api/client'
import { isWebAuthnAvailable, startAuthentication } from '../lib/webauthn'

export default function Login() {
  const { login, setSession, features } = useAuth()
  const navigate = useNavigate()
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [isSubmitting, setIsSubmitting] = useState(false)

  const passkeysEnabled = features?.passkeys && isWebAuthnAvailable()

  async function handleGoogleLogin() {
    setError(null)
    setIsSubmitting(true)
    try {
      const { client_id } = await api.auth.google.clientId()
      const redirectUri = `${window.location.origin}/login/oauth/callback`
      const params = new URLSearchParams({
        client_id,
        redirect_uri: redirectUri,
        response_type: 'code',
        scope: 'openid email profile',
        prompt: 'select_account',
      })
      window.location.href = `https://accounts.google.com/o/oauth2/v2/auth?${params}`
    } catch (err: any) {
      setError(err instanceof APIError ? err.message : 'Google sign-in is not available')
      setIsSubmitting(false)
    }
  }

  async function handlePasskeyLogin() {
    setError(null)
    setIsSubmitting(true)
    try {
      const beginResp = await api.auth.passkey.loginBegin()
      const credential = await startAuthentication(beginResp.options)
      const finishResp = await api.auth.passkey.loginFinish(beginResp.challenge_id, credential)
      setSession(finishResp.access_token, finishResp.refresh_token, finishResp.user)
      navigate('/dashboard')
    } catch (err: any) {
      if (err instanceof APIError) {
        setError(err.message)
      } else if (err?.name === 'NotAllowedError') {
        setError('Passkey authentication was cancelled')
      } else {
        setError(err?.message ?? 'Passkey authentication failed')
      }
    } finally {
      setIsSubmitting(false)
    }
  }

  async function handleSubmit(e: FormEvent) {
    e.preventDefault()
    setError(null)
    setIsSubmitting(true)
    try {
      const resp = await login(email, password)
      if (resp.status === 'requires_mfa') {
        navigate('/mfa-verify', {
          state: {
            pending_token: resp.pending_token,
            mfa_methods: resp.mfa_methods,
          },
          replace: true,
        })
      } else {
        navigate('/dashboard')
      }
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

  return (
    <div className="min-h-screen flex items-center justify-center bg-surface-0">
      <div className="max-w-md w-full space-y-8 p-8 bg-surface-1 border border-border-default rounded-md">
        <div>
          <h1 className="text-3xl font-bold text-text-primary">Clawvisor</h1>
          <h2 className="mt-2 text-lg text-text-secondary">Sign in to your account</h2>
        </div>

        {error && (
          <div className="p-3 bg-danger/10 text-danger rounded text-sm">{error}</div>
        )}

        <div className="space-y-3">
          {passkeysEnabled && (
            <button
              onClick={handlePasskeyLogin}
              disabled={isSubmitting}
              className="w-full py-3 px-4 bg-brand text-surface-0 rounded font-medium hover:bg-brand-strong disabled:opacity-50"
            >
              {isSubmitting ? 'Authenticating...' : 'Sign in with passkey'}
            </button>
          )}
          <button
            onClick={handleGoogleLogin}
            disabled={isSubmitting}
            className="w-full py-3 px-4 bg-surface-2 text-text-primary rounded font-medium hover:bg-surface-3 disabled:opacity-50 flex items-center justify-center gap-2"
          >
            <svg className="w-5 h-5" viewBox="0 0 24 24"><path d="M22.56 12.25c0-.78-.07-1.53-.2-2.25H12v4.26h5.92a5.06 5.06 0 0 1-2.2 3.32v2.77h3.57c2.08-1.92 3.28-4.74 3.28-8.1z" fill="#4285F4"/><path d="M12 23c2.97 0 5.46-.98 7.28-2.66l-3.57-2.77c-.98.66-2.23 1.06-3.71 1.06-2.86 0-5.29-1.93-6.16-4.53H2.18v2.84C3.99 20.53 7.7 23 12 23z" fill="#34A853"/><path d="M5.84 14.09c-.22-.66-.35-1.36-.35-2.09s.13-1.43.35-2.09V7.07H2.18C1.43 8.55 1 10.22 1 12s.43 3.45 1.18 4.93l2.85-2.22.81-.62z" fill="#FBBC05"/><path d="M12 5.38c1.62 0 3.06.56 4.21 1.64l3.15-3.15C17.45 2.09 14.97 1 12 1 7.7 1 3.99 3.47 2.18 7.07l3.66 2.84c.87-2.6 3.3-4.53 6.16-4.53z" fill="#EA4335"/></svg>
            Sign in with Google
          </button>
          <div className="relative">
            <div className="absolute inset-0 flex items-center">
              <div className="w-full border-t border-border-subtle" />
            </div>
            <div className="relative flex justify-center text-sm">
              <span className="px-2 bg-surface-1 text-text-tertiary">or sign in with password</span>
            </div>
          </div>
        </div>

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

          <div>
            <label htmlFor="password" className="block text-sm font-medium text-text-secondary">
              Password
            </label>
            <input
              id="password"
              type="password"
              required
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              className="mt-1 block w-full rounded border border-border-default bg-surface-0 text-text-primary px-3 py-2 focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand"
            />
          </div>

          <button
            type="submit"
            disabled={isSubmitting}
            className="w-full py-2 px-4 bg-brand text-surface-0 rounded font-medium hover:bg-brand-strong disabled:opacity-50"
          >
            {isSubmitting ? 'Signing in...' : 'Sign in'}
          </button>
        </form>

        <p className="text-center text-sm text-text-secondary">
          Don't have an account?{' '}
          <Link to="/register" className="text-brand hover:underline">
            Register
          </Link>
        </p>
      </div>
    </div>
  )
}
