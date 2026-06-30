import { useState, FormEvent } from 'react'
import { useNavigate, useLocation } from 'react-router-dom'
import { api, setAccessToken, type AuthResponse } from '../api/client'
import { useAuth } from '../hooks/useAuth'
import { isWebAuthnAvailable, startRegistration } from '../lib/webauthn'
import { nextAfterAuth } from '../lib/nextAfterAuth'

type Step = 'choose' | 'passkey' | 'password' | 'totp'

export default function SetupAuth() {
  const { setSession } = useAuth()
  const navigate = useNavigate()
  const location = useLocation()
  const setupToken: string = (location.state as any)?.setup_token ?? ''
  const isUpgrade = (location.state as any)?.upgrade === true

  const [step, setStep] = useState<Step>('choose')
  const [error, setError] = useState<string | null>(null)
  const [isSubmitting, setIsSubmitting] = useState(false)

  // Password + TOTP flow state
  const [password, setPassword] = useState('')
  const [totpQR, setTotpQR] = useState<string | null>(null)
  const [totpSecret, setTotpSecret] = useState<string | null>(null)
  const [totpCode, setTotpCode] = useState('')

  function completeAuth(resp: AuthResponse) {
    setSession(resp.access_token, resp.refresh_token, resp.user)
    navigate(nextAfterAuth(), { replace: true })
  }

  async function handlePasskey() {
    setError(null)
    setIsSubmitting(true)
    try {
      const beginResp = await api.auth.passkey.registerBegin(setupToken)
      const credential = await startRegistration(beginResp.options)
      const finishResp = await api.auth.passkey.registerFinish(setupToken, beginResp.challenge_id, credential)
      completeAuth(finishResp)
    } catch (err: any) {
      setError(err.message ?? 'Passkey registration failed')
    } finally {
      setIsSubmitting(false)
    }
  }

  async function handleSetPassword(e: FormEvent) {
    e.preventDefault()
    setError(null)
    setIsSubmitting(true)
    try {
      // Setup password returns session tokens so we can call TOTP setup
      const pwResp = await api.auth.setupPassword(password, setupToken) as AuthResponse
      setAccessToken(pwResp.access_token)
      // Now set up TOTP (uses the session token we just stored)
      const totp = await api.auth.totp.setup()
      setTotpQR(totp.qr_data_url)
      setTotpSecret(totp.secret)
      setStep('totp')
    } catch (err: any) {
      setError(err.message ?? 'Failed to set password')
    } finally {
      setIsSubmitting(false)
    }
  }

  async function handleConfirmTOTP(e: FormEvent) {
    e.preventDefault()
    setError(null)
    setIsSubmitting(true)
    try {
      await api.auth.totp.confirm(totpCode)
      // TOTP confirmed — we already have session tokens from password setup
      navigate(nextAfterAuth(), { replace: true })
    } catch (err: any) {
      setError(err.message ?? 'Invalid TOTP code')
    } finally {
      setIsSubmitting(false)
    }
  }

  if (!setupToken) {
    return (
      <div className="min-h-screen flex items-center justify-center bg-surface-0">
        <div className="max-w-md w-full p-8 bg-surface-1 border border-border-default rounded-md text-center">
          <h2 className="text-lg font-semibold text-text-primary mb-2">Session expired</h2>
          <p className="text-sm text-text-secondary mb-4">Please sign in again.</p>
          <button
            onClick={() => navigate('/login')}
            className="py-2 px-4 bg-brand text-surface-0 rounded font-medium hover:bg-brand-strong"
          >
            Go to login
          </button>
        </div>
      </div>
    )
  }

  return (
    <div className="min-h-screen flex items-center justify-center bg-surface-0">
      <div className="max-w-md w-full space-y-6 p-8 bg-surface-1 border border-border-default rounded-md">
        <div>
          <h1 className="text-3xl font-bold text-text-primary">Clawvisor</h1>
          <h2 className="mt-2 text-lg text-text-secondary">
            {isUpgrade ? 'Secure your account' : 'Set up authentication'}
          </h2>
          {isUpgrade && (
            <p className="mt-1 text-sm text-warning">
              Password-only login is no longer supported. Please add a passkey or enable TOTP.
            </p>
          )}
        </div>

        {error && (
          <div className="p-3 bg-danger/10 text-danger rounded text-sm">{error}</div>
        )}

        {step === 'choose' && (
          <div className="space-y-3">
            {isWebAuthnAvailable() && (
              <button
                onClick={handlePasskey}
                disabled={isSubmitting}
                className="w-full py-3 px-4 bg-brand text-surface-0 rounded font-medium hover:bg-brand-strong disabled:opacity-50 text-left"
              >
                <div className="font-medium">Set up passkey</div>
                <div className="text-sm text-surface-0/70 mt-0.5">Recommended &mdash; use Face ID, Touch ID, or your security key</div>
              </button>
            )}
            <button
              onClick={() => setStep('password')}
              className="w-full py-3 px-4 bg-surface-2 text-text-primary rounded font-medium hover:bg-surface-3 text-left"
            >
              <div className="font-medium">Use password instead</div>
              <div className="text-sm text-text-tertiary mt-0.5">Requires TOTP authenticator app for 2FA</div>
            </button>
          </div>
        )}

        {step === 'password' && (
          <form className="space-y-4" onSubmit={handleSetPassword}>
            <div>
              <label htmlFor="password" className="block text-sm font-medium text-text-secondary">
                Create a password
              </label>
              <input
                id="password"
                type="password"
                required
                minLength={8}
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                className="mt-1 block w-full rounded border border-border-default bg-surface-0 text-text-primary px-3 py-2 focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand"
              />
              <p className="mt-1 text-xs text-text-tertiary">Minimum 8 characters</p>
            </div>
            <button
              type="submit"
              disabled={isSubmitting}
              className="w-full py-2 px-4 bg-brand text-surface-0 rounded font-medium hover:bg-brand-strong disabled:opacity-50"
            >
              {isSubmitting ? 'Setting up...' : 'Continue to TOTP setup'}
            </button>
            <button
              type="button"
              onClick={() => setStep('choose')}
              className="w-full py-2 text-sm text-text-tertiary hover:text-text-primary"
            >
              Back
            </button>
          </form>
        )}

        {step === 'totp' && (
          <form className="space-y-4" onSubmit={handleConfirmTOTP}>
            <p className="text-sm text-text-secondary">
              Scan this QR code with your authenticator app (Google Authenticator, 1Password, etc.)
            </p>
            {totpQR && (
              <div className="flex justify-center">
                <img src={totpQR} alt="TOTP QR Code" className="w-48 h-48" />
              </div>
            )}
            {totpSecret && (
              <div className="text-center">
                <p className="text-xs text-text-tertiary mb-1">Or enter this code manually:</p>
                <code className="text-sm bg-surface-2 px-3 py-1 rounded font-mono select-all">{totpSecret}</code>
              </div>
            )}
            <div>
              <label htmlFor="totp-code" className="block text-sm font-medium text-text-secondary">
                Enter 6-digit code
              </label>
              <input
                id="totp-code"
                type="text"
                inputMode="numeric"
                pattern="[0-9]{6}"
                maxLength={6}
                required
                value={totpCode}
                onChange={(e) => setTotpCode(e.target.value.replace(/\D/g, ''))}
                className="mt-1 block w-full rounded border border-border-default bg-surface-0 text-text-primary px-3 py-2 text-center text-2xl tracking-widest focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand"
                autoComplete="one-time-code"
              />
            </div>
            <button
              type="submit"
              disabled={isSubmitting || totpCode.length !== 6}
              className="w-full py-2 px-4 bg-brand text-surface-0 rounded font-medium hover:bg-brand-strong disabled:opacity-50"
            >
              {isSubmitting ? 'Verifying...' : 'Verify and finish'}
            </button>
          </form>
        )}
      </div>
    </div>
  )
}
