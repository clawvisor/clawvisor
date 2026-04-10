import { useState, FormEvent } from 'react'
import { useNavigate, useLocation } from 'react-router-dom'
import { api, APIError } from '../api/client'
import { useAuth } from '../hooks/useAuth'
import { startAuthentication } from '../lib/webauthn'

interface MFAState {
  pending_token: string
  mfa_methods: {
    has_totp: boolean
    passkey_count: number
    has_backup_codes: boolean
  }
}

export default function MFAVerify() {
  const { setSession } = useAuth()
  const navigate = useNavigate()
  const location = useLocation()
  const state = location.state as MFAState | null

  const [error, setError] = useState<string | null>(null)
  const [isSubmitting, setIsSubmitting] = useState(false)
  const [totpCode, setTotpCode] = useState('')
  const [backupCode, setBackupCode] = useState('')
  const [showBackupCode, setShowBackupCode] = useState(false)

  const pendingToken = state?.pending_token ?? ''
  const hasTOTP = state?.mfa_methods?.has_totp ?? false
  const hasPasskeys = (state?.mfa_methods?.passkey_count ?? 0) > 0
  const hasBackupCodes = state?.mfa_methods?.has_backup_codes ?? false

  async function handlePasskey() {
    setError(null)
    setIsSubmitting(true)
    try {
      const beginResp = await api.auth.passkey.verifyBegin(pendingToken)
      const credential = await startAuthentication(beginResp.options)
      const finishResp = await api.auth.passkey.verifyFinish(pendingToken, beginResp.challenge_id, credential)
      setSession(finishResp.access_token, finishResp.refresh_token, finishResp.user)
      navigate('/dashboard', { replace: true })
    } catch (err: any) {
      if (err?.name === 'NotAllowedError') {
        setError('Passkey verification was cancelled')
      } else if (err instanceof APIError) {
        setError(err.message)
      } else {
        setError(err?.message ?? 'Passkey verification failed')
      }
    } finally {
      setIsSubmitting(false)
    }
  }

  async function handleTOTP(e: FormEvent) {
    e.preventDefault()
    setError(null)
    setIsSubmitting(true)
    try {
      const resp = await api.auth.totp.verify(pendingToken, totpCode)
      setSession(resp.access_token, resp.refresh_token, resp.user)
      navigate('/dashboard', { replace: true })
    } catch (err: any) {
      setError(err instanceof APIError ? err.message : 'Invalid code')
    } finally {
      setIsSubmitting(false)
    }
  }

  async function handleBackupCode(e: FormEvent) {
    e.preventDefault()
    setError(null)
    setIsSubmitting(true)
    try {
      const resp = await api.auth.backupCode.verify(pendingToken, backupCode)
      setSession(resp.access_token, resp.refresh_token, resp.user)
      navigate('/dashboard', { replace: true })
    } catch (err: any) {
      setError(err instanceof APIError ? err.message : 'Invalid backup code')
    } finally {
      setIsSubmitting(false)
    }
  }

  if (!pendingToken) {
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

  if (showBackupCode) {
    return (
      <div className="min-h-screen flex items-center justify-center bg-surface-0">
        <div className="max-w-md w-full space-y-6 p-8 bg-surface-1 border border-border-default rounded-md">
          <div>
            <h1 className="text-3xl font-bold text-text-primary">Clawvisor</h1>
            <h2 className="mt-2 text-lg text-text-secondary">Use a backup code</h2>
            <p className="mt-1 text-sm text-text-tertiary">
              Enter one of your backup codes to sign in. Each code can only be used once.
            </p>
          </div>

          {error && (
            <div className="p-3 bg-danger/10 text-danger rounded text-sm">{error}</div>
          )}

          <form className="space-y-4" onSubmit={handleBackupCode}>
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

  return (
    <div className="min-h-screen flex items-center justify-center bg-surface-0">
      <div className="max-w-md w-full space-y-6 p-8 bg-surface-1 border border-border-default rounded-md">
        <div>
          <h1 className="text-3xl font-bold text-text-primary">Clawvisor</h1>
          <h2 className="mt-2 text-lg text-text-secondary">Verify your identity</h2>
          <p className="mt-1 text-sm text-text-tertiary">
            Choose a verification method to complete sign-in.
          </p>
        </div>

        {error && (
          <div className="p-3 bg-danger/10 text-danger rounded text-sm">{error}</div>
        )}

        {hasPasskeys && (
          <button
            onClick={handlePasskey}
            disabled={isSubmitting}
            className="w-full py-3 px-4 bg-brand text-surface-0 rounded font-medium hover:bg-brand-strong disabled:opacity-50"
          >
            {isSubmitting ? 'Verifying...' : 'Verify with passkey'}
          </button>
        )}

        {hasTOTP && hasPasskeys && (
          <div className="relative">
            <div className="absolute inset-0 flex items-center">
              <div className="w-full border-t border-border-subtle" />
            </div>
            <div className="relative flex justify-center text-sm">
              <span className="px-2 bg-surface-1 text-text-tertiary">or use authenticator</span>
            </div>
          </div>
        )}

        {hasTOTP && (
          <form className="space-y-4" onSubmit={handleTOTP}>
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
                <span className="px-2 bg-surface-1 text-text-tertiary">lost your device?</span>
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

        <button
          onClick={() => navigate('/login')}
          className="w-full py-2 text-sm text-text-tertiary hover:text-text-primary"
        >
          Back to login
        </button>
      </div>
    </div>
  )
}
