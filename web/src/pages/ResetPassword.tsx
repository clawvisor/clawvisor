import { useState, useEffect, FormEvent } from 'react'
import { useNavigate, useSearchParams, Link } from 'react-router-dom'
import { api, APIError, type ResetMethods } from '../api/client'
import { useAuth } from '../hooks/useAuth'

type Step = 'loading' | 'verify' | 'new-password' | 'error'

export default function ResetPassword() {
  const { setSession } = useAuth()
  const navigate = useNavigate()
  const [searchParams] = useSearchParams()
  const resetToken = searchParams.get('token') ?? ''

  const [step, setStep] = useState<Step>('loading')
  const [methods, setMethods] = useState<ResetMethods | null>(null)
  const [verifiedToken, setVerifiedToken] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [isSubmitting, setIsSubmitting] = useState(false)

  // MFA inputs
  const [totpCode, setTotpCode] = useState('')
  const [backupCode, setBackupCode] = useState('')
  const [showBackupCode, setShowBackupCode] = useState(false)

  // New password
  const [password, setPassword] = useState('')
  const [confirmPassword, setConfirmPassword] = useState('')

  useEffect(() => {
    if (!resetToken) {
      setError('Missing or invalid reset link. Please request a new one.')
      setStep('error')
      return
    }
    api.auth.resetPassword.methods(resetToken)
      .then((m) => {
        setMethods(m)
        setStep('verify')
      })
      .catch((err) => {
        setError(err instanceof APIError ? err.message : 'This reset link is invalid or has expired.')
        setStep('error')
      })
  }, [resetToken])

  async function handleVerifyTOTP(e: FormEvent) {
    e.preventDefault()
    setError(null)
    setIsSubmitting(true)
    try {
      const resp = await api.auth.resetPassword.verifyTotp(resetToken, totpCode)
      setVerifiedToken(resp.reset_token)
      setStep('new-password')
    } catch (err: any) {
      setError(err instanceof APIError ? err.message : 'Invalid code')
    } finally {
      setIsSubmitting(false)
    }
  }

  async function handleVerifyBackupCode(e: FormEvent) {
    e.preventDefault()
    setError(null)
    setIsSubmitting(true)
    try {
      const resp = await api.auth.resetPassword.verifyBackupCode(resetToken, backupCode)
      setVerifiedToken(resp.reset_token)
      setStep('new-password')
    } catch (err: any) {
      setError(err instanceof APIError ? err.message : 'Invalid backup code')
    } finally {
      setIsSubmitting(false)
    }
  }

  async function handleResetPassword(e: FormEvent) {
    e.preventDefault()
    setError(null)
    if (password !== confirmPassword) {
      setError('Passwords do not match')
      return
    }
    if (password.length < 8) {
      setError('Password must be at least 8 characters')
      return
    }
    setIsSubmitting(true)
    try {
      const resp = await api.auth.resetPassword.reset(verifiedToken, password)
      setSession(resp.access_token, resp.refresh_token, resp.user)
      navigate('/dashboard', { replace: true })
    } catch (err: any) {
      setError(err instanceof APIError ? err.message : 'Could not reset password')
    } finally {
      setIsSubmitting(false)
    }
  }

  // Loading state
  if (step === 'loading') {
    return (
      <div className="min-h-screen flex items-center justify-center bg-surface-0">
        <div className="max-w-md w-full p-8 bg-surface-1 border border-border-default rounded-md text-center">
          <p className="text-text-secondary">Validating reset link...</p>
        </div>
      </div>
    )
  }

  // Error / expired state
  if (step === 'error') {
    return (
      <div className="min-h-screen flex items-center justify-center bg-surface-0">
        <div className="max-w-md w-full space-y-6 p-8 bg-surface-1 border border-border-default rounded-md">
          <div>
            <h1 className="text-3xl font-bold text-text-primary">Clawvisor</h1>
            <h2 className="mt-2 text-lg text-text-secondary">Reset link expired</h2>
          </div>
          <div className="p-3 bg-danger/10 text-danger rounded text-sm">
            {error}
          </div>
          <Link
            to="/forgot-password"
            className="block w-full py-2 px-4 bg-brand text-surface-0 rounded font-medium hover:bg-brand-strong text-center"
          >
            Request a new link
          </Link>
          <Link
            to="/login"
            className="block w-full py-2 text-sm text-text-tertiary hover:text-text-primary text-center"
          >
            Back to login
          </Link>
        </div>
      </div>
    )
  }

  // New password step
  if (step === 'new-password') {
    return (
      <div className="min-h-screen flex items-center justify-center bg-surface-0">
        <div className="max-w-md w-full space-y-6 p-8 bg-surface-1 border border-border-default rounded-md">
          <div>
            <h1 className="text-3xl font-bold text-text-primary">Clawvisor</h1>
            <h2 className="mt-2 text-lg text-text-secondary">Set a new password</h2>
            <p className="mt-1 text-sm text-text-tertiary">
              Your identity has been verified. Choose a new password.
            </p>
          </div>

          {error && (
            <div className="p-3 bg-danger/10 text-danger rounded text-sm">{error}</div>
          )}

          <form className="space-y-4" onSubmit={handleResetPassword}>
            <div>
              <label htmlFor="new-password" className="block text-sm font-medium text-text-secondary">
                New password
              </label>
              <input
                id="new-password"
                type="password"
                required
                minLength={8}
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                className="mt-1 block w-full rounded border border-border-default bg-surface-0 text-text-primary px-3 py-2 focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand"
              />
            </div>

            <div>
              <label htmlFor="confirm-password" className="block text-sm font-medium text-text-secondary">
                Confirm password
              </label>
              <input
                id="confirm-password"
                type="password"
                required
                minLength={8}
                value={confirmPassword}
                onChange={(e) => setConfirmPassword(e.target.value)}
                className="mt-1 block w-full rounded border border-border-default bg-surface-0 text-text-primary px-3 py-2 focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand"
              />
            </div>

            <button
              type="submit"
              disabled={isSubmitting || password.length < 8}
              className="w-full py-2 px-4 bg-brand text-surface-0 rounded font-medium hover:bg-brand-strong disabled:opacity-50"
            >
              {isSubmitting ? 'Resetting...' : 'Reset password'}
            </button>
          </form>
        </div>
      </div>
    )
  }

  // MFA verification step — backup code sub-view
  if (showBackupCode) {
    return (
      <div className="min-h-screen flex items-center justify-center bg-surface-0">
        <div className="max-w-md w-full space-y-6 p-8 bg-surface-1 border border-border-default rounded-md">
          <div>
            <h1 className="text-3xl font-bold text-text-primary">Clawvisor</h1>
            <h2 className="mt-2 text-lg text-text-secondary">Use a backup code</h2>
            <p className="mt-1 text-sm text-text-tertiary">
              Enter one of your backup codes to verify your identity. Each code can only be used once.
            </p>
          </div>

          {error && (
            <div className="p-3 bg-danger/10 text-danger rounded text-sm">{error}</div>
          )}

          <form className="space-y-4" onSubmit={handleVerifyBackupCode}>
            <div>
              <label htmlFor="backup-code" className="block text-sm font-medium text-text-secondary">
                Backup code
              </label>
              <input
                id="backup-code"
                type="text"
                required
                value={backupCode}
                onChange={(e) => setBackupCode(e.target.value)}
                placeholder="xxxx-xxxx"
                className="mt-1 block w-full rounded border border-border-default bg-surface-0 text-text-primary px-3 py-3 text-center text-2xl tracking-widest focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand placeholder:text-text-tertiary"
                autoComplete="off"
              />
            </div>
            <button
              type="submit"
              disabled={isSubmitting || backupCode.length < 8}
              className="w-full py-2 px-4 bg-brand text-surface-0 rounded font-medium hover:bg-brand-strong disabled:opacity-50"
            >
              {isSubmitting ? 'Verifying...' : 'Verify'}
            </button>
          </form>

          <button
            onClick={() => { setShowBackupCode(false); setError(null); setBackupCode('') }}
            className="w-full py-2 text-sm text-text-tertiary hover:text-text-primary"
          >
            Back to other methods
          </button>
        </div>
      </div>
    )
  }

  // MFA verification step — main view
  const hasTOTP = methods?.has_totp ?? false
  const hasBackupCodes = methods?.has_backup_codes ?? false

  return (
    <div className="min-h-screen flex items-center justify-center bg-surface-0">
      <div className="max-w-md w-full space-y-6 p-8 bg-surface-1 border border-border-default rounded-md">
        <div>
          <h1 className="text-3xl font-bold text-text-primary">Clawvisor</h1>
          <h2 className="mt-2 text-lg text-text-secondary">Verify your identity</h2>
          <p className="mt-1 text-sm text-text-tertiary">
            To reset your password, verify your identity with one of the methods below.
          </p>
        </div>

        {error && (
          <div className="p-3 bg-danger/10 text-danger rounded text-sm">{error}</div>
        )}

        {hasTOTP && (
          <form className="space-y-4" onSubmit={handleVerifyTOTP}>
            <div>
              <label htmlFor="totp-code" className="block text-sm font-medium text-text-secondary">
                Authenticator code
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
                placeholder="000000"
                className="mt-1 block w-full rounded border border-border-default bg-surface-0 text-text-primary px-3 py-3 text-center text-2xl tracking-widest focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand placeholder:text-text-tertiary"
                autoComplete="one-time-code"
              />
            </div>
            <button
              type="submit"
              disabled={isSubmitting || totpCode.length !== 6}
              className="w-full py-2 px-4 bg-brand text-surface-0 rounded font-medium hover:bg-brand-strong disabled:opacity-50"
            >
              {isSubmitting ? 'Verifying...' : 'Verify'}
            </button>
          </form>
        )}

        {hasBackupCodes && (
          <>
            <div className="relative">
              <div className="absolute inset-0 flex items-center">
                <div className="w-full border-t border-border-subtle" />
              </div>
              <div className="relative flex justify-center text-sm">
                <span className="px-2 bg-surface-1 text-text-tertiary">
                  {hasTOTP ? "don't have your authenticator?" : 'verify with'}
                </span>
              </div>
            </div>
            <button
              onClick={() => { setShowBackupCode(true); setError(null) }}
              className="w-full py-2 px-4 bg-surface-2 text-text-primary rounded font-medium hover:bg-surface-3"
            >
              Use a backup code
            </button>
          </>
        )}

        {!hasTOTP && !hasBackupCodes && (
          <div className="p-3 bg-danger/10 text-danger rounded text-sm">
            No verification methods available for this account. Please contact support.
          </div>
        )}

        <Link
          to="/login"
          className="block w-full py-2 text-sm text-text-tertiary hover:text-text-primary text-center"
        >
          Back to login
        </Link>
      </div>
    </div>
  )
}
