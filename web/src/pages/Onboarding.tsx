import { useState, useEffect, useRef, useCallback } from 'react'
import { useMutation, useQueryClient, useQuery } from '@tanstack/react-query'
import { api, type ServiceInfo, type ServiceActionInfo } from '../api/client'
import { serviceName, serviceDescription } from '../lib/services'

// ── Types ─────────────────────────────────────────────────────────────────────

interface ServiceType {
  baseId: string
  oauth: boolean
  actions: ServiceActionInfo[]
  activatedCount: number
}

// ── Stepper ───────────────────────────────────────────────────────────────────

const STEPS = ['Connect a Service', 'Create an Agent', 'Set Up Notifications']

function Stepper({
  current,
  completed,
  onStepClick,
}: {
  current: number
  completed: number[]
  onStepClick: (step: number) => void
}) {
  return (
    <div className="flex items-center justify-center gap-0 mb-10">
      {STEPS.map((label, i) => {
        const stepNum = i + 1
        const isDone = completed.includes(stepNum)
        const isActive = stepNum === current
        return (
          <div key={label} className="flex items-center">
            {i > 0 && (
              <div className={`w-12 h-px mx-1 ${isDone || isActive ? 'bg-brand' : 'bg-border-default'}`} />
            )}
            <button
              type="button"
              onClick={() => onStepClick(stepNum)}
              className="flex flex-col items-center gap-1.5 bg-transparent border-0 p-0 cursor-pointer"
            >
              <div
                className={`w-8 h-8 rounded-full flex items-center justify-center text-sm font-medium transition-colors ${
                  isDone
                    ? 'bg-brand text-surface-0'
                    : isActive
                      ? 'bg-brand text-surface-0 ring-2 ring-brand/20'
                      : 'bg-surface-2 text-text-tertiary'
                }`}
              >
                {isDone ? (
                  <svg className="w-4 h-4" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={3}>
                    <path strokeLinecap="round" strokeLinejoin="round" d="M5 13l4 4L19 7" />
                  </svg>
                ) : (
                  stepNum
                )}
              </div>
              <span className={`text-xs font-medium whitespace-nowrap ${isActive ? 'text-text-primary' : 'text-text-tertiary'}`}>
                {label}
              </span>
            </button>
          </div>
        )
      })}
    </div>
  )
}

// ── Step 1: Services ──────────────────────────────────────────────────────────

