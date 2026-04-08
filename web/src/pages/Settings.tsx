import { useState, useEffect, useRef, useCallback } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import type { NotificationConfig, PendingGroup } from '../api/client'
import { useNavigate } from 'react-router-dom'
import { api, APIError } from '../api/client'
import { useAuth } from '../hooks/useAuth'
import { QRCodeSVG } from 'qrcode.react'
import CountdownTimer from '../components/CountdownTimer'
import { formatDistanceToNow } from 'date-fns'

export default function Settings() {
  const { features } = useAuth()
  const passwordAuth = features?.password_auth ?? false

  return (
    <div className="p-8 space-y-10">
      <h1 className="text-2xl font-bold text-text-primary">Settings</h1>
      <DaemonInfo />
      {!features?.multi_tenant && <LLMSection />}
      {!features?.multi_tenant && <GoogleOAuthSection />}
      <DevicePairing />
      <TelegramSetupSection />
      {passwordAuth && <PasswordSection />}
      {passwordAuth && <DangerZone />}
    </div>
  )
}

// ── Daemon ID display ────────────────────────────────────────────────────────

function DaemonInfo() {
  const [copied, setCopied] = useState(false)

  const { data: pairInfo } = useQuery({
    queryKey: ['pair-info'],
    queryFn: () => api.devices.pairInfo(),
    retry: false,
  })

  if (!pairInfo?.daemon_id) return null

  function copyId() {
    navigator.clipboard.writeText(pairInfo!.daemon_id)
    setCopied(true)
    setTimeout(() => setCopied(false), 2000)
  }

  return (
    <section className="space-y-4">
      <div>
        <h2 className="text-lg font-semibold text-text-primary">Daemon ID</h2>
        <p className="text-sm text-text-tertiary mt-0.5">
          Use this ID to connect MCP clients or other tools to your daemon via the relay.
        </p>
      </div>
      <div className="bg-surface-1 border border-border-default rounded-md px-5 py-4 flex items-center justify-between max-w-lg">
        <code className="text-lg font-mono font-semibold text-text-primary tracking-wide select-all">
          {pairInfo.daemon_id}
        </code>
        <button
          onClick={copyId}
          className="ml-4 px-3 py-1.5 text-xs rounded border border-border-strong text-text-secondary hover:bg-surface-2 transition-colors"
        >
          {copied ? 'Copied!' : 'Copy'}
        </button>
      </div>
    </section>
  )
}

// ── LLM configuration ───────────────────────────────────────────────────────

