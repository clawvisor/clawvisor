import { useState, useEffect } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { api, type ServiceInfo } from '../api/client'
import { formatDistanceToNow } from 'date-fns'
import { serviceName, actionName, serviceBrand, serviceDescription } from '../lib/services'

// ── Active Service Card ──────────────────────────────────────────────────────

function ActiveServiceCard({ svc }: { svc: ServiceInfo }) {
  const qc = useQueryClient()
  const [apiKeyInput, setApiKeyInput] = useState('')
  const [showKeyInput, setShowKeyInput] = useState(false)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const alias = svc.alias || undefined

  async function handleReauth() {
    setError(null)
    try {
      const { url } = await api.services.oauthGetUrl(svc.id, undefined, alias)
      const popup = window.open(url, '_blank', 'width=600,height=700')
      if (!popup) window.location.href = url
    } catch (e: any) {
      setError(e.message ?? 'Failed to start OAuth flow')
    }
  }

  async function handleSaveKey() {
    if (!apiKeyInput.trim()) return
    setSaving(true)
    setError(null)
    try {
      await api.services.activateWithKey(svc.id, apiKeyInput.trim(), alias)
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
    if (!confirm(`Deactivate ${serviceName(svc.id, svc.alias)}? Your agents will lose access.`)) return
    qc.invalidateQueries({ queryKey: ['services'] })
  }

  const brand = serviceBrand(svc.id)

  return (
    <div className={`bg-white border rounded-lg p-5 space-y-3 border-l-4 ${brand.border}`}>
      <div className="flex items-start justify-between">
        <div>
          <h3 className="font-semibold text-gray-900">{serviceName(svc.id, svc.alias)}</h3>
          <p className="text-xs text-gray-400 mt-0.5">
            {svc.id}{svc.alias && svc.alias !== 'default' ? `:${svc.alias}` : ''}
          </p>
          <p className="text-xs text-gray-400 mt-0.5">{svc.actions.map(a => actionName(a)).join(' · ')}</p>
        </div>
        <span className="px-2 py-0.5 rounded-full bg-green-100 text-green-700 text-xs font-medium">Active</span>
      </div>

      {svc.activated_at && (
        <p className="text-xs text-gray-400">
          Activated {formatDistanceToNow(new Date(svc.activated_at), { addSuffix: true })}
        </p>
      )}

      {error && <p className="text-xs text-red-500">{error}</p>}

      {svc.requires_activation !== false && (
        <div className="pt-1 space-y-2">
          <div className="flex gap-2">
            {svc.oauth ? (
              <button
                onClick={handleReauth}
                className="text-xs px-3 py-1.5 rounded border border-gray-300 text-gray-600 hover:bg-gray-50"
              >
                Re-authorize
              </button>
            ) : (
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
      )}
    </div>
  )
}

// ── Add Service Modal ────────────────────────────────────────────────────────

interface ServiceType {
  baseId: string
  oauth: boolean
  requiresActivation: boolean
  actions: string[]
  activatedCount: number
}

function AddServiceModal({
  services,
  onClose,
}: {
  services: ServiceInfo[]
  onClose: () => void
}) {
  const qc = useQueryClient()
  const [aliasInputFor, setAliasInputFor] = useState<string | null>(null)
  const [aliasValue, setAliasValue] = useState('')
  const [keyInputFor, setKeyInputFor] = useState<string | null>(null)
  const [keyValue, setKeyValue] = useState('')
  const [keyAlias, setKeyAlias] = useState<string | undefined>(undefined)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)

  // Close on Escape
  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose])

  // Close modal when OAuth completes
  useEffect(() => {
    function handler(e: MessageEvent) {
      if (e.data?.type === 'clawvisor_oauth_done') {
        qc.invalidateQueries({ queryKey: ['services'] })
        onClose()
      }
    }
    window.addEventListener('message', handler)
    return () => window.removeEventListener('message', handler)
  }, [qc, onClose])

  // Build deduplicated service types (exclude credential-free services)
  const typeMap = new Map<string, ServiceType>()
  for (const svc of services) {
    if (!(svc.requires_activation ?? true)) continue
    const baseId = svc.id
    const existing = typeMap.get(baseId)
    if (existing) {
      if (svc.status === 'activated') existing.activatedCount++
    } else {
      typeMap.set(baseId, {
        baseId,
        oauth: svc.oauth,
        requiresActivation: svc.requires_activation ?? true,
        actions: svc.actions,
        activatedCount: svc.status === 'activated' ? 1 : 0,
      })
    }
  }
  const serviceTypes = Array.from(typeMap.values())

  async function handleActivateOAuth(serviceId: string, alias?: string) {
    setError(null)
    try {
      const { url } = await api.services.oauthGetUrl(serviceId, undefined, alias)
      const popup = window.open(url, '_blank', 'width=600,height=700')
      if (!popup) window.location.href = url
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
      onClose()
    } catch (e: any) {
      setError(e.message ?? 'Failed to save API key')
    } finally {
      setSaving(false)
    }
  }

  function startActivation(st: ServiceType, alias?: string) {
    setError(null)
    setAliasInputFor(null)
    setAliasValue('')
    if (st.oauth) {
      handleActivateOAuth(st.baseId, alias)
    } else {
      setKeyInputFor(st.baseId)
      setKeyAlias(alias)
    }
  }

  function startAddAccount(st: ServiceType) {
    setError(null)
    setKeyInputFor(null)
    setKeyValue('')
    setAliasInputFor(st.baseId)
    setAliasValue('')
  }

  function confirmAlias(st: ServiceType) {
    const alias = aliasValue.trim()
    if (!alias) return
    setAliasInputFor(null)
    startActivation(st, alias)
  }

  const brand = serviceBrand

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center">
      {/* Backdrop */}
      <div className="absolute inset-0 bg-black/40" onClick={onClose} />

      {/* Modal */}
      <div className="relative bg-white rounded-lg shadow-xl w-full max-w-lg mx-4 max-h-[80vh] flex flex-col">
        <div className="flex items-center justify-between px-6 py-4 border-b">
          <h2 className="text-lg font-semibold text-gray-900">Add Service</h2>
          <button
            onClick={onClose}
            className="text-gray-400 hover:text-gray-600 text-xl leading-none"
          >
            &times;
          </button>
        </div>

        <div className="px-6 py-4 overflow-y-auto space-y-3">
          <p className="text-sm text-gray-500">Select a service to activate:</p>

          {error && <p className="text-xs text-red-500">{error}</p>}

          {serviceTypes.map(st => {
            const b = brand(st.baseId)
            const isActivated = st.activatedCount > 0
            const desc = serviceDescription(st.baseId)
            return (
              <div key={st.baseId} className={`border rounded-lg p-4 space-y-2 border-l-4 ${b.border}`}>
                <div>
                  <h3 className="font-semibold text-gray-900">{serviceName(st.baseId)}</h3>
                  {desc && <p className="text-xs text-gray-500 mt-0.5">{desc}</p>}
                  <p className="text-xs text-gray-400 mt-0.5">
                    {st.oauth ? 'Activate with OAuth' : 'Activate with API key'}
                  </p>
                </div>

                {/* Alias input */}
                {aliasInputFor === st.baseId && (
                  <div className="flex gap-2">
                    <input
                      type="text"
                      value={aliasValue}
                      onChange={e => setAliasValue(e.target.value)}
                      onKeyDown={e => e.key === 'Enter' && confirmAlias(st)}
                      placeholder="Account name (e.g. personal)"
                      className="flex-1 text-xs px-2 py-1.5 border rounded focus:outline-none focus:ring-1 focus:ring-blue-500"
                      autoFocus
                    />
                    <button
                      onClick={() => confirmAlias(st)}
                      disabled={!aliasValue.trim()}
                      className="text-xs px-3 py-1.5 rounded bg-blue-600 text-white hover:bg-blue-700 disabled:opacity-50"
                    >
                      Continue
                    </button>
                    <button
                      onClick={() => setAliasInputFor(null)}
                      className="text-xs px-3 py-1.5 rounded border border-gray-300 text-gray-600 hover:bg-gray-50"
                    >
                      Cancel
                    </button>
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
                      className="flex-1 text-xs px-2 py-1.5 border rounded focus:outline-none focus:ring-1 focus:ring-blue-500"
                      autoFocus
                    />
                    <button
                      onClick={handleSaveKey}
                      disabled={saving || !keyValue.trim()}
                      className="text-xs px-3 py-1.5 rounded bg-blue-600 text-white hover:bg-blue-700 disabled:opacity-50"
                    >
                      {saving ? 'Saving…' : 'Save'}
                    </button>
                    <button
                      onClick={() => { setKeyInputFor(null); setKeyValue('') }}
                      className="text-xs px-3 py-1.5 rounded border border-gray-300 text-gray-600 hover:bg-gray-50"
                    >
                      Cancel
                    </button>
                  </div>
                )}

                {/* Action buttons (hide when inline inputs are active for this service) */}
                {aliasInputFor !== st.baseId && keyInputFor !== st.baseId && (
                  <div className="flex gap-2">
                    {!isActivated && (
                      <button
                        onClick={() => startActivation(st)}
                        className="text-xs px-3 py-1.5 rounded bg-blue-600 text-white hover:bg-blue-700"
                      >
                        Activate
                      </button>
                    )}
                    {isActivated && (
                      <button
                        onClick={() => startAddAccount(st)}
                        className="text-xs px-3 py-1.5 rounded border border-gray-300 text-gray-600 hover:bg-gray-50"
                      >
                        + Add account
                      </button>
                    )}
                  </div>
                )}
              </div>
            )
          })}

          {serviceTypes.length === 0 && (
            <p className="text-sm text-gray-400">No services available to activate.</p>
          )}
        </div>
      </div>
    </div>
  )
}