function OnboardingServices({
  allServices,
  onComplete,
  onSkip,
}: {
  allServices: ServiceInfo[]
  onComplete: () => void
  onSkip: () => void
}) {
  const qc = useQueryClient()
  const [aliasInputFor, setAliasInputFor] = useState<string | null>(null)
  const [aliasValue, setAliasValue] = useState('')
  const [keyInputFor, setKeyInputFor] = useState<string | null>(null)
  const [keyValue, setKeyValue] = useState('')
  const [keyAlias, setKeyAlias] = useState<string | undefined>(undefined)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)

  // Detect OAuth completion
  useEffect(() => {
    function handler(e: MessageEvent) {
      if (e.data?.type === 'clawvisor_oauth_done') {
        qc.invalidateQueries({ queryKey: ['services'] })
      }
    }
    window.addEventListener('message', handler)
    return () => window.removeEventListener('message', handler)
  }, [qc])

  // Check for newly activated services
  const { data: freshServices } = useQuery({
    queryKey: ['services'],
    queryFn: () => api.services.list(),
  })
  const hasActivated = (freshServices?.services ?? allServices).some(
    s => s.status === 'activated' && (s.requires_activation ?? true)
  )

  // Build deduplicated service types (only those requiring activation)
  const services = freshServices?.services ?? allServices
  const typeMap = new Map<string, ServiceType>()
  for (const svc of services) {
    if (!(svc.requires_activation ?? true)) continue
    const existing = typeMap.get(svc.id)
    if (existing) {
      if (svc.status === 'activated') existing.activatedCount++
    } else {
      typeMap.set(svc.id, {
        baseId: svc.id,
        oauth: svc.oauth,
        actions: svc.actions,
        activatedCount: svc.status === 'activated' ? 1 : 0,
      })
    }
  }
  const serviceTypes = Array.from(typeMap.values())

  async function handleActivateOAuth(serviceId: string, alias?: string) {
    setError(null)
    try {
      const resp = await api.services.oauthGetUrl(serviceId, undefined, alias)
      if (resp.already_authorized) {
        qc.invalidateQueries({ queryKey: ['services'] })
        return
      }
      if (resp.url) {
        const popup = window.open(resp.url, '_blank', 'width=600,height=700')
        if (!popup) window.location.href = resp.url
      }
    } catch (e: any) {
      setError(e.message ?? 'Failed to start OAuth flow')
    }
  }

  async function handleSaveKey() {
    if (!keyValue.trim() || !keyInputFor) return
    setSaving(true)
    setError(null)
    try {
      await api.services.activateWithKey(keyInputFor, keyValue.trim(), keyAlias)
      setKeyValue('')
      setKeyInputFor(null)
      setKeyAlias(undefined)
      qc.invalidateQueries({ queryKey: ['services'] })
    } catch (e: any) {
      setError(e.message ?? 'Failed to save API key')
    } finally {
      setSaving(false)
    }
  }

  function showAliasPrompt(st: ServiceType) {
    setError(null)
    setKeyInputFor(null)
    setKeyValue('')
    setAliasInputFor(st.baseId)
    setAliasValue('')
  }

  function confirmAlias(st: ServiceType) {
    const alias = aliasValue.trim() || undefined
    setAliasInputFor(null)
    setError(null)
    if (st.oauth) {
      handleActivateOAuth(st.baseId, alias)
    } else {
      setKeyInputFor(st.baseId)
      setKeyAlias(alias)
    }
  }

  return (
    <div className="space-y-5">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-lg font-semibold text-text-primary">How Clawvisor works</h2>
        </div>
        <div className="flex items-center gap-3 shrink-0">
          <button onClick={onSkip} className="text-sm text-text-tertiary hover:text-text-primary">
            Skip
          </button>
          {hasActivated && (
            <button
              onClick={onComplete}
              className="px-5 py-2 text-sm font-medium rounded bg-brand text-surface-0 hover:bg-brand-strong"
            >
              Continue
            </button>
          )}
        </div>
      </div>

      <div className="text-sm text-text-secondary space-y-2">
        <p>
          Clawvisor sits between your AI agent and sensitive APIs like Gmail, Slack, and GitHub.
          Your agent never holds credentials directly — instead, every request flows through Clawvisor,
          where you control what's allowed and what requires your approval.
        </p>
        <p>To get started, connect at least one service below.</p>
      </div>

      {error && <p className="text-sm text-danger">{error}</p>}

      <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-3">
        {serviceTypes.map(st => {
          const desc = serviceDescription(st.baseId)
          const isActivated = st.activatedCount > 0
          return (
            <div key={st.baseId} className="border border-border-default rounded-md p-4 space-y-2 bg-surface-1">
              <div className="flex items-center justify-between">
                <div>
                  <h3 className="font-semibold text-text-primary text-sm">{serviceName(st.baseId)}</h3>
                  {desc && <p className="text-xs text-text-tertiary mt-0.5">{desc}</p>}
                  <p className="text-xs text-text-tertiary mt-0.5">
                    {st.oauth ? 'OAuth' : 'API key'}
                  </p>
                </div>
                {isActivated && (
                  <span className="text-xs font-medium text-success bg-success/10 px-2 py-0.5 rounded">
                    Connected
                  </span>
                )}
              </div>

              {/* Alias input */}
              {aliasInputFor === st.baseId && (
                <div className="space-y-1.5">
                  <p className="text-xs text-text-tertiary">Label this connection (leave blank for default):</p>
                  <div className="flex gap-2">
                    <input
                      type="text"
                      value={aliasValue}
                      onChange={e => setAliasValue(e.target.value)}
                      onKeyDown={e => e.key === 'Enter' && confirmAlias(st)}
                      placeholder="e.g. personal, work"
                      className="flex-1 text-xs px-2 py-1.5 border border-border-default bg-surface-0 text-text-primary rounded focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand placeholder:text-text-tertiary"
                      autoFocus
                    />
                    <button
                      onClick={() => confirmAlias(st)}
                      className="text-xs px-3 py-1.5 rounded bg-brand text-surface-0 hover:bg-brand-strong"
                    >
                      Continue
                    </button>
                    <button
                      onClick={() => setAliasInputFor(null)}
                      className="text-xs px-3 py-1.5 rounded border border-border-strong text-text-primary hover:bg-surface-2"
                    >
                      Cancel
                    </button>
                  </div>
                </div>
              )}

              {/* API key input */}
              {keyInputFor === st.baseId && (
                <div className="flex gap-2">
                  <input
                    type="password"
                    value={keyValue}
                    onChange={e => setKeyValue(e.target.value)}
                    onKeyDown={e => e.key === 'Enter' && handleSaveKey()}
                    placeholder="Paste your token…"
                    className="flex-1 text-xs px-2 py-1.5 border border-border-default bg-surface-0 text-text-primary rounded focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand placeholder:text-text-tertiary"
                    autoFocus
                  />
                  <button
                    onClick={handleSaveKey}
                    disabled={saving || !keyValue.trim()}
                    className="text-xs px-3 py-1.5 rounded bg-brand text-surface-0 hover:bg-brand-strong disabled:opacity-50"
                  >
                    {saving ? 'Saving…' : 'Save'}
                  </button>
                  <button
                    onClick={() => { setKeyInputFor(null); setKeyValue('') }}
                    className="text-xs px-3 py-1.5 rounded border border-border-strong text-text-primary hover:bg-surface-2"
                  >
                    Cancel
                  </button>
                </div>
              )}

              {/* Activate button */}
              {aliasInputFor !== st.baseId && keyInputFor !== st.baseId && !isActivated && (
                <button
                  onClick={() => showAliasPrompt(st)}
                  className="text-xs px-3 py-1.5 rounded bg-brand text-surface-0 hover:bg-brand-strong"
                >
                  Activate
                </button>
              )}
            </div>
          )
        })}
      </div>
    </div>
  )
}