function LLMSection() {
  const qc = useQueryClient()
  const [editing, setEditing] = useState(false)
  const [provider, setProvider] = useState('')
  const [endpoint, setEndpoint] = useState('')
  const [apiKey, setApiKey] = useState('')
  const [model, setModel] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [success, setSuccess] = useState(false)

  const { data: status } = useQuery({
    queryKey: ['llm-status'],
    queryFn: () => api.llm.status(),
  })

  const updateMut = useMutation({
    mutationFn: () => api.llm.update(provider, endpoint, apiKey, model),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['llm-status'] })
      setEditing(false)
      setApiKey('')
      setError(null)
      setSuccess(true)
      setTimeout(() => setSuccess(false), 5000)
    },
    onError: (err: Error) => setError(err.message),
  })

  function startEditing() {
    setProvider(status?.provider ?? 'anthropic')
    setEndpoint('')
    setApiKey('')
    setModel(status?.model ?? '')
    setError(null)
    setEditing(true)
  }

  function handleSubmit() {
    if (!apiKey) { setError('API key is required'); return }
    setError(null)
    updateMut.mutate()
  }

  return (
    <section className="space-y-4">
      <div>
        <h2 className="text-lg font-semibold text-text-primary">LLM Configuration</h2>
        <p className="text-sm text-text-tertiary mt-0.5">
          API key used for intent verification and risk assessment.
        </p>
      </div>

      {error && <div className="text-sm text-danger max-w-lg">{error}</div>}
      {success && <div className="text-sm text-success max-w-lg">LLM configuration updated.</div>}

      {status?.spend_cap_exhausted && !editing && (
        <div className="max-w-lg px-4 py-2.5 rounded-md bg-warning/10 border border-warning/30 text-sm text-text-primary">
          Free LLM credit exhausted. Add your own API key to restore verification and risk assessment.
        </div>
      )}

      {!editing ? (
        <div className="bg-surface-1 border border-border-default rounded-md p-5 space-y-3 max-w-lg">
          <div className="text-sm text-text-secondary space-y-1">
            <p><span className="font-medium text-text-tertiary">Provider:</span> {status?.provider ?? '—'}</p>
            <p><span className="font-medium text-text-tertiary">Model:</span> {status?.model ?? '—'}</p>
            <p>
              <span className="font-medium text-text-tertiary">Status:</span>{' '}
              {status?.spend_cap_exhausted
                ? <span className="text-warning">Credit exhausted</span>
                : <span className="text-success">Active</span>}
            </p>
          </div>
          {status?.usage && (
            <div className="space-y-1.5">
              <div className="flex items-center justify-between text-xs text-text-tertiary">
                <span>Free credit</span>
                <span>{Math.round(100 - status.usage.pct_used)}% remaining</span>
              </div>
              <div className="h-2 rounded-full bg-surface-2 overflow-hidden">
                <div
                  className={`h-full rounded-full transition-all ${
                    status.usage.pct_used >= 90 ? 'bg-danger' : status.usage.pct_used >= 70 ? 'bg-warning' : 'bg-brand'
                  }`}
                  style={{ width: `${Math.min(status.usage.pct_used, 100)}%` }}
                />
              </div>
            </div>
          )}
          <button
            onClick={startEditing}
            className="px-4 py-1.5 text-sm rounded border border-brand/30 text-brand hover:bg-brand/10"
          >
            {status?.spend_cap_exhausted ? 'Configure API key' : 'Update'}
          </button>
        </div>
      ) : (
        <div className="bg-surface-1 border border-border-default rounded-md p-5 space-y-3 max-w-lg">
          <div>
            <label className="text-xs font-medium text-text-tertiary">Provider</label>
            <select
              value={provider}
              onChange={e => setProvider(e.target.value)}
              className="mt-1 block w-full text-sm rounded border border-border-default bg-surface-0 text-text-primary px-3 py-1.5 focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand"
            >
              <option value="anthropic">Anthropic</option>
              <option value="openai">OpenAI</option>
            </select>
          </div>
          <div>
            <label className="text-xs font-medium text-text-tertiary">Endpoint <span className="text-text-tertiary font-normal">(optional, leave blank for default)</span></label>
            <input
              type="text"
              value={endpoint}
              onChange={e => setEndpoint(e.target.value)}
              placeholder={provider === 'openai' ? 'https://api.openai.com/v1' : 'https://api.anthropic.com/v1'}
              className="mt-1 block w-full text-sm rounded border border-border-default bg-surface-0 text-text-primary px-3 py-1.5 focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand placeholder:text-text-tertiary"
            />
          </div>
          <div>
            <label className="text-xs font-medium text-text-tertiary">API Key</label>
            <input
              type="password"
              value={apiKey}
              onChange={e => { setApiKey(e.target.value); setError(null) }}
              placeholder="sk-..."
              className="mt-1 block w-full text-sm rounded border border-border-default bg-surface-0 text-text-primary px-3 py-1.5 focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand placeholder:text-text-tertiary"
            />
          </div>
          <div>
            <label className="text-xs font-medium text-text-tertiary">Model <span className="text-text-tertiary font-normal">(optional)</span></label>
            <input
              type="text"
              value={model}
              onChange={e => setModel(e.target.value)}
              placeholder="claude-haiku-4-5-20251001"
              className="mt-1 block w-full text-sm rounded border border-border-default bg-surface-0 text-text-primary px-3 py-1.5 focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand placeholder:text-text-tertiary"
            />
          </div>
          <div className="flex items-center gap-2 pt-1">
            <button
              onClick={handleSubmit}
              disabled={updateMut.isPending || !apiKey}
              className="px-4 py-1.5 text-sm rounded bg-brand text-surface-0 hover:bg-brand-strong disabled:opacity-50"
            >
              {updateMut.isPending ? 'Saving…' : 'Save'}
            </button>
            <button
              onClick={() => { setEditing(false); setError(null) }}
              className="text-sm text-text-tertiary hover:text-text-primary"
            >
              Cancel
            </button>
          </div>
        </div>
      )}
    </section>
  )
}

// ── Google OAuth credentials ─────────────────────────────────────────────────

function GoogleOAuthSection() {
  const [editing, setEditing] = useState(false)
  const [clientId, setClientId] = useState('')
  const [clientSecret, setClientSecret] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [success, setSuccess] = useState(false)

  const saveMut = useMutation({
    mutationFn: () => api.system.setGoogleOAuth(clientId, clientSecret),
    onSuccess: () => {
      setEditing(false)
      setClientId('')
      setClientSecret('')
      setError(null)
      setSuccess(true)
      setTimeout(() => setSuccess(false), 5000)
    },
    onError: (err: Error) => setError(err.message),
  })

  function handleSubmit() {
    if (!clientId || !clientSecret) { setError('Both fields are required'); return }
    setError(null)
    saveMut.mutate()
  }

  return (
    <section className="space-y-4">
      <div>
        <h2 className="text-lg font-semibold text-text-primary">Google OAuth</h2>
        <p className="text-sm text-text-tertiary mt-0.5">
          Configure Google OAuth credentials to enable Gmail, Calendar, Drive, and Contacts adapters.
        </p>
      </div>

      {error && <div className="text-sm text-danger max-w-lg">{error}</div>}
      {success && <div className="text-sm text-success max-w-lg">Google OAuth credentials saved.</div>}

      {!editing ? (
        <button
          onClick={() => { setEditing(true); setError(null); setSuccess(false) }}
          className="px-4 py-1.5 text-sm rounded border border-brand/30 text-brand hover:bg-brand/10"
        >
          Configure
        </button>
      ) : (
        <div className="bg-surface-1 border border-border-default rounded-md p-5 space-y-3 max-w-lg">
          <div>
            <label className="text-xs font-medium text-text-tertiary">Client ID</label>
            <input
              type="text"
              value={clientId}
              onChange={e => { setClientId(e.target.value); setError(null) }}
              placeholder="123456789.apps.googleusercontent.com"
              className="mt-1 block w-full text-sm rounded border border-border-default bg-surface-0 text-text-primary px-3 py-1.5 focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand placeholder:text-text-tertiary"
            />
          </div>
          <div>
            <label className="text-xs font-medium text-text-tertiary">Client Secret</label>
            <input
              type="password"
              value={clientSecret}
              onChange={e => { setClientSecret(e.target.value); setError(null) }}
              placeholder="GOCSPX-..."
              className="mt-1 block w-full text-sm rounded border border-border-default bg-surface-0 text-text-primary px-3 py-1.5 focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand placeholder:text-text-tertiary"
            />
          </div>
          <div className="flex items-center gap-2 pt-1">
            <button
              onClick={handleSubmit}
              disabled={saveMut.isPending || !clientId || !clientSecret}
              className="px-4 py-1.5 text-sm rounded bg-brand text-surface-0 hover:bg-brand-strong disabled:opacity-50"
            >
              {saveMut.isPending ? 'Saving…' : 'Save'}
            </button>
            <button
              onClick={() => { setEditing(false); setError(null) }}
              className="text-sm text-text-tertiary hover:text-text-primary"
            >
              Cancel
            </button>
          </div>
        </div>
      )}
    </section>
  )
}

