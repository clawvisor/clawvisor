import { useState, useEffect, useRef, useCallback } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import type { LocalDaemon, NotificationConfig, PendingGroup, TelegramGroup } from '../api/client'
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
    <div className="p-4 sm:p-8 space-y-10">
      <h1 className="text-2xl font-bold text-text-primary">Settings</h1>
      <DaemonInfo />
      {!features?.multi_tenant && <LLMSection />}
      {!features?.multi_tenant && <OAuthCredentialsSection />}
      {features?.mobile_pairing && <DevicePairing />}
      {features?.local_daemon && <LocalDaemonPairing />}
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

// ── OAuth Credentials ────────────────────────────────────────────────────────

function OAuthCredentialsSection() {
  const qc = useQueryClient()
  const [editingService, setEditingService] = useState<string | null>(null)
  const [clientIdValue, setClientIdValue] = useState('')
  const [clientSecretValue, setClientSecretValue] = useState('')
  const [error, setError] = useState<string | null>(null)

  const { data: googleOAuth } = useQuery({
    queryKey: ['google-oauth'],
    queryFn: () => api.system.getGoogleOAuth(),
  })

  const { data: pkceCredentials } = useQuery({
    queryKey: ['pkce-credentials'],
    queryFn: () => api.system.listPKCECredentials(),
  })

  const { data: services } = useQuery({
    queryKey: ['services'],
    queryFn: () => api.services.list(),
  })

  const googleSaveMut = useMutation({
    mutationFn: () => api.system.setGoogleOAuth(clientIdValue, clientSecretValue),
    onSuccess: () => {
      setEditingService(null)
      setClientIdValue('')
      setClientSecretValue('')
      setError(null)
      qc.invalidateQueries({ queryKey: ['google-oauth'] })
      qc.invalidateQueries({ queryKey: ['services'] })
    },
    onError: (err: Error) => setError(err.message),
  })

  const pkceSaveMut = useMutation({
    mutationFn: ({ serviceId, clientId }: { serviceId: string; clientId: string }) =>
      api.system.setPKCECredential(serviceId, clientId),
    onSuccess: () => {
      setEditingService(null)
      setClientIdValue('')
      setError(null)
      qc.invalidateQueries({ queryKey: ['pkce-credentials'] })
      qc.invalidateQueries({ queryKey: ['services'] })
    },
    onError: (err: Error) => setError(err.message),
  })

  const pkceDeleteMut = useMutation({
    mutationFn: (serviceId: string) => api.system.deletePKCECredential(serviceId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['pkce-credentials'] })
      qc.invalidateQueries({ queryKey: ['services'] })
    },
    onError: (err: Error) => setError(err.message),
  })

  const serviceList = services?.services

  // Check if any Google services exist
  const hasGoogle = serviceList?.some(s => s.id.startsWith('google.'))

  // Find services that have PKCE flow (deduplicate by base ID)
  const pkceServices = new Map<string, string>()
  if (serviceList) {
    for (const svc of serviceList) {
      if (svc.pkce_flow && !pkceServices.has(svc.id)) {
        pkceServices.set(svc.id, svc.name)
      }
    }
  }

  // Build PKCE credential lookup
  const pkceCredMap = new Map<string, string>()
  if (pkceCredentials) {
    for (const c of pkceCredentials) {
      pkceCredMap.set(c.service_id, c.client_id)
    }
  }

  // Nothing to show if no OAuth services exist
  if (!hasGoogle && pkceServices.size === 0) return null

  function startEditing(serviceId: string, currentClientId?: string) {
    setEditingService(serviceId)
    setClientIdValue(currentClientId ?? '')
    setClientSecretValue('')
    setError(null)
  }

  const inputClass = "w-full text-sm rounded border border-border-default bg-surface-0 text-text-primary px-3 py-1.5 focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand placeholder:text-text-tertiary"

  return (
    <section className="space-y-4">
      <div>
        <h2 className="text-lg font-semibold text-text-primary">OAuth Credentials</h2>
        <p className="text-sm text-text-tertiary mt-0.5">
          Configure OAuth app credentials for services that require browser-based authorization.
        </p>
      </div>

      {error && <div className="text-sm text-danger max-w-lg">{error}</div>}

      <div className="space-y-2 max-w-lg">
        {/* Google OAuth (client ID + secret, shared across all google.* services) */}
        {hasGoogle && (
          <div className="bg-surface-1 border border-border-default rounded-md p-4">
            <div className="flex items-center justify-between">
              <div>
                <span className="text-sm font-medium text-text-primary">Google</span>
                <span className="text-xs text-text-tertiary ml-2">Gmail, Calendar, Drive, Contacts</span>
              </div>
              {editingService !== '__google__' && (
                <div className="flex items-center gap-2">
                  {googleOAuth?.configured ? (
                    <>
                      <span className="text-xs text-success">Configured</span>
                      <button
                        onClick={() => startEditing('__google__')}
                        className="text-xs text-text-tertiary hover:text-text-primary"
                      >
                        Edit
                      </button>
                    </>
                  ) : (
                    <button
                      onClick={() => startEditing('__google__')}
                      className="text-xs px-2.5 py-1 rounded border border-brand/30 text-brand hover:bg-brand/10"
                    >
                      Configure
                    </button>
                  )}
                </div>
              )}
            </div>

            {editingService === '__google__' && (
              <div className="mt-3 space-y-2">
                <div>
                  <label className="text-xs font-medium text-text-tertiary">Client ID</label>
                  <input
                    type="text"
                    value={clientIdValue}
                    onChange={e => { setClientIdValue(e.target.value); setError(null) }}
                    placeholder="123456789.apps.googleusercontent.com"
                    className={`mt-1 ${inputClass}`}
                    autoFocus
                  />
                </div>
                <div>
                  <label className="text-xs font-medium text-text-tertiary">Client Secret</label>
                  <input
                    type="password"
                    value={clientSecretValue}
                    onChange={e => { setClientSecretValue(e.target.value); setError(null) }}
                    placeholder="GOCSPX-..."
                    className={`mt-1 ${inputClass}`}
                  />
                </div>
                <div className="flex items-center gap-2 pt-1">
                  <button
                    onClick={() => {
                      if (!clientIdValue || !clientSecretValue) { setError('Both fields are required'); return }
                      setError(null)
                      googleSaveMut.mutate()
                    }}
                    disabled={googleSaveMut.isPending || !clientIdValue || !clientSecretValue}
                    className="px-3 py-1 text-xs rounded bg-brand text-surface-0 hover:bg-brand-strong disabled:opacity-50"
                  >
                    {googleSaveMut.isPending ? 'Saving…' : 'Save'}
                  </button>
                  <button
                    onClick={() => { setEditingService(null); setError(null) }}
                    className="text-xs text-text-tertiary hover:text-text-primary"
                  >
                    Cancel
                  </button>
                </div>
              </div>
            )}
          </div>
        )}

        {/* PKCE services (client ID only) */}
        {Array.from(pkceServices.entries()).map(([serviceId, serviceName]) => {
          const configured = pkceCredMap.get(serviceId)
          const isEditing = editingService === serviceId

          return (
            <div
              key={serviceId}
              className="bg-surface-1 border border-border-default rounded-md p-4"
            >
              <div className="flex items-center justify-between">
                <div>
                  <span className="text-sm font-medium text-text-primary">{serviceName}</span>
                  <span className="text-xs text-text-tertiary ml-2">{serviceId}</span>
                </div>
                {!isEditing && (
                  <div className="flex items-center gap-2">
                    {configured ? (
                      <>
                        <span className="text-xs text-success">Configured</span>
                        <button
                          onClick={() => startEditing(serviceId, configured)}
                          className="text-xs text-text-tertiary hover:text-text-primary"
                        >
                          Edit
                        </button>
                        <button
                          onClick={() => pkceDeleteMut.mutate(serviceId)}
                          disabled={pkceDeleteMut.isPending}
                          className="text-xs text-danger/70 hover:text-danger"
                        >
                          Remove
                        </button>
                      </>
                    ) : (
                      <button
                        onClick={() => startEditing(serviceId)}
                        className="text-xs px-2.5 py-1 rounded border border-brand/30 text-brand hover:bg-brand/10"
                      >
                        Configure
                      </button>
                    )}
                  </div>
                )}
              </div>

              {isEditing && (
                <div className="mt-3 space-y-2">
                  <input
                    type="text"
                    value={clientIdValue}
                    onChange={e => { setClientIdValue(e.target.value); setError(null) }}
                    onKeyDown={e => {
                      if (e.key === 'Enter' && clientIdValue.trim()) {
                        pkceSaveMut.mutate({ serviceId, clientId: clientIdValue.trim() })
                      }
                    }}
                    placeholder="OAuth Client ID"
                    className={`${inputClass} font-mono`}
                    autoFocus
                  />
                  <div className="flex items-center gap-2">
                    <button
                      onClick={() => pkceSaveMut.mutate({ serviceId, clientId: clientIdValue.trim() })}
                      disabled={pkceSaveMut.isPending || !clientIdValue.trim()}
                      className="px-3 py-1 text-xs rounded bg-brand text-surface-0 hover:bg-brand-strong disabled:opacity-50"
                    >
                      {pkceSaveMut.isPending ? 'Saving…' : 'Save'}
                    </button>
                    <button
                      onClick={() => { setEditingService(null); setError(null) }}
                      className="text-xs text-text-tertiary hover:text-text-primary"
                    >
                      Cancel
                    </button>
                  </div>
                </div>
              )}
            </div>
          )
        })}
      </div>
    </section>
  )
}