// ── Main Page ────────────────────────────────────────────────────────────────

export default function Services() {
  const qc = useQueryClient()
  const [showModal, setShowModal] = useState(false)

  const { data, isLoading, error } = useQuery({
    queryKey: ['services'],
    queryFn: () => api.services.list(),
  })

  // Refresh when the OAuth popup signals completion (for cases where modal isn't open).
  useEffect(() => {
    function handler(e: MessageEvent) {
      if (e.data?.type === 'clawvisor_oauth_done') {
        qc.invalidateQueries({ queryKey: ['services'] })
      }
    }
    window.addEventListener('message', handler)
    return () => window.removeEventListener('message', handler)
  }, [qc])

  const allServices = data?.services ?? []
  const activeServices = allServices.filter(s => s.status === 'activated')

  return (
    <div className="p-8 space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-bold text-gray-900">Services</h1>
          <p className="text-sm text-gray-500 mt-1">Your activated services.</p>
        </div>
        <button
          onClick={() => setShowModal(true)}
          className="px-4 py-2 rounded-lg bg-blue-600 text-white text-sm font-medium hover:bg-blue-700"
        >
          + Add service
        </button>
      </div>

      {isLoading && <div className="text-sm text-gray-400">Loading…</div>}
      {error && <div className="text-sm text-red-500">Failed to load services.</div>}

      {!isLoading && !error && activeServices.length === 0 && (
        <p className="text-sm text-gray-400">
          No services activated yet. Click "Add service" above to get started.
        </p>
      )}

      <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4">
        {activeServices.map(svc => (
          <ActiveServiceCard key={`${svc.id}:${svc.alias ?? 'default'}`} svc={svc} />
        ))}
      </div>

      {showModal && (
        <AddServiceModal
          services={allServices}
          onClose={() => setShowModal(false)}
        />
      )}
    </div>
  )
}