// ── Telegram Setup (progressive stepper) ────────────────────────────────────

function StepHeader({ step, title, done, active, onToggle }: {
  step: number
  title: string
  done: boolean
  active: boolean
  onToggle?: () => void
}) {
  return (
    <button
      onClick={onToggle}
      disabled={!onToggle}
      className="flex items-center gap-3 w-full text-left group"
    >
      <span className={`flex-shrink-0 w-7 h-7 rounded-full flex items-center justify-center text-xs font-bold border-2 transition-colors ${
        done
          ? 'bg-green-500/15 border-green-500/40 text-green-500'
          : active
            ? 'bg-brand/15 border-brand/40 text-brand'
            : 'bg-surface-2 border-border-default text-text-tertiary'
      }`}>
        {done ? (
          <svg className="w-3.5 h-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="3" strokeLinecap="round" strokeLinejoin="round">
            <path d="M20 6L9 17l-5-5" />
          </svg>
        ) : step}
      </span>
      <span className={`text-sm font-medium transition-colors ${
        active ? 'text-text-primary' : done ? 'text-text-secondary group-hover:text-text-primary' : 'text-text-tertiary'
      }`}>
        {title}
      </span>
    </button>
  )
}

function TelegramSetupSection() {
  const qc = useQueryClient()

  // ── Shared state ─────────────────────────────────────────
  const [error, setError] = useState<string | null>(null)
  const [expandedStep, setExpandedStep] = useState<number | null>(null)

  // Bot pairing state
  const [botToken, setBotToken] = useState('')
  const [pairingId, setPairingId] = useState<string | null>(null)
  const [botUsername, setBotUsername] = useState<string | null>(null)
  const [pairingStatus, setPairingStatus] = useState<string | null>(null)
  const [code, setCode] = useState('')
  const [testResult, setTestResult] = useState<'success' | 'error' | null>(null)
  const pollRef = useRef<ReturnType<typeof setInterval> | null>(null)

  // ── Queries ──────────────────────────────────────────────
  const { data: configs } = useQuery({
    queryKey: ['notifications'],
    queryFn: (): Promise<NotificationConfig[]> => api.notifications.list(),
  })

  const tg = configs?.find((c: NotificationConfig) => c.channel === 'telegram')
  const hasBotToken = Boolean(tg?.config?.bot_token)
  const activeGroupId = tg?.config?.group_chat_id
  const autoApprovalEnabled = Boolean(tg?.config?.auto_approval_enabled)

  const { data: pendingGroups } = useQuery({
    queryKey: ['telegram-groups'],
    queryFn: () => api.notifications.listTelegramGroups(),
    enabled: hasBotToken,
  })

  const { data: pairedAgents } = useQuery({
    queryKey: ['paired-agents'],
    queryFn: () => api.notifications.listPairedAgents(),
    enabled: Boolean(activeGroupId),
    refetchInterval: 10000,
  })

  // Derive current step
  const currentStep = !hasBotToken ? 1
    : !activeGroupId ? 2
    : !autoApprovalEnabled ? 3
    : 4

  // Auto-expand to current step
  useEffect(() => {
    setExpandedStep(currentStep)
  }, [currentStep])

  // ── Polling helpers ──────────────────────────────────────
  const stopPolling = useCallback(() => {
    if (pollRef.current) {
      clearInterval(pollRef.current)
      pollRef.current = null
    }
  }, [])

  useEffect(() => () => stopPolling(), [stopPolling])

  const resetPairing = () => {
    stopPolling()
    setPairingId(null)
    setPairingStatus(null)
    setBotUsername(null)
    setCode('')
    setError(null)
  }

  // ── Mutations ────────────────────────────────────────────
  const startMut = useMutation({
    mutationFn: () => api.notifications.startPairing(botToken),
    onSuccess: (data) => {
      setPairingId(data.pairing_id)
      setBotUsername(data.bot_username)
      setPairingStatus('polling')
      setError(null)
      stopPolling()
      pollRef.current = setInterval(async () => {
        try {
          const s = await api.notifications.pairingStatus(data.pairing_id)
          setPairingStatus(s.status)
          if (s.status === 'ready' || s.status === 'expired' || s.status === 'confirmed') {
            stopPolling()
          }
        } catch { /* ignore */ }
      }, 2000)
    },
    onError: (err: Error) => setError(err.message),
  })

  const confirmMut = useMutation({
    mutationFn: () => api.notifications.confirmPairing(pairingId!, code),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['notifications'] })
      resetPairing()
      setBotToken('')
    },
    onError: (err: Error) => setError(err.message),
  })

  const deleteBotMut = useMutation({
    mutationFn: () => api.notifications.deleteTelegram(),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['notifications'] })
      resetPairing()
      setBotToken('')
      setTestResult(null)
    },
  })

  const testMut = useMutation({
    mutationFn: () => api.notifications.testTelegram(),
    onSuccess: () => { setTestResult('success'); setTimeout(() => setTestResult(null), 5000) },
    onError: () => { setTestResult('error'); setTimeout(() => setTestResult(null), 5000) },
  })

  const detectMut = useMutation({
    mutationFn: () => api.notifications.detectTelegramGroups(),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ['telegram-groups'] }) },
  })

  const enableGroupMut = useMutation({
    mutationFn: (chatId: string) => api.notifications.upsertTelegramGroup(chatId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['notifications'] })
      qc.invalidateQueries({ queryKey: ['telegram-groups'] })
    },
  })

  const disableGroupMut = useMutation({
    mutationFn: () => api.notifications.deleteTelegramGroup(),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ['notifications'] }) },
  })

  const dismissMut = useMutation({
    mutationFn: (chatId: string) => api.notifications.dismissTelegramGroup(chatId),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ['telegram-groups'] }) },
  })

  const autoApprovalMut = useMutation({
    mutationFn: (enabled: boolean) => api.notifications.setAutoApproval(enabled),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ['notifications'] }) },
  })

  const agentPairingMut = useMutation({
    mutationFn: () => api.notifications.createGroupPairing(),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ['paired-agents'] }) },
  })

  // ── Toggle helper for completed steps ────────────────────
  const toggleStep = (step: number) => {
    setExpandedStep(prev => prev === step ? null : step)
  }

  return (
    <section className="space-y-4">
      <div>
        <h2 className="text-lg font-semibold text-text-primary">Telegram</h2>
        <p className="text-sm text-text-tertiary mt-0.5">
          Receive notifications, approve tasks inline, and enable auto-approval via group chat context.
        </p>
      </div>

      {error && <div className="text-sm text-danger max-w-xl">{error}</div>}

      <div className="max-w-xl space-y-1">
        {/* ── Step 1: Connect Bot ────────────────────────────── */}
        <div className="bg-surface-1 border border-border-default rounded-md px-5 py-4 space-y-3">
          <StepHeader
            step={1}
            title="Connect your Telegram bot"
            done={hasBotToken}
            active={currentStep === 1}
            onToggle={hasBotToken ? () => toggleStep(1) : undefined}
          />

          {expandedStep === 1 && (
            <div className="ml-10 space-y-3">
              {!hasBotToken ? (
                <>
                  {!pairingId ? (
                    <>
                      <div className="bg-surface-2 border border-border-default rounded-md p-3 text-xs text-text-secondary space-y-1.5">
                        <ol className="list-decimal list-inside space-y-1">
                          <li>Message <a href="https://t.me/BotFather" target="_blank" rel="noreferrer" className="text-brand hover:underline">@BotFather</a> on Telegram</li>
                          <li>Send <code className="bg-surface-1 px-1 rounded">/newbot</code> and follow the prompts</li>
                          <li>Copy the bot token you receive</li>
                        </ol>
                      </div>
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
                    </>
                  ) : pairingStatus === 'polling' ? (
                    <>
                      <p className="text-sm text-text-secondary">
                        Open{' '}
                        <a href={`https://t.me/${botUsername}`} target="_blank" rel="noreferrer" className="text-brand hover:underline font-medium">@{botUsername}</a>
                        {' '}and send <code className="bg-surface-2 px-1 rounded text-xs">/start</code>
                      </p>
                      <div className="flex items-center gap-2 text-sm text-text-tertiary">
                        <svg className="animate-spin h-4 w-4 text-brand" viewBox="0 0 24 24" fill="none">
                          <circle className="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" strokeWidth="4" />
                          <path className="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4z" />
                        </svg>
                        Waiting for your message...
                      </div>
                      <button onClick={resetPairing} className="text-xs text-text-tertiary hover:text-text-primary">Cancel</button>
                    </>
                  ) : pairingStatus === 'ready' ? (
                    <>
                      <p className="text-sm text-text-secondary">Enter the pairing code from your Telegram chat:</p>
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
                        <button onClick={resetPairing} className="text-xs text-text-tertiary hover:text-text-primary">Cancel</button>
                      </div>
                    </>
                  ) : pairingStatus === 'expired' ? (
                    <>
                      <p className="text-sm text-danger">Pairing session expired.</p>
                      <button onClick={resetPairing} className="px-3 py-1 text-xs rounded bg-brand text-surface-0 hover:bg-brand-strong">Start Over</button>
                    </>
                  ) : null}
                </>
              ) : (
                /* Configured — collapsed view */
                <div className="space-y-2">
                  <div className="text-sm text-text-secondary space-y-0.5">
                    <p><span className="text-text-tertiary">Bot token:</span> {tg!.config.bot_token.slice(0, 8)}...{tg!.config.bot_token.slice(-4)}</p>
                    <p><span className="text-text-tertiary">Chat ID:</span> {tg!.config.chat_id}</p>
                  </div>
                  <div className="flex items-center gap-2">
                    <button
                      onClick={() => testMut.mutate()}
                      disabled={testMut.isPending}
                      className="px-3 py-1 text-xs rounded border border-brand/30 text-brand hover:bg-brand/10 disabled:opacity-50"
                    >
                      {testMut.isPending ? 'Sending...' : 'Test'}
                    </button>
                    <button
                      onClick={() => { deleteBotMut.mutate() }}
                      disabled={deleteBotMut.isPending}
                      className="text-xs text-danger hover:text-red-400"
                    >
                      Remove
                    </button>
                  </div>
                  {testResult === 'success' && <p className="text-xs text-success">Test message sent!</p>}
                  {testResult === 'error' && <p className="text-xs text-danger">Test failed. Check bot settings.</p>}
                </div>
              )}
            </div>
          )}
        </div>

        {/* ── Step 2: Create Group ───────────────────────────── */}
        <div className={`bg-surface-1 border border-border-default rounded-md px-5 py-4 space-y-3 ${!hasBotToken ? 'opacity-50' : ''}`}>
          <StepHeader
            step={2}
            title="Create a group chat"
            done={Boolean(activeGroupId)}
            active={currentStep === 2}
            onToggle={hasBotToken && activeGroupId ? () => toggleStep(2) : undefined}
          />

          {expandedStep === 2 && hasBotToken && (
            <div className="ml-10 space-y-3">
              {!activeGroupId ? (
                <>
                  <div className="bg-surface-2 border border-border-default rounded-md p-3 text-xs text-text-secondary space-y-2">
                    <p className="font-medium text-text-primary">Create a Telegram group with your bot and agent:</p>
                    <ol className="list-decimal list-inside space-y-1.5">
                      <li>Create a new Telegram group and add your bot</li>
                      <li>
                        <strong>Disable bot privacy mode:</strong> message{' '}
                        <a href="https://t.me/BotFather" target="_blank" rel="noreferrer" className="text-brand hover:underline">@BotFather</a>,
                        send <code className="bg-surface-1 px-1 rounded">/mybots</code>, select your bot, go to{' '}
                        <em>Bot Settings &rarr; Group Privacy &rarr; Turn off</em>
                      </li>
                      <li>
                        <strong>Configure your agent for group channels:</strong> if using OpenClaw, enable{' '}
                        <code className="bg-surface-1 px-1 rounded">group_channels</code> in your bot&apos;s config so it can send and receive messages in groups
                      </li>
                      <li>Add your agent bot to the same group</li>
                    </ol>
                  </div>

                  <div className="flex items-center justify-between">
                    <p className="text-xs text-text-tertiary">Once set up, scan for groups your bot has been added to.</p>
                    <button
                      onClick={() => detectMut.mutate()}
                      disabled={detectMut.isPending}
                      className="flex items-center gap-1.5 px-3 py-1 text-xs rounded border border-border-default text-text-tertiary hover:text-text-primary hover:border-border-hover disabled:opacity-50"
                    >
                      {detectMut.isPending ? (
                        <svg className="animate-spin h-3 w-3" viewBox="0 0 24 24" fill="none">
                          <circle className="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" strokeWidth="4" />
                          <path className="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4z" />
                        </svg>
                      ) : (
                        <svg className="h-3 w-3" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2">
                          <path d="M1 4v6h6M23 20v-6h-6" />
                          <path d="M20.49 9A9 9 0 005.64 5.64L1 10m22 4l-4.64 4.36A9 9 0 013.51 15" />
                        </svg>
                      )}
                      Scan for Groups
                    </button>
                  </div>

                  {pendingGroups && pendingGroups.length > 0 ? (
                    <div className="bg-surface-0 border border-border-default rounded-md divide-y divide-border-default">
                      {pendingGroups.map((g: PendingGroup) => (
                        <div key={g.chat_id} className="flex items-center justify-between px-4 py-3">
                          <div className="text-sm">
                            <span className="text-text-primary font-medium">{g.title || g.chat_id}</span>
                            <span className="text-text-tertiary ml-2 text-xs">({g.type})</span>
                          </div>
                          <div className="flex items-center gap-2">
                            <button
                              onClick={() => enableGroupMut.mutate(g.chat_id)}
                              disabled={enableGroupMut.isPending}
                              className="px-3 py-1 text-xs rounded bg-brand text-surface-0 hover:bg-brand-strong disabled:opacity-50"
                            >
                              Connect
                            </button>
                            <button
                              onClick={() => dismissMut.mutate(g.chat_id)}
                              disabled={dismissMut.isPending}
                              className="text-xs text-text-tertiary hover:text-text-primary"
                            >
                              Dismiss
                            </button>
                          </div>
                        </div>
                      ))}
                    </div>
                  ) : (
                    <p className="text-xs text-text-tertiary">
                      No groups detected yet. Add your bot to a group, then click Scan.
                    </p>
                  )}
                </>
              ) : (
                /* Group connected — collapsed view */
                <div className="space-y-2">
                  <p className="text-sm text-text-secondary">
                    <span className="text-text-tertiary">Group:</span>{' '}
                    <span className="font-mono">{activeGroupId}</span>
                  </p>
                  <button
                    onClick={() => disableGroupMut.mutate()}
                    disabled={disableGroupMut.isPending}
                    className="text-xs text-danger hover:text-red-400"
                  >
                    Disconnect Group
                  </button>
                </div>
              )}
            </div>
          )}
        </div>

        {/* ── Step 3: Enable Auto-Approval ───────────────────── */}
        <div className={`bg-surface-1 border border-border-default rounded-md px-5 py-4 space-y-3 ${!activeGroupId ? 'opacity-50' : ''}`}>
          <StepHeader
            step={3}
            title="Enable auto-approval"
            done={autoApprovalEnabled}
            active={currentStep === 3}
            onToggle={activeGroupId ? () => toggleStep(3) : undefined}
          />

          {expandedStep === 3 && activeGroupId && (
            <div className="ml-10 space-y-3">
              <p className="text-xs text-text-secondary leading-relaxed">
                When enabled, Clawvisor reads your group chat to detect when you&apos;ve approved a task in conversation.
                If the LLM finds clear approval, the task is auto-approved without requiring a dashboard click.
              </p>
              <label className="flex items-center gap-3 cursor-pointer">
                <button
                  onClick={() => autoApprovalMut.mutate(!autoApprovalEnabled)}
                  disabled={autoApprovalMut.isPending}
                  className={`relative inline-flex h-5 w-9 flex-shrink-0 rounded-full border-2 border-transparent transition-colors ${
                    autoApprovalEnabled ? 'bg-green-500' : 'bg-surface-2'
                  }`}
                >
                  <span
                    className={`pointer-events-none inline-block h-4 w-4 rounded-full bg-white shadow transform transition-transform ${
                      autoApprovalEnabled ? 'translate-x-4' : 'translate-x-0'
                    }`}
                  />
                </button>
                <span className="text-sm text-text-secondary">
                  {autoApprovalEnabled ? 'Auto-approval is on' : 'Auto-approval is off'}
                </span>
              </label>
            </div>
          )}
        </div>

        {/* ── Step 4: Agent Pairing ──────────────────────────── */}
        <div className={`bg-surface-1 border border-border-default rounded-md px-5 py-4 space-y-3 ${!activeGroupId ? 'opacity-50' : ''}`}>
          <StepHeader
            step={4}
            title="Pair agents"
            done={Boolean(pairedAgents && pairedAgents.length > 0)}
            active={currentStep === 4}
            onToggle={activeGroupId ? () => toggleStep(4) : undefined}
          />

          {expandedStep === 4 && activeGroupId && (
            <div className="ml-10 space-y-3">
              <p className="text-xs text-text-secondary leading-relaxed">
                Pair each agent to this group so auto-approval is scoped correctly.
                Each agent only checks the group it&apos;s paired to.
              </p>

              {pairedAgents && pairedAgents.length > 0 && (
                <div className="flex flex-wrap gap-2">
                  {pairedAgents.map((a) => (
                    <span
                      key={a.id}
                      className="inline-flex items-center gap-1.5 px-2.5 py-1 text-xs rounded-full bg-green-500/10 text-green-500 border border-green-500/20"
                    >
                      <span className="w-1.5 h-1.5 rounded-full bg-green-500" />
                      {a.name}
                    </span>
                  ))}
                </div>
              )}

              <button
                onClick={() => agentPairingMut.mutate()}
                disabled={agentPairingMut.isPending}
                className="px-3 py-1.5 text-xs rounded border border-border-default text-text-secondary hover:text-text-primary hover:border-border-hover disabled:opacity-50"
              >
                {agentPairingMut.isPending ? 'Generating...' : 'New Pairing Request'}
              </button>

              {agentPairingMut.data && (
                <div className="bg-surface-0 border border-border-default rounded-md p-4 space-y-3">
                  <p className="text-xs text-text-tertiary">
                    Copy this instruction and paste it into your Telegram group for the agent. Expires in 5 minutes.
                  </p>
                  <pre className="text-xs text-text-secondary bg-surface-1 rounded p-3 overflow-x-auto whitespace-pre-wrap break-all">
                    {agentPairingMut.data.instruction}
                  </pre>
                  <button
                    onClick={() => navigator.clipboard.writeText(agentPairingMut.data!.instruction)}
                    className="px-3 py-1 text-xs rounded border border-border-default text-text-secondary hover:text-text-primary hover:border-border-hover"
                  >
                    Copy to Clipboard
                  </button>
                </div>
              )}
            </div>
          )}
        </div>
      </div>
    </section>
  )
}

