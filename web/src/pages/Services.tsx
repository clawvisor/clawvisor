import { useState, useEffect } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { api, type ServiceInfo, type ServiceActionInfo } from '../api/client'
import { formatDistanceToNow } from 'date-fns'
import { serviceName, actionName, serviceDescription } from '../lib/services'

// ── Active Service Row ───────────────────────────────────────────────────────

function ActiveServiceRow({ svc }: { svc: ServiceInfo }) {
  const qc = useQueryClient()
  const [apiKeyInput, setApiKeyInput] = useState('')
  const [showKeyInput, setShowKeyInput] = useState(false)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const alias = svc.alias || undefined

  async function handleReauth() {
    setError(null)
    try {
      const resp = await api.services.oauthGetUrl(svc.id, undefined, alias)
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
    setError(null)
    try {
      await api.services.deactivate(svc.id, alias)
      qc.invalidateQueries({ queryKey: ['services'] })
    } catch (e: any) {
      setError(e.message ?? 'Failed to deactivate service')
    }
  }

  return (
    <div>
      <div className="flex items-center gap-4 px-4 py-3">
        {/* Name + meta */}
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-2">
            <span className="font-medium text-text-primary text-sm truncate">{serviceName(svc.id, svc.alias)}</span>
            <span className="text-xs text-text-tertiary shrink-0">
              {svc.id}{svc.alias && svc.alias !== 'default' ? `:${svc.alias}` : ''}
            </span>
          </div>
          <p className="text-xs text-text-tertiary mt-0.5">{svc.actions.map(a => a.display_name || actionName(a.id, svc.id)).join(' · ')}</p>
        </div>

        {/* Activated time */}
        {svc.activated_at && (
          <span className="text-xs text-text-tertiary shrink-0 hidden sm:block">
            {formatDistanceToNow(new Date(svc.activated_at), { addSuffix: true })}
          </span>
        )}

        {/* Actions */}
        {svc.requires_activation !== false && (
          <div className="flex gap-1.5 shrink-0">
            {!svc.credential_free && (svc.oauth ? (
              <button
                onClick={handleReauth}
                className="text-xs px-2.5 py-1 rounded border border-border-strong text-text-primary hover:bg-surface-2"
              >
                Re-authorize
              </button>
            ) : (
              <button
                onClick={() => { setShowKeyInput(v => !v); setError(null) }}
                className="text-xs px-2.5 py-1 rounded border border-border-strong text-text-primary hover:bg-surface-2"
              >
                Update token
              </button>
            ))}
            <button
              onClick={handleDeactivate}
              className="text-xs px-2.5 py-1 rounded bg-danger/10 text-danger border border-danger/20 hover:bg-danger/20"
            >
              Deactivate
            </button>
          </div>
        )}
      </div>

      {error && <p className="text-xs text-danger px-4 pb-2">{error}</p>}

      {showKeyInput && (
        <div className="flex gap-2 px-4 pb-3">
          <input
            type="password"
            value={apiKeyInput}
            onChange={e => setApiKeyInput(e.target.value)}
            onKeyDown={e => e.key === 'Enter' && handleSaveKey()}
            placeholder="Paste your token…"
            className="flex-1 text-xs px-2 py-1.5 border border-border-default bg-surface-0 text-text-primary rounded focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand placeholder:text-text-tertiary"
            autoFocus
          />
          <button
            onClick={handleSaveKey}
            disabled={saving || !apiKeyInput.trim()}
            className="text-xs px-3 py-1.5 rounded bg-brand text-surface-0 hover:bg-brand-strong disabled:opacity-50"
          >
            {saving ? 'Saving…' : 'Save'}
          </button>
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
  credentialFree: boolean
  actions: ServiceActionInfo[]
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

  // Build deduplicated service types
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
        credentialFree: svc.credential_free ?? false,
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
        onClose()
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
      onClose()
    } catch (e: any) {
      setError(e.message ?? 'Failed to save API key')
    } finally {
      setSaving(false)
    }
  }

  async function handleActivateCredentialFree(serviceId: string) {
    setError(null)
    try {
      await api.services.activate(serviceId)
      qc.invalidateQueries({ queryKey: ['services'] })
      onClose()
    } catch (e: any) {
      setError(e.message ?? 'Failed to activate service')
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
    <div className="fixed inset-0 z-50 flex items-center justify-center">
      {/* Backdrop */}
      <div className="absolute inset-0 bg-black/60" onClick={onClose} />

      {/* Modal */}
      <div className="relative bg-surface-1 border border-border-default rounded-md w-full max-w-lg mx-4 max-h-[80vh] flex flex-col">
        <div className="flex items-center justify-between px-6 py-4 border-b border-border-default">
          <h2 className="text-lg font-semibold text-text-primary">Add Service</h2>
          <button
            onClick={onClose}
            className="text-text-tertiary hover:text-text-primary text-xl leading-none"
          >
            &times;
          </button>
        </div>

        <div className="px-6 py-4 overflow-y-auto space-y-3">
          <p className="text-sm text-text-tertiary">Select a service to activate:</p>

          {error && <p className="text-xs text-danger">{error}</p>}

          {serviceTypes.map(st => {
            const isActivated = st.activatedCount > 0
            const desc = serviceDescription(st.baseId)
            return (
              <div key={st.baseId} className="border border-border-default rounded-md p-4 space-y-2">
                <div>
                  <h3 className="font-semibold text-text-primary">{serviceName(st.baseId)}</h3>
                  {desc && <p className="text-xs text-text-tertiary mt-0.5">{desc}</p>}
                  <p className="text-xs text-text-tertiary mt-0.5">
                    {st.credentialFree ? 'No credentials needed' : st.oauth ? 'Activate with OAuth' : 'Activate with API key'}
                  </p>
                </div>

                {/* Label input */}
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

                {/* Action button (hide when inline inputs are active for this service) */}
                {aliasInputFor !== st.baseId && keyInputFor !== st.baseId && (
                  <button
                    onClick={() => st.credentialFree ? handleActivateCredentialFree(st.baseId) : showAliasPrompt(st)}
                    className={`text-xs px-3 py-1.5 rounded ${isActivated
                      ? 'border border-border-strong text-text-primary hover:bg-surface-2'
                      : 'bg-brand text-surface-0 hover:bg-brand-strong'
                    }`}
                  >
                    {isActivated ? '+ Add account' : 'Activate'}
                  </button>
                )}
              </div>
            )
          })}

          {serviceTypes.length === 0 && (
            <p className="text-sm text-text-tertiary">No services available to activate.</p>
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
          <h1 className="text-2xl font-bold text-text-primary">Services</h1>
          <p className="text-sm text-text-tertiary mt-1">Your activated services.</p>
        </div>
        <button
          onClick={() => setShowModal(true)}
          className="px-4 py-2 rounded bg-brand text-surface-0 text-sm font-medium hover:bg-brand-strong"
        >
          + Add service
        </button>
      </div>

      {isLoading && <div className="text-sm text-text-tertiary">Loading…</div>}
      {error && <div className="text-sm text-danger">Failed to load services.</div>}

      {!isLoading && !error && activeServices.length === 0 && (
        <p className="text-sm text-text-tertiary">
          No services activated yet. Click "Add service" above to get started.
        </p>
      )}

      <div className="bg-surface-1 border border-border-default rounded-md divide-y divide-border-subtle">
        {activeServices.map(svc => (
          <ActiveServiceRow key={`${svc.id}:${svc.alias ?? 'default'}`} svc={svc} />
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
