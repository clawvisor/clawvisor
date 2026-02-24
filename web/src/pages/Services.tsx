import { useState, useEffect } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { api, type ServiceInfo } from '../api/client'
import { formatDistanceToNow } from 'date-fns'

function ServiceCard({ svc }: { svc: ServiceInfo }) {
  const qc = useQueryClient()
  const [apiKeyInput, setApiKeyInput] = useState('')
  const [showKeyInput, setShowKeyInput] = useState(false)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)

  async function handleActivateOAuth() {
    setError(null)
    try {
      const { url } = await api.services.oauthGetUrl(svc.id)
      // Open consent screen in a new window; current dashboard stays open.
      const popup = window.open(url, '_blank', 'width=600,height=700')
      if (!popup) {
        // Fallback if popups are blocked — navigate current window.
        window.location.href = url
      }
    } catch (e: any) {
      setError(e.message ?? 'Failed to start OAuth flow')
    }
  }

  async function handleSaveKey() {
    if (!apiKeyInput.trim()) return
    setSaving(true)
    setError(null)
    try {
      await api.services.activateWithKey(svc.id, apiKeyInput.trim())
      setApiKeyInput('')
      setShowKeyInput(false)
      qc.invalidateQueries({ queryKey: ['services'] })
    } catch (e: any) {
      setError(e.message ?? 'Failed to save API key')
    } finally {
      setSaving(false)
    }
  }

  async function handleDeactivate() {
    if (!confirm(`Deactivate ${svc.id}? Your agents will lose access to this service.`)) return
    qc.invalidateQueries({ queryKey: ['services'] })
  }

  const isActivated = svc.status === 'activated'

  return (
    <div className="bg-white border rounded-lg p-5 space-y-3">
      <div className="flex items-start justify-between">
        <div>
          <h3 className="font-semibold text-gray-900">{svc.id}</h3>
          <p className="text-xs text-gray-400 mt-0.5">{svc.actions.join(' · ')}</p>
        </div>
        <StatusBadge status={svc.status} />
      </div>

      {isActivated && svc.activated_at && (
        <p className="text-xs text-gray-400">
          Activated {formatDistanceToNow(new Date(svc.activated_at), { addSuffix: true })}
        </p>
      )}

      {error && <p className="text-xs text-red-500">{error}</p>}

      <div className="pt-1 space-y-2">
        {svc.requires_activation === false ? null : isActivated ? (
          <div className="flex gap-2">
            {svc.oauth && (
              <button
                onClick={handleActivateOAuth}
                className="text-xs px-3 py-1.5 rounded border border-gray-300 text-gray-600 hover:bg-gray-50"
              >
                Re-authorize
              </button>
            )}
            {!svc.oauth && (
              <button
                onClick={() => { setShowKeyInput(v => !v); setError(null) }}
                className="text-xs px-3 py-1.5 rounded border border-gray-300 text-gray-600 hover:bg-gray-50"
              >
                Update token
              </button>
            )}
            <button
              onClick={handleDeactivate}
              className="text-xs px-3 py-1.5 rounded border border-red-200 text-red-600 hover:bg-red-50"
            >
              Deactivate
            </button>
          </div>
        ) : svc.oauth ? (
          <button
            onClick={handleActivateOAuth}
            className="text-xs px-3 py-1.5 rounded bg-blue-600 text-white hover:bg-blue-700"
          >
            Activate with OAuth ↗
          </button>
        ) : (
          <button
            onClick={() => { setShowKeyInput(v => !v); setError(null) }}
            className="text-xs px-3 py-1.5 rounded bg-blue-600 text-white hover:bg-blue-700"
          >
            Activate with API key
          </button>
        )}

        {showKeyInput && (
          <div className="flex gap-2">
            <input
              type="password"
              value={apiKeyInput}
              onChange={e => setApiKeyInput(e.target.value)}
              onKeyDown={e => e.key === 'Enter' && handleSaveKey()}
              placeholder="Paste your token…"
              className="flex-1 text-xs px-2 py-1.5 border rounded focus:outline-none focus:ring-1 focus:ring-blue-500"
              autoFocus
            />
            <button
              onClick={handleSaveKey}
              disabled={saving || !apiKeyInput.trim()}
              className="text-xs px-3 py-1.5 rounded bg-blue-600 text-white hover:bg-blue-700 disabled:opacity-50"
            >
              {saving ? 'Saving…' : 'Save'}
            </button>
          </div>
        )}
      </div>
    </div>
  )
}

function StatusBadge({ status }: { status: string }) {
  if (status === 'activated') {
    return <span className="px-2 py-0.5 rounded-full bg-green-100 text-green-700 text-xs font-medium">Activated</span>
  }
  return <span className="px-2 py-0.5 rounded-full bg-gray-100 text-gray-500 text-xs font-medium">Not activated</span>
}

export default function Services() {
  const qc = useQueryClient()
  const { data, isLoading, error } = useQuery({
    queryKey: ['services'],
    queryFn: () => api.services.list(),
  })

  // Refresh when the OAuth popup signals completion.
  useEffect(() => {
    function handler(e: MessageEvent) {
      if (e.data?.type === 'clawvisor_oauth_done') {
        qc.invalidateQueries({ queryKey: ['services'] })
      }
    }
    window.addEventListener('message', handler)
    return () => window.removeEventListener('message', handler)
  }, [qc])

  return (
    <div className="p-8 space-y-6">
      <h1 className="text-2xl font-bold text-gray-900">Services</h1>
      <p className="text-sm text-gray-500">
        Activate services to let your agents use them. Credentials are stored securely in the vault.
        OAuth services open a consent window. API key services (e.g. GitHub) accept a personal access token.
      </p>

      {isLoading && <div className="text-sm text-gray-400">Loading…</div>}
      {error && <div className="text-sm text-red-500">Failed to load services.</div>}

      {data && data.services.length === 0 && (
        <p className="text-sm text-gray-400">No adapters registered. Add one in the server configuration.</p>
      )}

      <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4">
        {data?.services.map(svc => (
          <ServiceCard key={svc.id} svc={svc} />
        ))}
      </div>
    </div>
  )
}