// ── Password change ────────────────────────────────────────────────────────────

function PasswordSection() {
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [confirm, setConfirm] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [success, setSuccess] = useState(false)

  const changeMut = useMutation({
    mutationFn: () => api.auth.updateMe(current, next),
    onSuccess: () => {
      setCurrent('')
      setNext('')
      setConfirm('')
      setError(null)
      setSuccess(true)
      setTimeout(() => setSuccess(false), 3000)
    },
    onError: (err: Error) => setError(err instanceof APIError ? err.message : 'Failed to change password'),
  })

  function handleSubmit() {
    if (next !== confirm) { setError('New passwords do not match'); return }
    if (next.length < 8) { setError('Password must be at least 8 characters'); return }
    setError(null)
    changeMut.mutate()
  }

  return (
    <section className="space-y-4">
      <h2 className="text-lg font-semibold text-text-primary">Change Password</h2>
      {error && <div className="text-sm text-danger">{error}</div>}
      {success && <div className="text-sm text-success">Password updated successfully.</div>}
      <div className="bg-surface-1 border border-border-default rounded-md p-5 space-y-3 max-w-lg">
        <div>
          <label className="text-xs font-medium text-text-tertiary">Current password</label>
          <input
            type="password"
            value={current}
            onChange={e => setCurrent(e.target.value)}
            className="mt-1 block w-full text-sm rounded border border-border-default bg-surface-0 text-text-primary px-3 py-1.5 focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand"
          />
        </div>
        <div>
          <label className="text-xs font-medium text-text-tertiary">New password</label>
          <input
            type="password"
            value={next}
            onChange={e => setNext(e.target.value)}
            className="mt-1 block w-full text-sm rounded border border-border-default bg-surface-0 text-text-primary px-3 py-1.5 focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand"
          />
        </div>
        <div>
          <label className="text-xs font-medium text-text-tertiary">Confirm new password</label>
          <input
            type="password"
            value={confirm}
            onChange={e => setConfirm(e.target.value)}
            className="mt-1 block w-full text-sm rounded border border-border-default bg-surface-0 text-text-primary px-3 py-1.5 focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand"
          />
        </div>
        <button
          onClick={handleSubmit}
          disabled={changeMut.isPending || !current || !next}
          className="px-4 py-1.5 text-sm rounded bg-brand text-surface-0 hover:bg-brand-strong disabled:opacity-50"
        >
          {changeMut.isPending ? 'Updating…' : 'Update Password'}
        </button>
      </div>
    </section>
  )
}