// ── Telegram Setup (progressive stepper) ────────────────────────────────────

function TelegramSetupSection() {
  const qc = useQueryClient()

  // ── Shared state ─────────────────────────────────────────
  const [error, setError] = useState<string | null>(null)
  const [botExpanded, setBotExpanded] = useState(false)

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

  const { data: activeGroups } = useQuery({
    queryKey: ['active-groups'],
    queryFn: () => api.notifications.listActiveGroups(),
    enabled: hasBotToken,
  })

  const { data: pendingGroups } = useQuery({
    queryKey: ['telegram-groups'],
    queryFn: () => api.notifications.listTelegramGroups(),
    enabled: hasBotToken,
  })

  // Auto-expand bot section if not configured
  useEffect(() => {
    if (!hasBotToken) setBotExpanded(true)
  }, [hasBotToken])

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

  // ── Bot Mutations ────────────────────────────────────────
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
        } catch (e) { console.warn('Settings: pairing status poll failed', e) }
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
      qc.invalidateQueries({ queryKey: ['active-groups'] })
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

  // ── Group Mutations ──────────────────────────────────────
  const detectMut = useMutation({
    mutationFn: () => api.notifications.detectTelegramGroups(),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ['telegram-groups'] }) },
  })

  const enableGroupMut = useMutation({
    mutationFn: (g: PendingGroup) => api.notifications.upsertTelegramGroup(g.chat_id, g.title),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['active-groups'] })
      qc.invalidateQueries({ queryKey: ['telegram-groups'] })
    },
  })

  const disableGroupMut = useMutation({
    mutationFn: (groupChatId: string) => api.notifications.deleteTelegramGroup(groupChatId),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ['active-groups'] }) },
  })

  const dismissMut = useMutation({
    mutationFn: (chatId: string) => api.notifications.dismissTelegramGroup(chatId),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ['telegram-groups'] }) },
  })

  // Manual group add
  const [manualChatId, setManualChatId] = useState('')
  const [showManualAdd, setShowManualAdd] = useState(false)
  const manualAddMut = useMutation({
    mutationFn: (groupChatId: string) => api.notifications.addGroupManually(groupChatId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['active-groups'] })
      setManualChatId('')
      setShowManualAdd(false)
    },
  })

  return (
    <section className="space-y-4">
      <div>
        <h2 className="text-lg font-semibold text-text-primary">Telegram</h2>
        <p className="text-sm text-text-tertiary mt-0.5">
          Receive notifications, approve tasks inline, and enable auto-approval via group chat context.
        </p>
      </div>

      {error && <div className="text-sm text-danger max-w-xl">{error}</div>}

      <div className="max-w-xl space-y-3">
        {/* ── Bot Connection ────────────────────────────────── */}
        <div className="bg-surface-1 border border-border-default rounded-md px-5 py-4 space-y-3">
          <button
            onClick={() => setBotExpanded(prev => !prev)}
            className="flex items-center gap-3 w-full text-left group"
          >
            <span className={`flex-shrink-0 w-7 h-7 rounded-full flex items-center justify-center text-xs font-bold border-2 transition-colors ${
              hasBotToken
                ? 'bg-green-500/15 border-green-500/40 text-green-500'
                : 'bg-brand/15 border-brand/40 text-brand'
            }`}>
              {hasBotToken ? (
                <svg className="w-3.5 h-3.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="3" strokeLinecap="round" strokeLinejoin="round">
                  <path d="M20 6L9 17l-5-5" />
                </svg>
              ) : '1'}
            </span>
            <span className="text-sm font-medium text-text-primary">Connect your Telegram bot</span>
          </button>

          {botExpanded && (
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
                        <p className="text-text-tertiary mt-1">This must be a <span className="text-text-secondary font-medium">separate bot</span> from the one your agent uses to chat with you. Clawvisor needs its own bot to send approval requests.</p>
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

        {/* ── Group Management ──────────────────────────────── */}
        {hasBotToken && (
          <div className="space-y-3">
            <div className="flex items-center justify-between">
              <h3 className="text-sm font-semibold text-text-primary">Groups</h3>
              <div className="flex items-center gap-2">
                <button
                  onClick={() => setShowManualAdd(prev => !prev)}
                  className="flex items-center gap-1.5 px-3 py-1 text-xs rounded border border-border-default text-text-tertiary hover:text-text-primary hover:border-border-hover"
                >
                  <svg className="h-3 w-3" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2">
                    <line x1="12" y1="5" x2="12" y2="19" /><line x1="5" y1="12" x2="19" y2="12" />
                  </svg>
                  Add by ID
                </button>
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
            </div>

            {/* Manual group add form */}
            {showManualAdd && (
              <div className="bg-surface-0 border border-border-default rounded-md p-4 space-y-2">
                <p className="text-xs text-text-secondary">
                  Enter the group chat ID to connect a group your bot is already in.
                </p>
                <form
                  className="flex items-center gap-2"
                  onSubmit={(e) => { e.preventDefault(); if (manualChatId.trim()) manualAddMut.mutate(manualChatId.trim()) }}
                >
                  <input
                    type="text"
                    value={manualChatId}
                    onChange={e => setManualChatId(e.target.value)}
                    placeholder="e.g. -1001234567890"
                    className="flex-1 px-3 py-1.5 text-xs rounded border border-border-default bg-surface-1 text-text-primary placeholder:text-text-tertiary focus:outline-none focus:ring-1 focus:ring-brand"
                  />
                  <button
                    type="submit"
                    disabled={!manualChatId.trim() || manualAddMut.isPending}
                    className="px-3 py-1.5 text-xs rounded bg-brand text-surface-0 hover:bg-brand-strong disabled:opacity-50"
                  >
                    {manualAddMut.isPending ? 'Validating...' : 'Connect'}
                  </button>
                </form>
                {manualAddMut.isError && (
                  <p className="text-xs text-danger">{(manualAddMut.error as Error).message || 'Bot is not a member of this group'}</p>
                )}
              </div>
            )}

            {/* Pending groups (detected but not connected) */}
            {pendingGroups && pendingGroups.length > 0 && (
              <div className="bg-surface-0 border border-border-default rounded-md divide-y divide-border-default">
                {pendingGroups.map((g: PendingGroup) => (
                  <div key={g.chat_id} className="flex items-center justify-between px-4 py-3">
                    <div className="text-sm">
                      <span className="text-text-primary font-medium">{g.title || g.chat_id}</span>
                      <span className="text-text-tertiary ml-2 text-xs">({g.type})</span>
                    </div>
                    <div className="flex items-center gap-2">
                      <button
                        onClick={() => enableGroupMut.mutate(g)}
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
            )}

            {/* Active groups */}
            {activeGroups && activeGroups.length > 0 ? (
              <div className="space-y-2">
                {activeGroups.map((g: TelegramGroup) => (
                  <TelegramGroupCard key={g.group_chat_id} group={g} onDisconnect={(id) => disableGroupMut.mutate(id)} />
                ))}
              </div>
            ) : (
              <div className="bg-surface-1 border border-border-default rounded-md p-4">
                <p className="text-xs text-text-tertiary">
                  No groups connected yet. Add your bot to a Telegram group, then click Scan to detect it.
                </p>
                <div className="bg-surface-2 border border-border-default rounded-md p-3 text-xs text-text-secondary space-y-2 mt-3">
                  <p className="font-medium text-text-primary">Setup instructions:</p>
                  <ol className="list-decimal list-inside space-y-1.5">
                    <li>Create a new Telegram group and add your Clawvisor bot</li>
                    <li>
                      <strong>Disable bot privacy mode:</strong> message{' '}
                      <a href="https://t.me/BotFather" target="_blank" rel="noreferrer" className="text-brand hover:underline">@BotFather</a>,
                      send <code className="bg-surface-1 px-1 rounded">/mybots</code>, select your bot, go to{' '}
                      <em>Bot Settings &rarr; Group Privacy &rarr; Turn off</em>
                    </li>
                    <li>Add your agent bot to the same group</li>
                    <li>Click <strong>Scan for Groups</strong> above to detect the group</li>
                  </ol>
                </div>
              </div>
            )}
          </div>
        )}
      </div>
    </section>
  )
}

// ── Per-group card with auto-approval and agent pairing ───────────────────────

function TelegramGroupCard({ group, onDisconnect }: { group: TelegramGroup; onDisconnect: (id: string) => void }) {
  const qc = useQueryClient()
  const [expanded, setExpanded] = useState(false)

  const { data: pairedAgents } = useQuery({
    queryKey: ['paired-agents', group.group_chat_id],
    queryFn: () => api.notifications.listPairedAgents(group.group_chat_id),
    refetchInterval: expanded ? 10000 : false,
  })

  const autoApprovalMut = useMutation({
    mutationFn: (vars: { enabled: boolean; notify?: boolean }) =>
      api.notifications.setAutoApproval(group.group_chat_id, vars.enabled, vars.notify),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ['active-groups'] }) },
  })

  const agentPairingMut = useMutation({
    mutationFn: () => api.notifications.createGroupPairing(group.group_chat_id),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ['paired-agents', group.group_chat_id] }) },
  })

  return (
    <div className="bg-surface-1 border border-border-default rounded-md px-5 py-4 space-y-3">
      <button onClick={() => setExpanded(prev => !prev)} className="flex items-center justify-between w-full text-left cursor-pointer">
        <div className="flex items-center gap-2 flex-1 min-w-0">
          <span className={`flex-shrink-0 w-2 h-2 rounded-full ${group.auto_approval_enabled && pairedAgents && pairedAgents.length > 0 ? 'bg-green-500' : 'bg-surface-2 border border-border-default'}`} />
          <span className="text-sm font-medium text-text-primary truncate">{group.title || group.group_chat_id}</span>
          {group.title && <span className="text-xs text-text-tertiary font-mono">{group.group_chat_id}</span>}
        </div>
        <div className="flex items-center gap-3">
          {pairedAgents && pairedAgents.length === 0 && (
            <span className="text-xs text-yellow-500">No agents paired</span>
          )}
          {group.auto_approval_enabled && pairedAgents && pairedAgents.length > 0 && (
            <span className="text-xs text-green-500">Auto-approval on</span>
          )}
          <svg className={`w-4 h-4 text-text-tertiary transition-transform ${expanded ? 'rotate-180' : ''}`} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2">
            <path d="M6 9l6 6 6-6" />
          </svg>
        </div>
      </button>

      {expanded && (
        <div className="space-y-4 pt-2 border-t border-border-default">
          {/* Auto-approval */}
          <div className="space-y-2">
            <p className="text-xs text-text-secondary leading-relaxed">
              When enabled, Clawvisor reads this group chat to detect when you&apos;ve approved a task in conversation.
            </p>
            <label className="flex items-center gap-3 cursor-pointer">
              <button
                onClick={() => autoApprovalMut.mutate({ enabled: !group.auto_approval_enabled })}
                disabled={autoApprovalMut.isPending}
                className={`relative inline-flex h-5 w-9 flex-shrink-0 rounded-full border-2 border-transparent transition-colors ${
                  group.auto_approval_enabled ? 'bg-green-500' : 'bg-surface-2'
                }`}
              >
                <span className={`pointer-events-none inline-block h-4 w-4 rounded-full bg-white shadow transform transition-transform ${
                  group.auto_approval_enabled ? 'translate-x-4' : 'translate-x-0'
                }`} />
              </button>
              <span className="text-sm text-text-secondary">Auto-approval</span>
            </label>
            {group.auto_approval_enabled && (
              <label className="flex items-center gap-3 cursor-pointer">
                <button
                  onClick={() => autoApprovalMut.mutate({ enabled: true, notify: !group.auto_approval_notify })}
                  disabled={autoApprovalMut.isPending}
                  className={`relative inline-flex h-5 w-9 flex-shrink-0 rounded-full border-2 border-transparent transition-colors ${
                    group.auto_approval_notify ? 'bg-green-500' : 'bg-surface-2'
                  }`}
                >
                  <span className={`pointer-events-none inline-block h-4 w-4 rounded-full bg-white shadow transform transition-transform ${
                    group.auto_approval_notify ? 'translate-x-4' : 'translate-x-0'
                  }`} />
                </button>
                <span className="text-sm text-text-secondary">Notify on auto-approval</span>
              </label>
            )}
          </div>

          {/* No agents warning */}
          {(!pairedAgents || pairedAgents.length === 0) && (
            <div className="flex items-start gap-2 px-3 py-2.5 rounded-md bg-yellow-500/10 border border-yellow-500/20">
              <svg className="w-4 h-4 text-yellow-500 flex-shrink-0 mt-0.5" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2">
                <path d="M10.29 3.86L1.82 18a2 2 0 001.71 3h16.94a2 2 0 001.71-3L13.71 3.86a2 2 0 00-3.42 0z" />
                <line x1="12" y1="9" x2="12" y2="13" /><line x1="12" y1="17" x2="12.01" y2="17" />
              </svg>
              <p className="text-xs text-yellow-600 dark:text-yellow-400">
                No agents paired to this group. Pair an agent below for auto-approval to take effect.
              </p>
            </div>
          )}

          {/* Paired agents */}
          <div className="space-y-2">
            <p className="text-xs font-medium text-text-tertiary">Paired Agents</p>
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

          {/* Disconnect */}
          <button
            onClick={() => onDisconnect(group.group_chat_id)}
            className="text-xs text-danger hover:text-red-400"
          >
            Disconnect Group
          </button>
        </div>
      )}
    </div>
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

// ── Local daemon pairing ──────────────────────────────────────────────────────

function DaemonCard({ daemon, onDelete, deleting, enabledServiceIds }: {
  daemon: LocalDaemon & { connected: boolean }
  onDelete: () => void
  deleting: boolean
  enabledServiceIds: Set<string>
}) {
  const qc = useQueryClient()
  const [editing, setEditing] = useState(false)
  const [newName, setNewName] = useState(daemon.name || '')
  const [showMenu, setShowMenu] = useState(false)
  const navigate = useNavigate()

  const { data: caps } = useQuery({
    queryKey: ['daemon-services', daemon.id],
    queryFn: () => api.localDaemon.services(daemon.id),
    enabled: daemon.connected,
    refetchInterval: 30000,
  })

  const renameMut = useMutation({
    mutationFn: (name: string) => api.localDaemon.rename(daemon.id, name),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['local-daemons'] })
      setEditing(false)
    },
  })

  const services = caps?.services ?? []

  return (
    <div className="bg-surface-1 border border-border-default rounded-lg px-5 py-4 max-w-xl">
      {/* Device header */}
      <div className="flex items-start gap-3.5">
        {/* Laptop icon */}
        <div className="shrink-0 mt-0.5 text-text-secondary">
          <svg width="32" height="32" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round">
            <rect x="3" y="4" width="18" height="12" rx="2" />
            <path d="M2 20h20" />
            <path d="M7 16v4" />
            <path d="M17 16v4" />
          </svg>
        </div>

        <div className="flex-1 min-w-0">
          <div className="flex items-center justify-between">
            <div className="flex items-center gap-2 min-w-0">
              {editing ? (
                <form onSubmit={e => { e.preventDefault(); if (newName.trim()) renameMut.mutate(newName.trim()) }} className="flex items-center gap-1.5">
                  <input
                    autoFocus
                    value={newName}
                    onChange={e => setNewName(e.target.value)}
                    className="text-sm font-medium px-2 py-0.5 rounded border border-border-default bg-surface-0 text-text-primary w-40"
                  />
                  <button type="submit" disabled={renameMut.isPending} className="text-xs text-brand hover:underline">Save</button>
                  <button type="button" onClick={() => { setEditing(false); setNewName(daemon.name || '') }} className="text-xs text-text-tertiary hover:underline">Cancel</button>
                </form>
              ) : (
                <>
                  <span className="font-medium text-text-primary truncate">{daemon.name || 'Local Computer'}</span>
                  <button onClick={() => setEditing(true)} className="shrink-0 text-text-tertiary hover:text-text-primary" title="Rename">
                    <svg width="12" height="12" viewBox="0 0 12 12" fill="none"><path d="M8.5 1.5l2 2-7 7H1.5V8.5l7-7z" stroke="currentColor" strokeWidth="1.2" strokeLinecap="round" strokeLinejoin="round"/></svg>
                  </button>
                </>
              )}
              <span className={`shrink-0 inline-block w-2 h-2 rounded-full ${daemon.connected ? 'bg-success animate-pulse' : 'bg-text-tertiary'}`} />
              <span className="shrink-0 text-xs text-text-tertiary">{daemon.connected ? 'Connected' : 'Disconnected'}</span>
            </div>

            {/* Overflow menu */}
            <div className="relative shrink-0 ml-2">
              <button
                onClick={() => setShowMenu(!showMenu)}
                className="p-1 rounded hover:bg-surface-2 text-text-tertiary hover:text-text-primary"
              >
                <svg width="16" height="16" viewBox="0 0 16 16" fill="currentColor"><circle cx="8" cy="3" r="1.25"/><circle cx="8" cy="8" r="1.25"/><circle cx="8" cy="13" r="1.25"/></svg>
              </button>
              {showMenu && (
                <>
                  <div className="fixed inset-0 z-10" onClick={() => setShowMenu(false)} />
                  <div className="absolute right-0 top-full mt-1 z-20 bg-surface-0 border border-border-default rounded-md shadow-lg py-1 min-w-[140px]">
                    <button
                      onClick={() => { setShowMenu(false); setEditing(true) }}
                      className="w-full text-left px-3 py-1.5 text-sm text-text-primary hover:bg-surface-1"
                    >
                      Rename
                    </button>
                    <button
                      onClick={() => {
                        setShowMenu(false)
                        if (confirm(`Unpair "${daemon.name || 'Local Computer'}"? Local services will stop working.`)) {
                          onDelete()
                        }
                      }}
                      disabled={deleting}
                      className="w-full text-left px-3 py-1.5 text-sm text-danger hover:bg-surface-1"
                    >
                      Unpair
                    </button>
                  </div>
                </>
              )}
            </div>
          </div>

          <p className="text-xs text-text-tertiary mt-0.5">
            Paired {formatDistanceToNow(new Date(daemon.paired_at), { addSuffix: true })}
            {daemon.last_connected_at && ` · Last seen ${formatDistanceToNow(new Date(daemon.last_connected_at), { addSuffix: true })}`}
          </p>
        </div>
      </div>

      {/* Services */}
      {daemon.connected && services.length > 0 && (
        <div className="mt-4 pt-4 border-t border-border-default">
          <div className="flex items-center justify-between mb-3">
            <p className="text-xs font-medium text-text-secondary uppercase tracking-wide">Services</p>
            <button
              onClick={() => navigate('/dashboard/services')}
              className="text-xs text-brand hover:underline"
            >
              Manage
            </button>
          </div>
          <div className="space-y-2.5">
            {services.map(svc => {
              const enabled = enabledServiceIds.has(svc.id)
              return (
                <div key={svc.id} className="flex items-center gap-3">
                  <div className="flex-1 min-w-0">
                    <p className="text-sm text-text-primary leading-tight">{svc.name}</p>
                    {svc.description && <p className="text-xs text-text-tertiary leading-tight mt-0.5">{svc.description}</p>}
                  </div>
                  {enabled ? (
                    <span className="shrink-0 text-[10px] px-1.5 py-0.5 rounded bg-success/15 text-success font-medium">Active</span>
                  ) : (
                    <span className="shrink-0 text-[10px] px-1.5 py-0.5 rounded bg-surface-2 text-text-tertiary font-medium">Inactive</span>
                  )}
                </div>
              )
            })}
          </div>
        </div>
      )}
    </div>
  )
}