// ── Step 2: Create Agent ──────────────────────────────────────────────────────

function OnboardingAgent({ onComplete, onSkip }: { onComplete: () => void; onSkip: () => void }) {
  const qc = useQueryClient()
  const [name, setName] = useState('')
  const [newToken, setNewToken] = useState<string | null>(null)
  const [formError, setFormError] = useState<string | null>(null)
  const [copied, setCopied] = useState(false)

  const createMut = useMutation({
    mutationFn: () => api.agents.create(name),
    onSuccess: (agent) => {
      qc.invalidateQueries({ queryKey: ['agents'] })
      setNewToken(agent.token ?? null)
      setName('')
      setFormError(null)
    },
    onError: (err: Error) => setFormError(err.message),
  })

  function handleCopy() {
    if (!newToken) return
    navigator.clipboard.writeText(newToken)
    setCopied(true)
    setTimeout(() => setCopied(false), 2000)
  }

  return (
    <div className="space-y-5">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-lg font-semibold text-text-primary">Create an agent</h2>
          <p className="text-sm text-text-tertiary mt-1">
            Agents use tokens to make requests through Clawvisor. You'll need at least one.
          </p>
        </div>
        <button onClick={onSkip} className="text-sm text-text-tertiary hover:text-text-primary shrink-0">
          Skip
        </button>
      </div>

      {formError && <p className="text-sm text-danger">{formError}</p>}

      {!newToken ? (
        <div className="bg-surface-1 border border-border-default rounded-md p-5 space-y-4">
          <div className="flex gap-3">
            <input
              value={name}
              onChange={e => setName(e.target.value)}
              onKeyDown={e => e.key === 'Enter' && name.trim() && createMut.mutate()}
              placeholder="Agent name (e.g. claude, my-bot)"
              className="flex-1 text-sm rounded border border-border-default bg-surface-0 text-text-primary px-3 py-2 focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand placeholder:text-text-tertiary"
              autoFocus
            />
            <button
              onClick={() => createMut.mutate()}
              disabled={createMut.isPending || !name.trim()}
              className="px-5 py-2 text-sm font-medium rounded bg-brand text-surface-0 hover:bg-brand-strong disabled:opacity-50"
            >
              {createMut.isPending ? 'Creating…' : 'Create'}
            </button>
          </div>
        </div>
      ) : (
        <div className="bg-success/10 border border-success/30 rounded-md p-5 space-y-3">
          <p className="text-sm font-medium text-success">
            Agent created! Copy your token now — it won't be shown again.
          </p>
          <div className="flex items-center gap-2">
            <code className="flex-1 bg-surface-1 border border-success/30 rounded px-3 py-2 text-xs font-mono text-text-primary break-all">
              {newToken}
            </code>
            <button
              onClick={handleCopy}
              className="text-xs px-3 py-1.5 rounded border border-success/30 text-success hover:bg-success/10 min-w-[60px]"
            >
              {copied ? 'Copied!' : 'Copy'}
            </button>
          </div>
          <div className="flex justify-end pt-1">
            <button
              onClick={onComplete}
              className="px-5 py-2 text-sm font-medium rounded bg-brand text-surface-0 hover:bg-brand-strong"
            >
              Continue
            </button>
          </div>
        </div>
      )}
    </div>
  )
}

