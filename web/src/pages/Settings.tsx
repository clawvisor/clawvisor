import { useState, useEffect } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import type { NotificationConfig } from '../api/client'
import { useNavigate } from 'react-router-dom'
import { api, APIError } from '../api/client'
import { useAuth } from '../hooks/useAuth'

export default function Settings() {
  return (
    <div className="p-8 space-y-10">
      <h1 className="text-2xl font-bold text-gray-900">Settings</h1>
      <TelegramSection />
      <PasswordSection />
      <DangerZone />
    </div>
  )
}

// ── Telegram notification config ─────────────────────────────────────────────

function TelegramSection() {
  const qc = useQueryClient()
  const [botToken, setBotToken] = useState('')
  const [chatId, setChatId] = useState('')
  const [saved, setSaved] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const { data: configs } = useQuery({
    queryKey: ['notifications'],
    queryFn: (): Promise<NotificationConfig[]> => api.notifications.list(),
  })

  // Populate form when data loads
  useEffect(() => {
    if (!configs) return
    const tg = configs.find((c: NotificationConfig) => c.channel === 'telegram')
    if (tg?.config?.bot_token) setBotToken(tg.config.bot_token)
    if (tg?.config?.chat_id) setChatId(tg.config.chat_id)
  }, [configs])

  const saveMut = useMutation({
    mutationFn: () => api.notifications.upsertTelegram(botToken, chatId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['notifications'] })
      setSaved(true)
      setTimeout(() => setSaved(false), 3000)
    },
    onError: (err: Error) => setError(err.message),
  })

  const deleteMut = useMutation({
    mutationFn: () => api.notifications.deleteTelegram(),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['notifications'] })
      setBotToken('')
      setChatId('')
    },
  })

  const tg = configs?.find((c: NotificationConfig) => c.channel === 'telegram')
  const isConfigured = Boolean(tg?.config?.bot_token)

  return (
    <section className="space-y-4">
      <div>
        <h2 className="text-lg font-semibold text-gray-800">Telegram Notifications</h2>
        <p className="text-sm text-gray-500 mt-0.5">
          Receive approval requests via Telegram. Create a bot via{' '}
          <a href="https://t.me/BotFather" target="_blank" rel="noreferrer" className="text-blue-600 hover:underline">BotFather</a>.
        </p>
      </div>

      {error && <div className="text-sm text-red-600">{error}</div>}

      <div className="bg-white border rounded-lg p-5 space-y-3 max-w-lg">
        <div>
          <label className="text-xs font-medium text-gray-600">Bot Token</label>
          <input
            type="password"
            value={botToken}
            onChange={e => setBotToken(e.target.value)}
            placeholder="1234567890:ABCDEF..."
            className="mt-1 block w-full text-sm rounded border border-gray-300 px-3 py-1.5 focus:outline-none focus:ring-1 focus:ring-blue-400"
          />
        </div>
        <div>
          <label className="text-xs font-medium text-gray-600">Chat ID</label>
          <input
            value={chatId}
            onChange={e => setChatId(e.target.value)}
            placeholder="Your Telegram chat or group ID"
            className="mt-1 block w-full text-sm rounded border border-gray-300 px-3 py-1.5 focus:outline-none focus:ring-1 focus:ring-blue-400"
          />
        </div>
        <div className="flex items-center gap-2 pt-1">
          <button
            onClick={() => saveMut.mutate()}
            disabled={saveMut.isPending || !botToken || !chatId}
            className="px-4 py-1.5 text-sm rounded bg-blue-600 text-white hover:bg-blue-700 disabled:opacity-50"
          >
            {saveMut.isPending ? 'Saving…' : saved ? 'Saved ✓' : 'Save'}
          </button>
          {isConfigured && (
            <button
              onClick={() => deleteMut.mutate()}
              disabled={deleteMut.isPending}
              className="text-sm text-red-500 hover:text-red-700"
            >
              Remove
            </button>
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
      <h2 className="text-lg font-semibold text-gray-800">Change Password</h2>
      {error && <div className="text-sm text-red-600">{error}</div>}
      {success && <div className="text-sm text-green-600">Password updated successfully.</div>}
      <div className="bg-white border rounded-lg p-5 space-y-3 max-w-lg">
        <div>
          <label className="text-xs font-medium text-gray-600">Current password</label>
          <input
            type="password"
            value={current}
            onChange={e => setCurrent(e.target.value)}
            className="mt-1 block w-full text-sm rounded border border-gray-300 px-3 py-1.5 focus:outline-none focus:ring-1 focus:ring-blue-400"
          />
        </div>
        <div>
          <label className="text-xs font-medium text-gray-600">New password</label>
          <input
            type="password"
            value={next}
            onChange={e => setNext(e.target.value)}
            className="mt-1 block w-full text-sm rounded border border-gray-300 px-3 py-1.5 focus:outline-none focus:ring-1 focus:ring-blue-400"
          />
        </div>
        <div>
          <label className="text-xs font-medium text-gray-600">Confirm new password</label>
          <input
            type="password"
            value={confirm}
            onChange={e => setConfirm(e.target.value)}
            className="mt-1 block w-full text-sm rounded border border-gray-300 px-3 py-1.5 focus:outline-none focus:ring-1 focus:ring-blue-400"
          />
        </div>
        <button
          onClick={handleSubmit}
          disabled={changeMut.isPending || !current || !next}
          className="px-4 py-1.5 text-sm rounded bg-blue-600 text-white hover:bg-blue-700 disabled:opacity-50"
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
      <h2 className="text-lg font-semibold text-red-700">Danger Zone</h2>
      <div className="border border-red-200 rounded-lg p-5 space-y-3 max-w-lg">
        <div>
          <p className="text-sm font-medium text-gray-800">Delete Account</p>
          <p className="text-xs text-gray-500 mt-0.5">
            Permanently delete your account and all data. This cannot be undone.
          </p>
        </div>
        {!open ? (
          <button
            onClick={() => setOpen(true)}
            className="text-sm px-3 py-1.5 rounded border border-red-300 text-red-600 hover:bg-red-50"
          >
            Delete my account
          </button>
        ) : (
          <div className="space-y-3">
            <p className="text-xs text-red-600">Enter your password to confirm deletion:</p>
            {error && <div className="text-xs text-red-600">{error}</div>}
            <input
              type="password"
              value={password}
              onChange={e => setPassword(e.target.value)}
              placeholder="Your password"
              className="block w-full text-sm rounded border border-red-300 px-3 py-1.5 focus:outline-none focus:ring-1 focus:ring-red-400"
            />
            <div className="flex gap-2">
              <button
                onClick={() => deleteMut.mutate()}
                disabled={deleteMut.isPending || !password}
                className="text-sm px-3 py-1.5 rounded bg-red-600 text-white hover:bg-red-700 disabled:opacity-50"
              >
                {deleteMut.isPending ? 'Deleting…' : 'Confirm Delete'}
              </button>
              <button
                onClick={() => { setOpen(false); setPassword(''); setError(null) }}
                className="text-sm px-3 py-1.5 rounded border border-gray-300 text-gray-600 hover:bg-gray-50"
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