const LOCAL_DAEMON_PORT = 25299
const LOCAL_DAEMON_INSTALL_CMD = 'curl -fsSL https://raw.githubusercontent.com/clawvisor/clawvisor/main/scripts/install-local.sh | sh'
const LOCAL_SERVICES_INSTALL_CMD = 'clawvisor-local install clawvisor/local-integrations'
const LOCAL_SERVICES_GUIDE_URL = 'https://github.com/clawvisor/clawvisor/blob/main/docs/LOCAL_ADAPTER_GUIDE.md'

function LocalDaemonPairing() {
  const qc = useQueryClient()
  const [pairing, setPairing] = useState(false)
  const [pairError, setPairError] = useState<string | null>(null)
  const [pairSuccess, setPairSuccess] = useState(false)
  const [waitingForConnection, setWaitingForConnection] = useState(false)

  const { data: daemons, isLoading } = useQuery({
    queryKey: ['local-daemons'],
    queryFn: () => api.localDaemon.list(),
  })

  const { data: enabledServices } = useQuery({
    queryKey: ['enabled-local-services'],
    queryFn: () => api.localDaemon.listEnabledServices(),
  })

  const enabledByDaemon = new Map<string, Set<string>>()
  for (const s of enabledServices ?? []) {
    let set = enabledByDaemon.get(s.daemon_id)
    if (!set) { set = new Set(); enabledByDaemon.set(s.daemon_id, set) }
    set.add(s.service_id)
  }

  // Probe localhost to see if a daemon is running and get its ID.
  // Only probe when there are already paired daemons — avoids unnecessary
  // localhost requests (and CSP/network errors) when no daemon exists.
  const hasDaemons = (daemons ?? []).length > 0
  const { data: localDaemonId } = useQuery({
    queryKey: ['local-daemon-probe'],
    queryFn: async () => {
      try {
        const resp = await fetch(`http://localhost:${LOCAL_DAEMON_PORT}/api/status`, { signal: AbortSignal.timeout(2000) })
        if (!resp.ok) return null
        const data = await resp.json() as { daemon_id?: string }
        return data.daemon_id ?? null
      } catch { return null }
    },
    staleTime: 30000,
    enabled: hasDaemons,
  })

  const localAlreadyPaired = !!(localDaemonId && daemons?.some(d => d.daemon_id === localDaemonId))

  const deleteMut = useMutation({
    mutationFn: (id: string) => api.localDaemon.delete(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['local-daemons'] }),
  })

  async function startPairing() {
    setPairing(true)
    setPairError(null)
    setPairSuccess(false)

    try {
      // Fetch pairing code from the locally-running daemon.
      const localResp = await fetch(`http://localhost:${LOCAL_DAEMON_PORT}/api/pairing/code`, {
        signal: AbortSignal.timeout(3000),
      })
      if (!localResp.ok) {
        throw new Error('Local daemon returned an error')
      }
      const { daemon_id, code, nonce, name: daemonName } = await localResp.json() as {
        daemon_id: string; code: string; nonce: string; name?: string
      }

      if (!daemon_id || !code || !nonce) {
        throw new Error('Invalid response from local daemon')
      }

      // Send to cloud to complete pairing.
      const result = await api.localDaemon.pair(daemon_id, code, nonce, daemonName)

      // Tell the local daemon the connection token so it can connect via WebSocket.
      try {
        await fetch(`http://localhost:${LOCAL_DAEMON_PORT}/api/pairing/complete`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ code, nonce, connection_token: result.connection_token }),
          signal: AbortSignal.timeout(3000),
        })
      } catch {
        // Non-fatal — daemon may connect on its own if it polls
      }

      setPairSuccess(true)
      setWaitingForConnection(true)
      // Poll for the daemon to finish connecting (DB row is created on WS connect).
      let connected = false
      for (let i = 0; i < 10; i++) {
        await new Promise(r => setTimeout(r, 3000))
        const result = await qc.fetchQuery({ queryKey: ['local-daemons'], queryFn: () => api.localDaemon.list() })
        if (result.some(d => d.daemon_id === daemon_id && d.connected)) {
          connected = true
          break
        }
      }
      setWaitingForConnection(false)
      qc.invalidateQueries({ queryKey: ['local-daemons'] })
      if (!connected) {
        setPairError('Paired but daemon did not connect. Check that the daemon is running and can reach this server.')
      }
      setTimeout(() => setPairSuccess(false), 5000)
    } catch (err) {
      if (err instanceof TypeError || (err instanceof DOMException && err.name === 'AbortError')) {
        setPairError('Could not reach local daemon. Make sure it is running on port ' + LOCAL_DAEMON_PORT + '.')
      } else {
        setPairError(err instanceof Error ? err.message : 'Pairing failed')
      }
    } finally {
      setPairing(false)
    }
  }

  const hasPairedDaemons = !isLoading && (daemons ?? []).length > 0
  const [showSetup, setShowSetup] = useState(false)

  const [daemonCopied, setDaemonCopied] = useState(false)
  const [servicesCopied, setServicesCopied] = useState(false)

  const copyButton = (text: string, copied: boolean, setCopied: (v: boolean) => void) => (
    <button
      onClick={() => {
        navigator.clipboard.writeText(text)
        setCopied(true)
        setTimeout(() => setCopied(false), 2000)
      }}
      className="absolute right-2 top-1/2 -translate-y-1/2 px-2 py-1 text-xs rounded border border-border-default text-text-secondary hover:text-text-primary hover:border-border-hover bg-surface-0"
    >
      {copied ? 'Copied!' : 'Copy'}
    </button>
  )

  const installBlock = (
    <div className="max-w-xl rounded-md border border-border-default bg-surface-1 p-4 space-y-3">
      <div className="space-y-1.5">
        <p className="text-sm text-text-secondary">1. Install the local daemon:</p>
        <div className="relative">
          <pre className="overflow-x-auto rounded bg-surface-0 border border-border-default px-3 py-2 pr-20 text-xs sm:text-sm font-mono text-text-primary whitespace-nowrap">
            {LOCAL_DAEMON_INSTALL_CMD}
          </pre>
          {copyButton(LOCAL_DAEMON_INSTALL_CMD, daemonCopied, setDaemonCopied)}
        </div>
      </div>
      <div className="space-y-1.5">
        <p className="text-sm text-text-secondary">2. Install the official services:</p>
        <div className="relative">
          <pre className="overflow-x-auto rounded bg-surface-0 border border-border-default px-3 py-2 pr-20 text-xs sm:text-sm font-mono text-text-primary whitespace-nowrap">
            {LOCAL_SERVICES_INSTALL_CMD}
          </pre>
          {copyButton(LOCAL_SERVICES_INSTALL_CMD, servicesCopied, setServicesCopied)}
        </div>
      </div>
      <p className="text-xs text-text-tertiary">
        This allows your agents to execute registered service commands on your computer. Only enable services you trust.
        {' '}<a href={LOCAL_SERVICES_GUIDE_URL} target="_blank" rel="noopener noreferrer" className="text-brand hover:underline">Learn more about local services &rarr;</a>
      </p>
    </div>
  )

  return (
    <section className="space-y-4">
      <div>
        <h2 className="text-lg font-semibold text-text-primary">Local Computer</h2>
        <p className="text-sm text-text-tertiary mt-0.5">
          Pair a local daemon to use local-only services like iMessage, file access, and browser navigation.
        </p>
      </div>

      {/* Install instructions: prominent when no daemons, collapsible when paired */}
      {hasPairedDaemons ? (
        <div>
          <button
            onClick={() => setShowSetup(v => !v)}
            className="flex items-center gap-1.5 text-sm text-text-secondary hover:text-text-primary"
          >
            <svg className={`w-3 h-3 transition-transform ${showSetup ? 'rotate-90' : ''}`} viewBox="0 0 6 10" fill="currentColor"><path d="M1 1l4 4-4 4" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" fill="none"/></svg>
            Setup instructions
          </button>
          <div className={`overflow-hidden transition-all duration-200 ${showSetup ? 'max-h-[500px] opacity-100 mt-3' : 'max-h-0 opacity-0'}`}>
            {installBlock}
          </div>
        </div>
      ) : (
        installBlock
      )}

      {/* Paired daemons list */}
      {hasPairedDaemons && (
        <div className="space-y-3">
          {daemons!.map(daemon => (
            <DaemonCard key={daemon.id} daemon={daemon} onDelete={() => deleteMut.mutate(daemon.id)} deleting={deleteMut.isPending} enabledServiceIds={enabledByDaemon.get(daemon.id) ?? new Set()} />
          ))}
        </div>
      )}

      {pairError && <div className="text-sm text-danger max-w-xl">{pairError}</div>}
      {waitingForConnection && (
        <div className="text-sm text-text-secondary max-w-xl flex items-center gap-2">
          <span className="inline-block w-3 h-3 border-2 border-brand border-t-transparent rounded-full animate-spin" />
          Waiting for daemon to connect…
        </div>
      )}
      {pairSuccess && !waitingForConnection && <div className="text-sm text-success max-w-xl">Local computer paired successfully.</div>}

      {!isLoading && !localAlreadyPaired && (
        <button
          onClick={startPairing}
          disabled={pairing}
          className="px-4 py-1.5 text-sm rounded bg-brand text-surface-0 hover:bg-brand-strong disabled:opacity-50"
        >
          {pairing ? 'Pairing…' : hasPairedDaemons ? 'Pair Another Computer' : 'Pair with Local Computer'}
        </button>
      )}
    </section>
  )
}