// ── Step 3: Telegram ──────────────────────────────────────────────────────────

function OnboardingTelegram({
  onComplete,
  onSkip,
}: {
  onComplete: () => void
  onSkip: () => void
}) {
  const qc = useQueryClient()
  const [error, setError] = useState<string | null>(null)

  const [botToken, setBotToken] = useState('')
  const [pairingId, setPairingId] = useState<string | null>(null)
  const [botUsername, setBotUsername] = useState<string | null>(null)
  const [pairingStatus, setPairingStatus] = useState<string | null>(null)
  const [code, setCode] = useState('')
  const [pairingSuccess, setPairingSuccess] = useState(false)
  const pollRef = useRef<ReturnType<typeof setInterval> | null>(null)

  const stopPolling = useCallback(() => {
    if (pollRef.current) {
      clearInterval(pollRef.current)
      pollRef.current = null
    }
  }, [])

  useEffect(() => () => stopPolling(), [stopPolling])

  // Start pairing
  const startMut = useMutation({
    mutationFn: () => api.notifications.startPairing(botToken),
    onSuccess: (data) => {
      setPairingId(data.pairing_id)
      setBotUsername(data.bot_username)
      setPairingStatus('polling')
      setError(null)
      setPairingSuccess(false)
      stopPolling()
      pollRef.current = setInterval(async () => {
        try {
          const s = await api.notifications.pairingStatus(data.pairing_id)
          setPairingStatus(s.status)
          if (s.status === 'ready' || s.status === 'expired' || s.status === 'confirmed') {
            stopPolling()
          }
        } catch {
          // ignore polling errors
        }
      }, 2000)
    },
    onError: (err: Error) => setError(err.message),
  })

  // Confirm pairing
  const confirmMut = useMutation({
    mutationFn: () => api.notifications.confirmPairing(pairingId!, code),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['notifications'] })
      setPairingSuccess(true)
      setPairingId(null)
      setPairingStatus(null)
      setBotToken('')
      setCode('')
      setError(null)
    },
    onError: (err: Error) => setError(err.message),
  })

  const resetPairing = () => {
    stopPolling()
    setPairingId(null)
    setPairingStatus(null)
    setBotUsername(null)
    setCode('')
    setError(null)
  }

  return (
    <div className="space-y-5">
      <div>
        <h2 className="text-lg font-semibold text-text-primary">Set up Telegram notifications</h2>
        <p className="text-sm text-text-tertiary mt-1">
          Get notified about approval requests and agent activity via Telegram.
        </p>
      </div>

      {error && <p className="text-sm text-danger">{error}</p>}

      {pairingSuccess ? (
        /* ── Success ────────────────────────────────────────────── */
        <div className="bg-success/10 border border-success/30 rounded-md p-5 space-y-3">
          <p className="text-sm font-medium text-success">Telegram paired successfully!</p>
          <div className="flex justify-end">
            <button
              onClick={onComplete}
              className="px-5 py-2 text-sm font-medium rounded bg-brand text-surface-0 hover:bg-brand-strong"
            >
              Finish
            </button>
          </div>
        </div>
      ) : !pairingId ? (
        /* ── Enter bot token ───────────────────────────────────── */
        <div className="space-y-3">
          <div className="bg-surface-2 border border-border-default rounded-md p-4 text-sm text-text-secondary space-y-2">
            <p className="font-medium text-text-primary">Setup steps:</p>
            <ol className="list-decimal list-inside space-y-1.5 text-text-secondary">
              <li>Open Telegram and message <a href="https://t.me/BotFather" target="_blank" rel="noreferrer" className="text-brand hover:underline">@BotFather</a></li>
              <li>Send <code className="bg-surface-2 px-1 rounded text-xs">/newbot</code> and follow the prompts</li>
              <li>Copy the <strong>bot token</strong> BotFather gives you</li>
            </ol>
          </div>
          <div className="bg-surface-1 border border-border-default rounded-md p-5 space-y-3">
            <div>
              <label className="text-xs font-medium text-text-tertiary">Bot Token</label>
              <input
                type="password"
                value={botToken}
                onChange={e => { setBotToken(e.target.value); setError(null) }}
                placeholder="1234567890:ABCDEF..."
                className="mt-1 block w-full text-sm rounded border border-border-default bg-surface-0 text-text-primary px-3 py-1.5 focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand placeholder:text-text-tertiary"
              />
            </div>
            <button
              onClick={() => startMut.mutate()}
              disabled={startMut.isPending || !botToken}
              className="px-4 py-1.5 text-sm rounded bg-brand text-surface-0 hover:bg-brand-strong disabled:opacity-50"
            >
              {startMut.isPending ? 'Validating...' : 'Start Pairing'}
            </button>
          </div>
        </div>
      ) : pairingStatus === 'polling' ? (
        /* ── Waiting for /start ────────────────────────────────── */
        <div className="bg-surface-1 border border-border-default rounded-md p-5 space-y-3">
          <p className="text-sm text-text-secondary">
            Open{' '}
            <a
              href={`https://t.me/${botUsername}`}
              target="_blank"
              rel="noreferrer"
              className="text-brand hover:underline font-medium"
            >
              @{botUsername}
            </a>{' '}
            in Telegram and send <code className="bg-surface-2 px-1 rounded text-xs">/start</code>
          </p>
          <div className="flex items-center gap-2 text-sm text-text-tertiary">
            <svg className="animate-spin h-4 w-4 text-brand" viewBox="0 0 24 24" fill="none">
              <circle className="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" strokeWidth="4" />
              <path className="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4z" />
            </svg>
            Waiting for your message...
          </div>
          <button onClick={resetPairing} className="text-sm text-text-tertiary hover:text-text-primary">
            Cancel
          </button>
        </div>
      ) : pairingStatus === 'ready' ? (
        /* ── Enter pairing code ────────────────────────────────── */
        <div className="bg-surface-1 border border-border-default rounded-md p-5 space-y-3">
          <p className="text-sm text-text-secondary">
            Enter the pairing code from your Telegram chat:
          </p>
          <input
            value={code}
            onChange={e => { setCode(e.target.value.toUpperCase()); setError(null) }}
            placeholder="ABCD1234"
            maxLength={8}
            className="block w-48 text-sm rounded border border-border-default bg-surface-0 text-text-primary px-3 py-1.5 font-mono tracking-widest uppercase focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand placeholder:text-text-tertiary"
          />
          <div className="flex items-center gap-2">
            <button
              onClick={() => confirmMut.mutate()}
              disabled={confirmMut.isPending || code.length !== 8}
              className="px-4 py-1.5 text-sm rounded bg-brand text-surface-0 hover:bg-brand-strong disabled:opacity-50"
            >
              {confirmMut.isPending ? 'Confirming...' : 'Confirm'}
            </button>
            <button onClick={resetPairing} className="text-sm text-text-tertiary hover:text-text-primary">
              Cancel
            </button>
          </div>
        </div>
      ) : pairingStatus === 'expired' ? (
        /* ── Expired ───────────────────────────────────────────── */
        <div className="bg-surface-1 border border-border-default rounded-md p-5 space-y-3">
          <p className="text-sm text-danger">Pairing session expired. Please try again.</p>
          <button
            onClick={resetPairing}
            className="px-4 py-1.5 text-sm rounded bg-brand text-surface-0 hover:bg-brand-strong"
          >
            Start Over
          </button>
        </div>
      ) : null}

      <div className="pt-2">
        <button onClick={onSkip} className="text-sm text-text-tertiary hover:text-text-primary">
          Skip — set this up later in Settings
        </button>
      </div>
    </div>
  )
}

