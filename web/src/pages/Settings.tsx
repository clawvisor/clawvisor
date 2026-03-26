import { useState, useEffect, useRef, useCallback } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import type { NotificationConfig } from '../api/client'
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
      <LLMSection />
      <DevicePairing />
      <TelegramSection />
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
    refetchInterval: 60_000,
    staleTime: 30_000,
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

// ── Telegram notification config ─────────────────────────────────────────────

function TelegramSection() {
  const qc = useQueryClient()
  const [error, setError] = useState<string | null>(null)
  const [testResult, setTestResult] = useState<'success' | 'error' | null>(null)

  // Pairing flow state
  const [botToken, setBotToken] = useState('')
  const [pairingId, setPairingId] = useState<string | null>(null)
  const [botUsername, setBotUsername] = useState<string | null>(null)
  const [pairingStatus, setPairingStatus] = useState<string | null>(null)
  const [code, setCode] = useState('')
  const [pairingSuccess, setPairingSuccess] = useState(false)
  const pollRef = useRef<ReturnType<typeof setInterval> | null>(null)

  const { data: configs } = useQuery({
    queryKey: ['notifications'],
    queryFn: (): Promise<NotificationConfig[]> => api.notifications.list(),
  })

  const tg = configs?.find((c: NotificationConfig) => c.channel === 'telegram')
  const isConfigured = Boolean(tg?.config?.bot_token)

  // Stop polling on unmount or when done
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
      // Start polling for status
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
      setTimeout(() => setPairingSuccess(false), 5000)
    },
    onError: (err: Error) => setError(err.message),
  })

  const deleteMut = useMutation({
    mutationFn: () => api.notifications.deleteTelegram(),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['notifications'] })
      setBotToken('')
      setTestResult(null)
      setPairingId(null)
      setPairingStatus(null)
      setPairingSuccess(false)
    },
  })

  const testMut = useMutation({
    mutationFn: () => api.notifications.testTelegram(),
    onSuccess: () => {
      setTestResult('success')
      setTimeout(() => setTestResult(null), 5000)
    },
    onError: (err: Error) => {
      setError(err.message)
      setTestResult('error')
      setTimeout(() => setTestResult(null), 5000)
    },
  })

  // Reset pairing flow
  const resetPairing = () => {
    stopPolling()
    setPairingId(null)
    setPairingStatus(null)
    setBotUsername(null)
    setCode('')
    setError(null)
  }

  return (
    <section className="space-y-4">
      <div>
        <h2 className="text-lg font-semibold text-text-primary">Telegram Notifications</h2>
        <p className="text-sm text-text-tertiary mt-0.5">
          Receive approval requests and status updates via Telegram.
        </p>
      </div>

      {error && <div className="text-sm text-danger max-w-lg">{error}</div>}
      {pairingSuccess && <div className="text-sm text-success max-w-lg">Paired successfully!</div>}

      {isConfigured ? (
        /* ── Configured state ──────────────────────────────────── */
        <div className="bg-surface-1 border border-border-default rounded-md p-5 space-y-3 max-w-lg">
          <div className="text-sm text-text-secondary space-y-1">
            <p><span className="font-medium text-text-tertiary">Bot token:</span> {tg!.config.bot_token.slice(0, 8)}...{tg!.config.bot_token.slice(-4)}</p>
            <p><span className="font-medium text-text-tertiary">Chat ID:</span> {tg!.config.chat_id}</p>
          </div>
          <div className="flex items-center gap-2 pt-1">
            <button
              onClick={() => testMut.mutate()}
              disabled={testMut.isPending}
              className="px-4 py-1.5 text-sm rounded border border-brand/30 text-brand hover:bg-brand/10 disabled:opacity-50"
            >
              {testMut.isPending ? 'Sending...' : 'Send test message'}
            </button>
            <button
              onClick={() => { deleteMut.mutate(); resetPairing() }}
              disabled={deleteMut.isPending}
              className="text-sm text-danger hover:text-red-400"
            >
              Remove
            </button>
            <button
              onClick={resetPairing}
              className="text-sm text-text-tertiary hover:text-text-primary"
            >
              Re-pair
            </button>
          </div>
          {testResult === 'success' && (
            <div className="text-sm text-success">Test message sent! Check your Telegram.</div>
          )}
          {testResult === 'error' && (
            <div className="text-sm text-danger">Test failed. Check your Telegram bot settings.</div>
          )}
        </div>
      ) : !pairingId ? (
        /* ── Step 1: Enter bot token ──────────────────────────── */
        <div className="space-y-3 max-w-lg">
          <div className="bg-surface-2 border border-border-default rounded-md p-4 text-sm text-text-secondary space-y-2">
            <p className="font-medium text-text-primary">Setup steps:</p>
            <ol className="list-decimal list-inside space-y-1.5 text-text-secondary">
              <li>Open Telegram and message <a href="https://t.me/BotFather" target="_blank" rel="noreferrer" className="text-brand hover:underline">@BotFather</a></li>
              <li>Send <code className="bg-surface-2 px-1 rounded text-xs">/newbot</code> and follow the prompts to create your bot</li>
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
        /* ── Step 2: Waiting for /start ───────────────────────── */
        <div className="bg-surface-1 border border-border-default rounded-md p-5 space-y-3 max-w-lg">
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
        /* ── Step 3: Enter pairing code ───────────────────────── */
        <div className="bg-surface-1 border border-border-default rounded-md p-5 space-y-3 max-w-lg">
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
        /* ── Expired ──────────────────────────────────────────── */
        <div className="bg-surface-1 border border-border-default rounded-md p-5 space-y-3 max-w-lg">
          <p className="text-sm text-danger">Pairing session expired. Please try again.</p>
          <button
            onClick={resetPairing}
            className="px-4 py-1.5 text-sm rounded bg-brand text-surface-0 hover:bg-brand-strong"
          >
            Start Over
          </button>
        </div>
      ) : null}
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