// ── Danger zone ────────────────────────────────────────────────────────────────

function DangerZone() {
  const { logout } = useAuth()
  const navigate = useNavigate()
  const [open, setOpen] = useState(false)
  const [password, setPassword] = useState('')
  const [error, setError] = useState<string | null>(null)

  const deleteMut = useMutation({
    mutationFn: () => api.auth.deleteMe(password),
    onSuccess: async () => {
      await logout()
      navigate('/login')
    },
    onError: (err: Error) => setError(err instanceof APIError ? err.message : 'Failed to delete account'),
  })

  return (
    <section className="space-y-4">
      <h2 className="text-lg font-semibold text-danger">Danger Zone</h2>
      <div className="border border-danger/30 rounded-md p-5 space-y-3 max-w-lg">
        <div>
          <p className="text-sm font-medium text-text-primary">Delete Account</p>
          <p className="text-xs text-text-tertiary mt-0.5">
            Permanently delete your account and all data. This cannot be undone.
          </p>
        </div>
        {!open ? (
          <button
            onClick={() => setOpen(true)}
            className="text-sm px-3 py-1.5 rounded bg-danger/10 text-danger border border-danger/20 hover:bg-danger/20"
          >
            Delete my account
          </button>
        ) : (
          <div className="space-y-3">
            <p className="text-xs text-danger">Enter your password to confirm deletion:</p>
            {error && <div className="text-xs text-danger">{error}</div>}
            <input
              type="password"
              value={password}
              onChange={e => setPassword(e.target.value)}
              placeholder="Your password"
              className="block w-full text-sm rounded border border-danger/30 bg-surface-0 text-text-primary px-3 py-1.5 focus:outline-none focus:ring-1 focus:ring-danger/30 placeholder:text-text-tertiary"
            />
            <div className="flex gap-2">
              <button
                onClick={() => deleteMut.mutate()}
                disabled={deleteMut.isPending || !password}
                className="text-sm px-3 py-1.5 rounded bg-danger text-surface-0 hover:bg-red-500 disabled:opacity-50"
              >
                {deleteMut.isPending ? 'Deleting…' : 'Confirm Delete'}
              </button>
              <button
                onClick={() => { setOpen(false); setPassword(''); setError(null) }}
                className="text-sm px-3 py-1.5 rounded border border-border-strong text-text-primary hover:bg-surface-2"
              >
                Cancel
              </button>
            </div>
          </div>
        )}
      </div>
    </section>
  )
}