// ── Onboarding (parent) ───────────────────────────────────────────────────────

function firstIncompleteStep(completed: number[]): number {
  for (let i = 1; i <= 3; i++) {
    if (!completed.includes(i)) return i
  }
  return 1
}

export default function Onboarding({
  allServices,
  initialCompleted,
  onDismiss,
}: {
  allServices: ServiceInfo[]
  initialCompleted: number[]
  onDismiss: () => void
}) {
  const qc = useQueryClient()
  const [completed, setCompleted] = useState<number[]>(initialCompleted)
  const [step, setStep] = useState(() => firstIncompleteStep(initialCompleted))

  function markComplete(s: number) {
    setCompleted(prev => {
      if (prev.includes(s)) return prev
      const next = [...prev, s]
      // All done — auto-dismiss
      if (next.length === 3) {
        qc.invalidateQueries({ queryKey: ['services'] })
        qc.invalidateQueries({ queryKey: ['agents'] })
        qc.invalidateQueries({ queryKey: ['notifications'] })
        // defer so state update finishes before unmount
        setTimeout(onDismiss, 0)
      }
      return next
    })
  }

  function advance(currentStep: number) {
    markComplete(currentStep)
    const next = currentStep + 1
    if (next <= 3) setStep(next)
  }

  function skip(currentStep: number) {
    const next = currentStep + 1
    if (next <= 3) {
      setStep(next)
    } else {
      onDismiss()
    }
  }

  return (
    <section className="border border-border-default rounded-md bg-surface-1 p-6 space-y-6">
      {/* Header */}
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-lg font-semibold text-text-primary">Get started</h2>
          <p className="text-sm text-text-tertiary mt-0.5">Three quick steps to start using Clawvisor.</p>
        </div>
        <button
          onClick={onDismiss}
          className="text-text-tertiary hover:text-text-primary text-sm"
        >
          Dismiss
        </button>
      </div>

      {/* Stepper */}
      <Stepper current={step} completed={completed} onStepClick={setStep} />

      {/* Step content */}
      {step === 1 && (
        <OnboardingServices
          allServices={allServices}
          onComplete={() => advance(1)}
          onSkip={() => skip(1)}
        />
      )}
      {step === 2 && (
        <OnboardingAgent onComplete={() => advance(2)} onSkip={() => skip(2)} />
      )}
      {step === 3 && (
        <OnboardingTelegram
          onComplete={() => advance(3)}
          onSkip={() => skip(3)}
        />
      )}
    </section>
  )
}
