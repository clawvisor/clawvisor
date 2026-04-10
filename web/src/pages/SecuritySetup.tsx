import { useState, useEffect, FormEvent } from 'react'
import { useNavigate } from 'react-router-dom'
import { api, APIError, type OnboardingStatus } from '../api/client'
import { useAuth } from '../hooks/useAuth'
import { isWebAuthnAvailable, startRegistration } from '../lib/webauthn'

type Step = 'loading' | 'security' | 'passkey' | 'totp' | 'totp-confirm' | 'backup-codes' | 'done'

export default function SecuritySetup() {
  const { user, refreshOnboarding } = useAuth()
  const navigate = useNavigate()
  const [step, setStep] = useState<Step>('loading')
  const [error, setError] = useState<string | null>(null)
  const [isSubmitting, setIsSubmitting] = useState(false)
  const [status, setStatus] = useState<OnboardingStatus | null>(null)

  // TOTP state
  const [totpQR, setTotpQR] = useState<string | null>(null)
  const [totpSecret, setTotpSecret] = useState<string | null>(null)
  const [totpCode, setTotpCode] = useState('')

  // Backup codes state
  const [backupCodes, setBackupCodes] = useState<string[] | null>(null)
  const [codesDownloaded, setCodesDownloaded] = useState(false)

  useEffect(() => {
    loadStatus()
  }, [])

  async function loadStatus() {
    try {
      const s = await api.auth.onboarding.status()
      setStatus(s)
      if (s.onboarding_completed) {
        navigate('/dashboard', { replace: true })
      } else if (s.has_security_method && s.has_backup_codes) {
        setStep('done')
      } else if (s.has_security_method) {
        setStep('backup-codes')
      } else {
        setStep('security')
      }
    } catch {
      setStep('security')
    }
  }

  // ── Passkey registration ──────────────────────────────────────────────────

  async function handleAddPasskey() {
    setError(null)
    setIsSubmitting(true)
    try {
      const beginResp = await api.auth.passkey.addBegin()
      const credential = await startRegistration(beginResp.options)
      await api.auth.passkey.addFinish(beginResp.challenge_id, credential)
      await loadStatus()
    } catch (err: any) {
      if (err?.name === 'NotAllowedError') {
        setError('Passkey registration was cancelled')
      } else {
        setError(err instanceof APIError ? err.message : 'Passkey registration failed')
      }
    } finally {
      setIsSubmitting(false)
    }
  }

  // ── TOTP setup ────────────────────────────────────────────────────────────

  async function handleStartTOTP() {
    setError(null)
    setIsSubmitting(true)
    try {
      const totp = await api.auth.totp.setup()
      setTotpQR(totp.qr_data_url)
      setTotpSecret(totp.secret)
      setStep('totp')
    } catch (err: any) {
      setError(err instanceof APIError ? err.message : 'Failed to start TOTP setup')
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
      await loadStatus()
    } catch (err: any) {
      setError(err instanceof APIError ? err.message : 'Invalid TOTP code')
    } finally {
      setIsSubmitting(false)
    }
  }

  // ── Backup codes ──────────────────────────────────────────────────────────

  async function handleGenerateBackupCodes() {
    setError(null)
    setIsSubmitting(true)
    try {
      const resp = await api.auth.onboarding.generateBackupCodes()
      setBackupCodes(resp.codes)
    } catch (err: any) {
      setError(err instanceof APIError ? err.message : 'Failed to generate backup codes')
    } finally {
      setIsSubmitting(false)
    }
  }

  function handleDownloadCodes() {
    if (!backupCodes) return
    const text = [
      'Clawvisor Backup Codes',
      `Generated: ${new Date().toISOString()}`,
      `Account: ${user?.email ?? ''}`,
      '',
      'Keep these codes in a safe place. Each code can only be used once.',
      '',
      ...backupCodes.map((code, i) => `${i + 1}. ${code}`),
    ].join('\n')
    const blob = new Blob([text], { type: 'text/plain' })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = 'clawvisor-backup-codes.txt'
    a.click()
    URL.revokeObjectURL(url)
    setCodesDownloaded(true)
  }

  async function handleContinueAfterCodes() {
    await loadStatus()
    if (status?.has_security_method && status?.has_backup_codes) {
      setStep('done')
    }
  }

  // ── Complete onboarding ───────────────────────────────────────────────────

  async function handleComplete() {
    setError(null)
    setIsSubmitting(true)
    try {
      await api.auth.onboarding.complete()
      await refreshOnboarding()
      navigate('/dashboard', { replace: true })
    } catch (err: any) {
      setError(err instanceof APIError ? err.message : 'Failed to complete onboarding')
    } finally {
      setIsSubmitting(false)
    }
  }

  // ── Render ────────────────────────────────────────────────────────────────

  if (step === 'loading') {
    return (
      <div className="min-h-screen flex items-center justify-center bg-surface-0">
        <div className="text-text-secondary">Loading...</div>
      </div>
    )
  }

  return (
    <div className="min-h-screen flex items-center justify-center bg-surface-0">
      <div className="max-w-md w-full space-y-6 p-8 bg-surface-1 border border-border-default rounded-md">
        <div>
          <h1 className="text-3xl font-bold text-text-primary">Clawvisor</h1>
          <h2 className="mt-2 text-lg text-text-secondary">Secure your account</h2>
          <div className="mt-3 flex gap-2">
            {['Security', 'Backup codes', 'Done'].map((label, i) => {
              const stepIndex = step === 'security' || step === 'passkey' || step === 'totp' || step === 'totp-confirm' ? 0
                : step === 'backup-codes' ? 1
                : 2
              return (
                <div key={label} className="flex-1">
                  <div className={`h-1 rounded-full ${i <= stepIndex ? 'bg-brand' : 'bg-surface-3'}`} />
                  <p className={`text-xs mt-1 ${i <= stepIndex ? 'text-brand' : 'text-text-tertiary'}`}>{label}</p>
                </div>
              )
            })}
          </div>
        </div>

        {error && (
          <div className="p-3 bg-danger/10 text-danger rounded text-sm">{error}</div>
        )}

        {/* Step: Choose security method */}
        {step === 'security' && (
          <div className="space-y-3">
            <p className="text-sm text-text-secondary">
              Set up at least one security method to protect your account.
            </p>
            {isWebAuthnAvailable() && (
              <button
                onClick={handleAddPasskey}
                disabled={isSubmitting}
                className="w-full py-3 px-4 bg-brand text-surface-0 rounded font-medium hover:bg-brand-strong disabled:opacity-50 text-left"
              >
                <div className="font-medium">Add a passkey</div>
                <div className="text-sm text-surface-0/70 mt-0.5">Recommended &mdash; use Face ID, Touch ID, or a security key</div>
              </button>
            )}
            <button
              onClick={handleStartTOTP}
              disabled={isSubmitting}
              className="w-full py-3 px-4 bg-surface-2 text-text-primary rounded font-medium hover:bg-surface-3 disabled:opacity-50 text-left"
            >
              <div className="font-medium">Set up authenticator app</div>
              <div className="text-sm text-text-tertiary mt-0.5">Use Google Authenticator, 1Password, or similar</div>
            </button>
          </div>
        )}

        {/* Step: TOTP QR scan */}
        {step === 'totp' && (
          <form className="space-y-4" onSubmit={handleConfirmTOTP}>
            <p className="text-sm text-text-secondary">
              Scan this QR code with your authenticator app.
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
              {isSubmitting ? 'Verifying...' : 'Verify'}
            </button>
            <button
              type="button"
              onClick={() => { setStep('security'); setError(null) }}
              className="w-full py-2 text-sm text-text-tertiary hover:text-text-primary"
            >
              Back
            </button>
          </form>
        )}

        {/* Step: Backup codes */}
        {step === 'backup-codes' && (
          <div className="space-y-4">
            <p className="text-sm text-text-secondary">
              Download your backup codes. You'll need these to recover your account if you lose access to your security method.
            </p>
            {!backupCodes ? (
              <button
                onClick={handleGenerateBackupCodes}
                disabled={isSubmitting}
                className="w-full py-2 px-4 bg-brand text-surface-0 rounded font-medium hover:bg-brand-strong disabled:opacity-50"
              >
                {isSubmitting ? 'Generating...' : 'Generate backup codes'}
              </button>
            ) : (
              <>
                <div className="bg-surface-0 border border-border-default rounded p-4">
                  <div className="grid grid-cols-2 gap-2">
                    {backupCodes.map((code, i) => (
                      <div key={i} className="font-mono text-sm text-text-primary bg-surface-2 px-3 py-1.5 rounded text-center">
                        {code}
                      </div>
                    ))}
                  </div>
                </div>
                <button
                  onClick={handleDownloadCodes}
                  className="w-full py-2 px-4 bg-surface-2 text-text-primary rounded font-medium hover:bg-surface-3"
                >
                  Download as text file
                </button>
                <button
                  onClick={handleContinueAfterCodes}
                  disabled={!codesDownloaded}
                  className="w-full py-2 px-4 bg-brand text-surface-0 rounded font-medium hover:bg-brand-strong disabled:opacity-50"
                >
                  {codesDownloaded ? 'Continue' : 'Download codes first'}
                </button>
              </>
            )}
          </div>
        )}

        {/* Step: Complete */}
        {step === 'done' && (
          <div className="space-y-4 text-center">
            <div className="text-4xl">&#10003;</div>
            <p className="text-sm text-text-secondary">
              Your account is secured. You're ready to go!
            </p>
            <button
              onClick={handleComplete}
              disabled={isSubmitting}
              className="w-full py-2 px-4 bg-brand text-surface-0 rounded font-medium hover:bg-brand-strong disabled:opacity-50"
            >
              {isSubmitting ? 'Finishing...' : 'Go to dashboard'}
            </button>
          </div>
        )}
      </div>
    </div>
  )
}