// ── Device pairing (QR code for iOS app) ─────────────────────────────────────

function DevicePairing() {
  const qc = useQueryClient()
  const [pairingState, setPairingState] = useState<{
    url: string
    code: string
    expiresAt: string
    existingIds: Set<string>
  } | null>(null)
  const [pairError, setPairError] = useState<string | null>(null)
  const timeoutRef = useRef<ReturnType<typeof setTimeout> | null>(null)

  const { data: pairInfo } = useQuery({
    queryKey: ['pair-info'],
    queryFn: () => api.devices.pairInfo(),
    retry: false,
  })

  const { data: devices } = useQuery({
    queryKey: ['devices'],
    queryFn: () => api.devices.list(),
  })

  // When devices changes while a pairing session is active, check for new device
  useEffect(() => {
    if (!pairingState || !devices) return
    const newDevice = devices.find(d => !pairingState.existingIds.has(d.id))
    if (newDevice) {
      setPairingState(null)
      clearPairingTimeout()
    }
  }, [devices, pairingState])

  // Clean up timeout on unmount
  useEffect(() => () => clearPairingTimeout(), [])

  function clearPairingTimeout() {
    if (timeoutRef.current) {
      clearTimeout(timeoutRef.current)
      timeoutRef.current = null
    }
  }

  const startMut = useMutation({
    mutationFn: () => api.devices.startPairing(),
    onSuccess: (session) => {
      if (!pairInfo) return
      const url = `https://clawvisor.com/clip/pair?d=${pairInfo.daemon_id}&t=${session.pairing_token}&r=${pairInfo.relay_host}`
      const existingIds = new Set((devices ?? []).map(d => d.id))
      setPairingState({ url, code: session.code, expiresAt: session.expires_at, existingIds })
      setPairError(null)
      clearPairingTimeout()
      timeoutRef.current = setTimeout(() => {
        setPairingState(null)
        setPairError('Pairing timed out — try again.')
      }, 5 * 60 * 1000)
    },
    onError: (err: Error) => setPairError(err.message),
  })

  const deleteMut = useMutation({
    mutationFn: (id: string) => api.devices.delete(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['devices'] }),
  })

  function cancelPairing() {
    setPairingState(null)
    clearPairingTimeout()
  }

  const formatCode = (code: string) =>
    code.length === 6 ? `${code.slice(0, 3)}-${code.slice(3)}` : code

  if (!pairInfo) return null

  return (
    <section className="space-y-4">
      <div>
        <h2 className="text-lg font-semibold text-text-primary">Mobile Device</h2>
        <p className="text-sm text-text-tertiary mt-0.5">
          Pair a mobile device to receive push notifications and approve requests from the Clawvisor iOS app.
        </p>
      </div>

      {/* Existing devices */}
      {(devices ?? []).length > 0 && (
        <div className="space-y-2">
          {devices!.map(device => (
            <div key={device.id} className="bg-surface-1 border border-border-default rounded-md px-5 py-4 flex items-center justify-between max-w-lg">
              <div>
                <span className="font-medium text-text-primary">{device.device_name}</span>
                <p className="text-xs text-text-tertiary mt-0.5">
                  Paired {formatDistanceToNow(new Date(device.paired_at), { addSuffix: true })}
                  {device.last_seen_at && ` · Last seen ${formatDistanceToNow(new Date(device.last_seen_at), { addSuffix: true })}`}
                </p>
              </div>
              <button
                onClick={() => {
                  if (confirm(`Unpair "${device.device_name}"? Push notifications will stop working.`)) {
                    deleteMut.mutate(device.id)
                  }
                }}
                disabled={deleteMut.isPending}
                className="text-xs px-3 py-1.5 rounded bg-danger/10 text-danger border border-danger/20 hover:bg-danger/20"
              >
                Unpair
              </button>
            </div>
          ))}
        </div>
      )}

      {/* Active pairing session */}
      {pairingState ? (
        <div className="bg-surface-1 border border-border-default rounded-md p-6 space-y-4 max-w-lg">
          <div className="flex items-center justify-between">
            <h3 className="text-sm font-semibold text-text-secondary">Scan with your phone's camera</h3>
            <CountdownTimer expiresAt={pairingState.expiresAt} />
          </div>
          <div className="flex items-center gap-6">
            <div className="bg-white p-3 rounded-lg shrink-0">
              <QRCodeSVG value={pairingState.url} size={180} level="L" />
            </div>
            <div className="space-y-3">
              <div>
                <p className="text-xs text-text-tertiary">Pairing code</p>
                <p className="text-2xl font-mono font-bold text-text-primary tracking-widest">
                  {formatCode(pairingState.code)}
                </p>
                <p className="text-xs text-text-tertiary mt-1">Enter this code on your phone if prompted.</p>
              </div>
              <div className="flex items-center gap-2 text-xs text-text-tertiary">
                <svg className="w-3.5 h-3.5 animate-spin" fill="none" viewBox="0 0 24 24">
                  <circle className="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" strokeWidth="4" />
                  <path className="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4z" />
                </svg>
                Waiting for pairing to complete...
              </div>
            </div>
          </div>
          <button
            onClick={cancelPairing}
            className="text-xs text-text-tertiary hover:text-text-secondary"
          >
            Cancel
          </button>
        </div>
      ) : (
        <div>
          {pairError && <div className="text-sm text-danger mb-2">{pairError}</div>}
          <button
            onClick={() => startMut.mutate()}
            disabled={startMut.isPending}
            className="px-4 py-1.5 text-sm rounded bg-brand text-surface-0 hover:bg-brand-strong disabled:opacity-50"
          >
            {startMut.isPending ? 'Starting…' : 'Pair Device'}
          </button>
        </div>
      )}
    </section>
  )
}
