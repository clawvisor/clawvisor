import { useState, useEffect, useRef, useCallback, type ReactNode, useMemo } from 'react'
import { useQuery, useQueryClient, useMutation } from '@tanstack/react-query'
import { NavLink } from 'react-router-dom'
import { api, type Agent, type ServiceInfo, type ServiceActionInfo, type VariableMeta, type LocalService, type RuntimePlaceholder, type VaultItem } from '../api/client'
import { formatDistanceToNow } from 'date-fns'
import { serviceName, serviceDescription } from '../lib/services'
import { useAuth } from '../hooks/useAuth'
import { ServiceIconBadge } from '../components/ServiceIcon'

const isMobile = /iPhone|iPad|iPod|Android/i.test(navigator.userAgent)

function openOAuthUrl(url: string) {
  if (isMobile) {
    window.location.href = url
    return
  }
  const popup = window.open(url, '_blank', 'width=600,height=700')
  if (!popup) window.location.href = url
}

function AccountSection({
  title,
  description,
  children,
}: {
  title: string
  description?: string
  children: ReactNode
}) {
  return (
    <section className="rounded-lg border border-border-default bg-surface-1 p-5 space-y-4">
      <div>
        <h2 className="text-lg font-semibold text-text-primary">{title}</h2>
        {description && <p className="mt-1 text-sm text-text-tertiary">{description}</p>}
      </div>
      {children}
    </section>
  )
}

function VaultInventorySection({
  vaultItems,
  placeholders,
  agents,
  services,
  googleOAuthConfigured,
}: {
  vaultItems: VaultItem[]
  placeholders: RuntimePlaceholder[]
  agents: Agent[]
  services: ServiceInfo[]
  googleOAuthConfigured: boolean
}) {
  const qc = useQueryClient()
  const [selectedID, setSelectedID] = useState('')
  const [addingSecret, setAddingSecret] = useState(false)
  const [newSecretID, setNewSecretID] = useState('')
  const [newSecretValue, setNewSecretValue] = useState('')
  const [newSecretError, setNewSecretError] = useState<string | null>(null)
  const [savingSecret, setSavingSecret] = useState(false)
  const selected = vaultItems.find(item => item.id === selectedID) ?? vaultItems[0]
  const connectedItems = vaultItems.filter(item => item.kind === 'connected_account')
  const secretItems = vaultItems.filter(item => item.kind !== 'connected_account')
  const activeCount = vaultItems.reduce((sum, item) => sum + item.active_placeholder_count, 0)
  const taskBoundCount = placeholders.filter(entry => !!entry.task_id && isPlaceholderActive(entry)).length
  const agentMap = useMemo(() => new Map(agents.map(agent => [agent.id, agent])), [agents])
  const serviceMap = useMemo(() => {
    const out = new Map<string, ServiceInfo>()
    for (const svc of services) out.set(serviceConnectionKey(svc.id, svc.alias), svc)
    return out
  }, [services])
  const selectedService = selected ? serviceForVaultItem(selected, serviceMap) : undefined
  const selectedPlaceholders = placeholders.filter(entry => selected ? placeholderBelongsToVaultItem(entry, selected) : false)

  async function handleCreateSecret() {
    const id = newSecretID.trim()
    const value = newSecretValue.trim()
    if (!id || !value) return
    setSavingSecret(true)
    setNewSecretError(null)
    try {
      await api.vault.createItem(id, value)
      setSelectedID(id)
      setNewSecretID('')
      setNewSecretValue('')
      setAddingSecret(false)
      qc.invalidateQueries({ queryKey: ['vault-items'] })
    } catch (e: any) {
      setNewSecretError(e.message ?? 'Failed to add secret')
    } finally {
      setSavingSecret(false)
    }
  }

  function renderItemGroup(items: VaultItem[], empty: string) {
    if (items.length === 0) {
      return (
        <div className="rounded border border-dashed border-border-default bg-surface-0 px-4 py-5 text-sm text-text-tertiary">
          {empty}
        </div>
      )
    }
    return items.map(item => {
      const service = serviceForVaultItem(item, serviceMap)
      const showBindingChips = item.kind !== 'connected_account' && (item.service_bindings?.length ?? 0) > 0
      return (
        <button
          key={item.id}
          onClick={() => setSelectedID(item.id)}
          className={`w-full rounded border px-4 py-3 text-left transition-colors ${
            selected?.id === item.id
              ? 'border-brand/50 bg-brand/5'
              : 'border-border-subtle bg-surface-0 hover:bg-surface-2'
          }`}
        >
          <div className="flex items-center justify-between gap-3">
            <div className="flex min-w-0 items-center gap-3">
              {service && <ServiceIconBadge iconSvg={service.icon_svg} iconUrl={service.icon_url} serviceId={service.id} size={28} />}
              <div className="min-w-0">
                <div className="truncate text-sm font-medium text-text-primary">{service ? serviceName(service.id, service.alias) : item.name}</div>
                <div className="mt-1 text-xs text-text-tertiary">
                  {vaultItemKindLabel(item)}
                  {item.provider ? ` · ${item.provider}` : ''}
                  {item.last_used_at ? ` · used ${formatDistanceToNow(new Date(item.last_used_at), { addSuffix: true })}` : ''}
                </div>
              </div>
            </div>
            <div className="shrink-0 rounded bg-surface-2 px-2 py-1 text-xs text-text-secondary">
              {item.active_placeholder_count} live
            </div>
          </div>
          {showBindingChips && item.service_bindings && (
            <div className="mt-2 flex flex-wrap gap-1.5">
              {item.service_bindings.map(binding => (
                <span key={`${binding.service_id}:${binding.alias ?? 'default'}`} className="rounded border border-border-subtle px-2 py-0.5 text-xs text-text-tertiary">
                  {binding.name}{binding.alias ? ` · ${binding.alias}` : ''}
                </span>
              ))}
            </div>
          )}
        </button>
      )
    })
  }

  return (
    <AccountSection
      title="Vault"
      description="Connected accounts and vaulted secrets share one inventory."
    >
      <div className="grid gap-3 md:grid-cols-4">
        <VaultMetric label="Vault items" value={String(vaultItems.length)} />
        <VaultMetric label="Active placeholders" value={String(activeCount)} />
        <VaultMetric label="Task-bound" value={String(taskBoundCount)} />
        <VaultMetric label="Google OAuth" value={googleOAuthConfigured ? 'Configured' : 'Missing'} />
      </div>

      <div className="grid gap-4 lg:grid-cols-[minmax(0,1fr)_minmax(320px,0.9fr)]">
        <div className="space-y-2">
          <div className="space-y-2">
            <div className="text-xs font-medium uppercase tracking-wider text-text-tertiary">Connected accounts</div>
            {renderItemGroup(connectedItems, 'No connected accounts yet.')}
          </div>
          <div className="space-y-2 pt-3">
            <div className="flex items-center justify-between gap-3">
              <div className="text-xs font-medium uppercase tracking-wider text-text-tertiary">Vault secrets</div>
              <button
                onClick={() => { setAddingSecret(v => !v); setNewSecretError(null) }}
                className="text-xs px-2.5 py-1 rounded border border-border-strong text-text-primary hover:bg-surface-2"
              >
                Add secret
              </button>
            </div>
            {addingSecret && (
              <div className="rounded border border-border-subtle bg-surface-0 px-4 py-3 space-y-2">
                <div className="grid gap-2 sm:grid-cols-[minmax(0,0.8fr)_minmax(0,1.2fr)_auto]">
                  <input
                    value={newSecretID}
                    onChange={e => setNewSecretID(e.target.value)}
                    placeholder="name, e.g. stripe.test"
                    className="min-w-0 text-xs px-2 py-1.5 border border-border-default bg-surface-0 text-text-primary rounded focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand placeholder:text-text-tertiary"
                    autoFocus
                  />
                  <input
                    type="password"
                    value={newSecretValue}
                    onChange={e => setNewSecretValue(e.target.value)}
                    onKeyDown={e => e.key === 'Enter' && handleCreateSecret()}
                    placeholder="Secret value"
                    className="min-w-0 text-xs px-2 py-1.5 border border-border-default bg-surface-0 text-text-primary rounded focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand placeholder:text-text-tertiary"
                  />
                  <button
                    onClick={handleCreateSecret}
                    disabled={savingSecret || !newSecretID.trim() || !newSecretValue.trim()}
                    className="text-xs px-3 py-1.5 rounded bg-brand text-surface-0 hover:bg-brand-strong disabled:opacity-50"
                  >
                    {savingSecret ? 'Saving…' : 'Save'}
                  </button>
                </div>
                {newSecretError && <p className="text-xs text-danger">{newSecretError}</p>}
              </div>
            )}
            {renderItemGroup(secretItems, 'No manual or captured vault secrets yet.')}
          </div>
        </div>

        <div className="rounded border border-border-subtle bg-surface-0 p-4">
          {!selected && (
            <div className="text-sm text-text-tertiary">Select a vault item.</div>
          )}
          {selected && (
            <div className="space-y-4">
              <div>
                <div className="text-sm font-semibold text-text-primary">{selected.name}</div>
                <div className="mt-1 text-xs text-text-tertiary">
                  {selected.id} · {selected.status}
                  {selected.scope ? ` · ${selected.scope} scoped` : ''}
                </div>
                {selected.metadata?.agent_id && (
                  <div className="mt-2 text-xs text-text-tertiary">
                    Agent: {selected.metadata.agent_name ?? selected.metadata.agent_id}
                  </div>
                )}
              </div>
              <div className="grid grid-cols-3 gap-2">
                <VaultMetric label="Live" value={String(selectedPlaceholders.filter(isPlaceholderActive).length)} />
                <VaultMetric label="Expired" value={String(selectedPlaceholders.filter(isPlaceholderExpired).length)} />
                <VaultMetric label="Revoked" value={String(selectedPlaceholders.filter(entry => !!entry.revoked_at).length)} />
              </div>
              {selectedService ? <VaultServiceActions svc={selectedService} /> : <VaultSecretActions item={selected} />}
              <div>
                <div className="mb-2 text-xs font-medium uppercase tracking-wider text-text-tertiary">Placeholders</div>
                <div className="space-y-2">
                  {selectedPlaceholders.length === 0 && (
                    <div className="rounded border border-dashed border-border-default px-3 py-4 text-sm text-text-tertiary">
                      No placeholders for this vault item yet. Task approvals mint task-scoped placeholders automatically.
                    </div>
                  )}
                  {selectedPlaceholders.slice(0, 8).map(entry => (
                    <div key={entry.placeholder} className="rounded border border-border-subtle bg-surface-1 px-3 py-2">
                      <div className="flex items-center justify-between gap-2">
                        <div className="min-w-0 text-xs text-text-secondary">
                          {entry.task_id ? 'Task-scoped' : 'Manual'} · {entry.agent_id ? (agentMap.get(entry.agent_id)?.name ?? entry.agent_id) : 'All agents'}
                        </div>
                        <span className={`shrink-0 rounded px-2 py-0.5 text-xs ${placeholderStatusClass(entry)}`}>
                          {placeholderStatus(entry)}
                        </span>
                      </div>
                      <code className="mt-1 block truncate text-xs text-text-tertiary">{entry.placeholder}</code>
                      <div className="mt-1 text-xs text-text-tertiary">
                        Minted {formatDistanceToNow(new Date(entry.created_at), { addSuffix: true })}
                        {entry.expires_at ? ` · expires ${formatDistanceToNow(new Date(entry.expires_at), { addSuffix: true })}` : ''}
                        {entry.use_count ? ` · used ${entry.use_count}x` : ''}
                      </div>
                    </div>
                  ))}
                </div>
              </div>
              <div className="rounded border border-border-subtle bg-surface-1 px-3 py-2 text-xs text-text-tertiary">
                Manual placeholder minting should start from this detail surface. The current v1 path mints credentials through approved tasks so placeholders inherit the task lifetime.
              </div>
            </div>
          )}
        </div>
      </div>
    </AccountSection>
  )
}

function vaultItemKindLabel(item: VaultItem) {
  if (item.kind === 'connected_account') return 'Connected account'
  if (item.kind === 'llm_provider_key') return item.scope === 'agent' ? 'Agent-scoped LLM key' : 'User LLM key'
  return 'Vault secret'
}

function serviceConnectionKey(serviceID: string, alias?: string) {
  return alias && alias !== 'default' ? `${serviceID}:${alias}` : serviceID
}

function serviceForVaultItem(item: VaultItem, serviceMap: Map<string, ServiceInfo>) {
  for (const binding of item.service_bindings ?? []) {
    const svc = serviceMap.get(serviceConnectionKey(binding.service_id, binding.alias))
    if (svc) return svc
  }
  return serviceMap.get(item.id)
}

function vaultStorageKeyForItemID(itemID: string) {
  const parts = itemID.trim().split(':')
  if (parts.length === 3 && parts[0] === 'llm' && parts[2] === 'user' && isLLMProvider(parts[1])) {
    return parts[1]
  }
  if (parts.length === 4 && parts[0] === 'llm' && parts[2] === 'agent' && isLLMProvider(parts[1]) && parts[3]) {
    return `agent:${parts[3]}:${parts[1]}`
  }
  return itemID
}

function isLLMProvider(provider: string) {
  return provider === 'anthropic' || provider === 'openai'
}

function placeholderBelongsToVaultItem(entry: RuntimePlaceholder, item: VaultItem) {
  const storageKey = vaultStorageKeyForItemID(item.id)
  if (entry.vault_item_id === item.id || entry.vault_item_id === storageKey) return true
  if (!entry.vault_item_id && entry.service_id === storageKey) return true
  for (const binding of item.service_bindings ?? []) {
    if (entry.service_id === binding.service_id) return true
    if (binding.alias && entry.service_id === `${binding.service_id}:${binding.alias}`) return true
  }
  return false
}

function VaultServiceActions({ svc }: { svc: ServiceInfo }) {
  const qc = useQueryClient()
  const [apiKeyInput, setApiKeyInput] = useState('')
  const [showKeyInput, setShowKeyInput] = useState(false)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [deviceCode, setDeviceCode] = useState<{ userCode: string; verificationUri: string } | null>(null)
  const pollRef = useRef<ReturnType<typeof setTimeout> | null>(null)
  const alias = svc.alias || undefined

  useEffect(() => () => { if (pollRef.current) clearTimeout(pollRef.current) }, [])

  function refreshAccountData() {
    qc.invalidateQueries({ queryKey: ['services'] })
    qc.invalidateQueries({ queryKey: ['vault-items'] })
    qc.invalidateQueries({ queryKey: ['runtime-placeholders'] })
  }

  async function handleReauth() {
    setError(null)
    try {
      if (svc.pkce_flow) {
        const resp = await api.services.pkceFlowStart(svc.id, alias)
        if (resp.authorize_url) openOAuthUrl(resp.authorize_url)
      } else if (svc.device_flow) {
        const resp = await api.services.deviceFlowStart(svc.id, alias)
        setDeviceCode({ userCode: resp.user_code, verificationUri: resp.verification_uri })
        const popup = window.open(resp.verification_uri, '_blank', 'width=600,height=700')
        if (!popup) window.open(resp.verification_uri, '_blank')
        function poll(flowId: string, interval: number) {
          pollRef.current = setTimeout(async () => {
            try {
              const r = await api.services.deviceFlowPoll(svc.id, flowId)
              if (r.status === 'complete') {
                setDeviceCode(null)
                refreshAccountData()
              } else if (r.status === 'pending' || r.status === 'slow_down') {
                poll(flowId, r.interval ?? interval)
              } else {
                setDeviceCode(null)
                setError(r.status === 'denied' ? 'Authorization denied.' : 'Authorization expired.')
              }
            } catch (e) {
              console.error('Services: device flow poll failed', e)
              setDeviceCode(null)
              setError('Failed to check authorization status')
            }
          }, interval * 1000)
        }
        poll(resp.flow_id, resp.interval)
      } else {
        const resp = await api.services.oauthGetUrl(svc.id, undefined, alias)
        if (resp.already_authorized) {
          refreshAccountData()
          return
        }
        if (resp.url) openOAuthUrl(resp.url)
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
      refreshAccountData()
    } catch (e: any) {
      setError(e.message ?? 'Failed to save API key')
    } finally {
      setSaving(false)
    }
  }

  async function handleDeactivate() {
    setError(null)
    try {
      const { affected_task_count } = await api.services.deactivatePreflight(svc.id, alias)
      const name = serviceName(svc.id, svc.alias)
      const taskWarning = affected_task_count > 0
        ? `\n\nThis will revoke ${affected_task_count} active task${affected_task_count === 1 ? '' : 's'} that use${affected_task_count === 1 ? 's' : ''} this service.`
        : ''
      if (!confirm(`Disconnect ${name}? Your agents will lose access.${taskWarning}`)) return
      await api.services.deactivate(svc.id, alias)
      refreshAccountData()
    } catch (e: any) {
      setError(e.message ?? 'Failed to disconnect service')
    }
  }

  return (
    <div className="rounded border border-border-subtle bg-surface-1 px-3 py-3">
      <div className="flex flex-wrap items-center gap-2">
        {!svc.credential_free && (svc.oauth || svc.pkce_flow || svc.device_flow ? (
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
          className="text-xs px-2.5 py-1 rounded text-danger border border-danger/20 hover:bg-danger/10"
        >
          Disconnect
        </button>
        {svc.activated_at && (
          <span className="text-xs text-text-tertiary">
            Connected {formatDistanceToNow(new Date(svc.activated_at), { addSuffix: true })}
          </span>
        )}
      </div>

      {error && <p className="mt-2 text-xs text-danger">{error}</p>}

      {deviceCode && (
        <div className="mt-3 space-y-1.5">
          <p className="text-xs text-text-secondary">Enter this code on the authorization page:</p>
          <div className="flex flex-wrap items-center gap-2">
            <code className="text-sm font-mono font-bold tracking-widest text-text-primary bg-surface-0 px-3 py-1.5 rounded border border-border-default select-all">
              {deviceCode.userCode}
            </code>
            <button
              onClick={() => navigator.clipboard.writeText(deviceCode.userCode)}
              className="text-xs px-2 py-1 rounded border border-border-strong text-text-primary hover:bg-surface-2"
            >
              Copy
            </button>
            <button
              onClick={() => window.open(deviceCode.verificationUri, '_blank')}
              className="text-xs px-2 py-1 rounded border border-border-strong text-text-primary hover:bg-surface-2"
            >
              Open page
            </button>
          </div>
        </div>
      )}

      {showKeyInput && (
        <div className="mt-3 space-y-1.5">
          {svc.key_display_name && (
            <label className="block text-xs text-text-secondary">{svc.key_display_name}</label>
          )}
          <div className="flex gap-2">
            <input
              type="password"
              value={apiKeyInput}
              onChange={e => setApiKeyInput(e.target.value)}
              onKeyDown={e => e.key === 'Enter' && handleSaveKey()}
              placeholder={svc.key_hint || 'Paste your token…'}
              className="min-w-0 flex-1 text-xs px-2 py-1.5 border border-border-default bg-surface-0 text-text-primary rounded focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand placeholder:text-text-tertiary"
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
        </div>
      )}
    </div>
  )
}

function VaultSecretActions({ item }: { item: VaultItem }) {
  const qc = useQueryClient()
  const [value, setValue] = useState('')
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)

  function refreshVaultData() {
    qc.invalidateQueries({ queryKey: ['vault-items'] })
    qc.invalidateQueries({ queryKey: ['runtime-placeholders'] })
  }

  async function handleUpdate() {
    if (!value.trim()) return
    setSaving(true)
    setError(null)
    try {
      await api.vault.updateItem(item.id, value.trim())
      setValue('')
      refreshVaultData()
    } catch (e: any) {
      setError(e.message ?? 'Failed to update vault item')
    } finally {
      setSaving(false)
    }
  }

  async function handleDelete() {
    setError(null)
    if (!confirm(`Delete ${item.name}? Existing placeholders for this item will stop resolving.`)) return
    try {
      await api.vault.deleteItem(item.id)
      refreshVaultData()
    } catch (e: any) {
      setError(e.message ?? 'Failed to delete vault item')
    }
  }

  return (
    <div className="rounded border border-border-subtle bg-surface-1 px-3 py-3">
      <div className="flex gap-2">
        <input
          type="password"
          value={value}
          onChange={e => setValue(e.target.value)}
          onKeyDown={e => e.key === 'Enter' && handleUpdate()}
          placeholder="New value"
          className="min-w-0 flex-1 text-xs px-2 py-1.5 border border-border-default bg-surface-0 text-text-primary rounded focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand placeholder:text-text-tertiary"
        />
        <button
          onClick={handleUpdate}
          disabled={saving || !value.trim()}
          className="text-xs px-3 py-1.5 rounded bg-brand text-surface-0 hover:bg-brand-strong disabled:opacity-50"
        >
          {saving ? 'Saving…' : 'Update'}
        </button>
        <button
          onClick={handleDelete}
          className="text-xs px-2.5 py-1 rounded text-danger border border-danger/20 hover:bg-danger/10"
        >
          Delete
        </button>
      </div>
      {error && <p className="mt-2 text-xs text-danger">{error}</p>}
    </div>
  )
}

function isPlaceholderActive(entry: RuntimePlaceholder) {
  return !entry.revoked_at && !isPlaceholderExpired(entry)
}

function isPlaceholderExpired(entry: RuntimePlaceholder) {
  return !!entry.expires_at && new Date(entry.expires_at).getTime() <= Date.now()
}

function placeholderStatus(entry: RuntimePlaceholder) {
  if (entry.revoked_at) return 'Revoked'
  if (isPlaceholderExpired(entry)) return 'Expired'
  return 'Live'
}

function placeholderStatusClass(entry: RuntimePlaceholder) {
  if (entry.revoked_at) return 'bg-danger/10 text-danger'
  if (isPlaceholderExpired(entry)) return 'bg-yellow-500/10 text-yellow-700'
  return 'bg-success/10 text-success'
}

function VaultMetric({ label, value }: { label: string; value: string }) {
  return (
    <div className="rounded border border-border-subtle bg-surface-0 p-3">
      <div className="text-xs uppercase tracking-wider text-text-tertiary">{label}</div>
      <div className="mt-1 text-sm font-medium text-text-primary">{value}</div>
    </div>
  )
}

// ── Active Service Row ───────────────────────────────────────────────────────

function ActiveServiceRow({ svc }: { svc: ServiceInfo }) {
  const qc = useQueryClient()
  const [apiKeyInput, setApiKeyInput] = useState('')
  const [showKeyInput, setShowKeyInput] = useState(false)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [deviceCode, setDeviceCode] = useState<{ userCode: string; verificationUri: string } | null>(null)
  const [renaming, setRenaming] = useState(false)
  const [renameValue, setRenameValue] = useState('')
  const pollRef = useRef<ReturnType<typeof setTimeout> | null>(null)

  useEffect(() => () => { if (pollRef.current) clearTimeout(pollRef.current) }, [])

  const alias = svc.alias || undefined

  async function handleReauth() {
    setError(null)
    try {
      if (svc.pkce_flow) {
        const resp = await api.services.pkceFlowStart(svc.id, alias)
        if (resp.authorize_url) {
          openOAuthUrl(resp.authorize_url)
        }
      } else if (svc.device_flow) {
        const resp = await api.services.deviceFlowStart(svc.id, alias)
        setDeviceCode({ userCode: resp.user_code, verificationUri: resp.verification_uri })
        const popup = window.open(resp.verification_uri, '_blank', 'width=600,height=700')
        if (!popup) window.open(resp.verification_uri, '_blank')
        function poll(flowId: string, interval: number) {
          pollRef.current = setTimeout(async () => {
            try {
              const r = await api.services.deviceFlowPoll(svc.id, flowId)
              if (r.status === 'complete') {
                setDeviceCode(null)
                qc.invalidateQueries({ queryKey: ['services'] })
              } else if (r.status === 'pending' || r.status === 'slow_down') {
                poll(flowId, r.interval ?? interval)
              } else {
                setDeviceCode(null)
                setError(r.status === 'denied' ? 'Authorization denied.' : 'Authorization expired.')
              }
            } catch (e) {
              console.error('Services: device flow poll failed', e)
              setDeviceCode(null)
              setError('Failed to check authorization status')
            }
          }, interval * 1000)
        }
        poll(resp.flow_id, resp.interval)
      } else {
        const resp = await api.services.oauthGetUrl(svc.id, undefined, alias)
        if (resp.already_authorized) {
          qc.invalidateQueries({ queryKey: ['services'] })
          return
        }
        if (resp.url) {
          openOAuthUrl(resp.url)
        }
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
    setError(null)
    try {
      const { affected_task_count } = await api.services.deactivatePreflight(svc.id, alias)
      const name = serviceName(svc.id, svc.alias)
      const taskWarning = affected_task_count > 0
        ? `\n\nThis will revoke ${affected_task_count} active task${affected_task_count === 1 ? '' : 's'} that use${affected_task_count === 1 ? 's' : ''} this service.`
        : ''
      if (!confirm(`Deactivate ${name}? Your agents will lose access.${taskWarning}`)) return
      await api.services.deactivate(svc.id, alias)
      qc.invalidateQueries({ queryKey: ['services'] })
    } catch (e: any) {
      setError(e.message ?? 'Failed to deactivate service')
    }
  }

  async function handleRename(overrideAlias?: string) {
    const newAlias = (overrideAlias ?? renameValue).trim() || 'default'
    const oldAlias = alias || 'default'
    if (newAlias === oldAlias) { setRenaming(false); return }
    setError(null)
    setSaving(true)
    try {
      await api.services.renameAlias(svc.id, oldAlias, newAlias)
      setRenaming(false)
      setRenameValue('')
      qc.invalidateQueries({ queryKey: ['services'] })
    } catch (e: any) {
      setError(e.message ?? 'Failed to rename')
    } finally {
      setSaving(false)
    }
  }

  const desc = serviceDescription(svc.id)

  return (
    <div className="group">
      <div className="flex flex-col sm:flex-row sm:items-center gap-3 px-5 py-4">
        <div className="flex items-center gap-3 min-w-0 flex-1">
          {/* Icon */}
          <ServiceIconBadge iconSvg={svc.icon_svg} iconUrl={svc.icon_url} serviceId={svc.id} size={28} />

          {/* Name + description */}
          <div className="min-w-0 flex-1">
            <div className="flex items-center gap-2">
              <span className="font-medium text-text-primary text-sm truncate">{serviceName(svc.id, svc.alias)}</span>
              <span className="w-1.5 h-1.5 rounded-full bg-success shrink-0" title="Connected" />
              <button
                onClick={() => { setRenaming(true); setRenameValue(svc.alias && svc.alias !== 'default' ? svc.alias : '') }}
                className="text-xs text-text-tertiary hover:text-text-secondary opacity-0 group-hover:opacity-100"
              >
                rename
              </button>
            </div>
            {desc && <p className="text-xs text-text-tertiary mt-0.5">{desc}</p>}
          </div>
        </div>

        {/* Actions + connected time */}
        <div className="shrink-0 flex flex-col items-end gap-1 ml-auto sm:ml-0">
          {svc.requires_activation !== false && (
            <div className="flex gap-1.5">
              {!svc.credential_free && (svc.oauth || svc.pkce_flow || svc.device_flow ? (
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
                className="text-xs px-2.5 py-1 rounded text-danger border border-danger/20 hover:bg-danger/10"
              >
                Disconnect
              </button>
            </div>
          )}
          {svc.activated_at && (
            <span className="text-xs text-text-tertiary hidden sm:block whitespace-nowrap">
              Connected {formatDistanceToNow(new Date(svc.activated_at), { addSuffix: true })}
            </span>
          )}
        </div>
      </div>

      {error && <p className="text-xs text-danger px-5 pb-3">{error}</p>}

      {renaming && (
        <div className="px-5 pb-3 ml-16 flex items-center gap-2">
          <input
            type="text"
            value={renameValue}
            onChange={e => setRenameValue(e.target.value)}
            onKeyDown={e => { if (e.key === 'Enter') handleRename(); if (e.key === 'Escape') { setRenaming(false); setError(null) } }}
            className="text-xs px-2 py-1 rounded border border-border-default bg-surface-0 text-text-primary w-48"
            placeholder="New label"
            autoFocus
          />
          <button
            onClick={() => handleRename()}
            disabled={saving}
            className="text-xs px-2.5 py-1 rounded border border-border-strong text-text-primary hover:bg-surface-2 disabled:opacity-50"
          >
            {saving ? 'Saving...' : 'Save'}
          </button>
          {svc.alias && svc.alias !== 'default' && (
            <button
              onClick={() => handleRename('default')}
              disabled={saving}
              className="text-xs px-2 py-1 text-danger hover:text-danger/80 disabled:opacity-50"
            >
              Clear label
            </button>
          )}
          <button
            onClick={() => { setRenaming(false); setError(null) }}
            className="text-xs px-2 py-1 text-text-tertiary hover:text-text-secondary"
          >
            Cancel
          </button>
        </div>
      )}

      {deviceCode && (
        <div className="px-5 pb-4 space-y-1.5 ml-16">
          <p className="text-xs text-text-secondary">Enter this code on the authorization page:</p>
          <div className="flex items-center gap-2">
            <code className="text-sm font-mono font-bold tracking-widest text-text-primary bg-surface-0 px-3 py-1.5 rounded border border-border-default select-all">
              {deviceCode.userCode}
            </code>
            <button
              onClick={() => navigator.clipboard.writeText(deviceCode.userCode)}
              className="text-xs px-2 py-1 rounded border border-border-strong text-text-primary hover:bg-surface-2"
            >
              Copy
            </button>
            <button
              onClick={() => window.open(deviceCode.verificationUri, '_blank')}
              className="text-xs px-2 py-1 rounded border border-border-strong text-text-primary hover:bg-surface-2"
            >
              Open page
            </button>
          </div>
          <p className="text-xs text-text-tertiary">Waiting for authorization&hellip;</p>
        </div>
      )}

      {showKeyInput && (
        <div className="px-5 pb-4 ml-16 space-y-1.5">
          {svc.key_display_name && (
            <label className="block text-xs text-text-secondary">
              {svc.key_display_name}
              <span className="text-danger ml-0.5">*</span>
            </label>
          )}
          {svc.key_description && (
            <p className="text-[10px] text-text-tertiary whitespace-pre-line">{svc.key_description}</p>
          )}
          <div className="flex gap-2">
            <input
              type="password"
              value={apiKeyInput}
              onChange={e => setApiKeyInput(e.target.value)}
              onKeyDown={e => e.key === 'Enter' && handleSaveKey()}
              placeholder={svc.key_hint || "Paste your token…"}
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
        </div>
      )}
    </div>
  )
}

// ── Add Service Modal ────────────────────────────────────────────────────────

interface ServiceType {
  baseId: string
  iconSvg?: string
  iconUrl?: string
  oauth: boolean
  deviceFlow: boolean
  pkceFlow: boolean
  pkceClientIdRequired: boolean
  autoIdentity: boolean
  requiresActivation: boolean
  credentialFree: boolean
  actions: ServiceActionInfo[]
  variables?: VariableMeta[]
  activatedCount: number
  description: string
  setupUrl?: string
  keyHint?: string
  keyDisplayName?: string
  keyDescription?: string
}

function AddServiceModal({
  services,
  onClose,
  onSuccess,
  googleOAuthMissing,
  microsoftOAuthMissing,
  activeConnectionCount,
}: {
  services: ServiceInfo[]
  onClose: () => void
  onSuccess: (serviceId: string) => void
  googleOAuthMissing: boolean
  microsoftOAuthMissing: boolean
  activeConnectionCount: number
}) {
  const qc = useQueryClient()
  const [aliasInputFor, setAliasInputFor] = useState<string | null>(null)
  const [aliasValue, setAliasValue] = useState('')
  const [keyInputFor, setKeyInputFor] = useState<string | null>(null)
  const [keyValue, setKeyValue] = useState('')
  const [keyAlias, setKeyAlias] = useState<string | undefined>(undefined)
  const [keyConfig, setKeyConfig] = useState<Record<string, string>>({})
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [activatingServiceId, setActivatingServiceId] = useState<string | null>(null)

  // Billing: connection limit awareness (only when billing is enabled)
  const { features: modalFeatures } = useAuth()
  const { data: billingStatus } = useQuery({
    queryKey: ['billing-status'],
    queryFn: () => api.billing.status(),
    enabled: !!modalFeatures?.billing,
    staleTime: 60_000,
  })
  const connectionLimit = billingStatus?.usage?.connections?.limit ?? -1
  const atConnectionLimit = connectionLimit >= 0 && activeConnectionCount >= connectionLimit

  // Search filter
  const [search, setSearch] = useState('')

  // PKCE client ID state (for services that need a client ID configured)
  const [pkceClientIdFor, setPkceClientIdFor] = useState<string | null>(null)
  const [pkceClientIdValue, setPkceClientIdValue] = useState('')
  const [pkceClientIdAlias, setPkceClientIdAlias] = useState<string | undefined>(undefined)

  // Device flow state
  const [deviceFlowFor, setDeviceFlowFor] = useState<string | null>(null)
  const [deviceFlowData, setDeviceFlowData] = useState<{
    flowId: string
    userCode: string
    verificationUri: string
    interval: number
  } | null>(null)
  const [deviceFlowStatus, setDeviceFlowStatus] = useState<string>('pending')
  const pollTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null)

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
        onSuccess(activatingServiceId ?? '')
      }
    }
    window.addEventListener('message', handler)
    return () => window.removeEventListener('message', handler)
  }, [qc, onSuccess, activatingServiceId])

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
        iconSvg: svc.icon_svg,
        iconUrl: svc.icon_url,
        oauth: svc.oauth,
        deviceFlow: svc.device_flow ?? false,
        pkceFlow: svc.pkce_flow ?? false,
        pkceClientIdRequired: svc.pkce_client_id_required ?? false,
        autoIdentity: svc.auto_identity ?? false,
        requiresActivation: svc.requires_activation ?? true,
        credentialFree: svc.credential_free ?? false,
        actions: svc.actions,
        variables: svc.variables,
        activatedCount: svc.status === 'activated' ? 1 : 0,
        description: svc.description || serviceDescription(svc.id),
        setupUrl: svc.setup_url,
        keyHint: svc.key_hint,
        keyDisplayName: svc.key_display_name,
        keyDescription: svc.key_description,
      })
    }
  }
  const allServiceTypes = Array.from(typeMap.values())
  const searchLower = search.toLowerCase().trim()
  const serviceTypes = searchLower
    ? allServiceTypes.filter(st =>
        serviceName(st.baseId).toLowerCase().includes(searchLower) ||
        st.baseId.toLowerCase().includes(searchLower) ||
        st.description.toLowerCase().includes(searchLower)
      )
    : allServiceTypes

  async function handleActivateOAuth(serviceId: string, alias?: string, newAccount?: boolean) {
    setError(null)
    setActivatingServiceId(serviceId)
    try {
      const resp = await api.services.oauthGetUrl(serviceId, undefined, alias, newAccount)
      if (resp.already_authorized) {
        qc.invalidateQueries({ queryKey: ['services'] })
        onSuccess(serviceId)
        return
      }
      if (resp.url) {
        openOAuthUrl(resp.url)
      }
    } catch (e: any) {
      setError(e.message ?? 'Failed to start OAuth flow')
    }
  }

  async function handleSaveKey() {
    if (!keyValue.trim() || !keyInputFor) return
    setSaving(true)
    setError(null)
    const serviceId = keyInputFor
    try {
      await api.services.activateWithKey(serviceId, keyValue.trim(), keyAlias, Object.keys(keyConfig).length > 0 ? keyConfig : undefined)
      setKeyValue('')
      setKeyInputFor(null)
      setKeyAlias(undefined)
      setKeyConfig({})
      qc.invalidateQueries({ queryKey: ['services'] })
      onSuccess(serviceId)
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
      onSuccess(serviceId)
    } catch (e: any) {
      setError(e.message ?? 'Failed to activate service')
    }
  }

  async function handleActivatePKCE(serviceId: string, alias?: string, clientId?: string) {
    setError(null)
    setActivatingServiceId(serviceId)

    try {
      const resp = await api.services.pkceFlowStart(serviceId, alias, clientId)
      if (resp.authorize_url) {
        openOAuthUrl(resp.authorize_url)
      }
    } catch (e: any) {
      setError(e.message ?? 'Failed to start authorization')
    }
  }

  const stopDeviceFlowPolling = useCallback(() => {
    if (pollTimerRef.current) {
      clearTimeout(pollTimerRef.current)
      pollTimerRef.current = null
    }
  }, [])

  // Cleanup polling on unmount
  useEffect(() => stopDeviceFlowPolling, [stopDeviceFlowPolling])

  async function handleActivateDeviceFlow(serviceId: string, alias?: string) {
    setError(null)
    setActivatingServiceId(serviceId)

    stopDeviceFlowPolling()
    try {
      const resp = await api.services.deviceFlowStart(serviceId, alias)
      setDeviceFlowFor(serviceId)
      setDeviceFlowData({
        flowId: resp.flow_id,
        userCode: resp.user_code,
        verificationUri: resp.verification_uri,
        interval: resp.interval,
      })
      setDeviceFlowStatus('pending')
      const popup = window.open(resp.verification_uri, '_blank', 'width=600,height=700')
      if (!popup) window.open(resp.verification_uri, '_blank')
      startDeviceFlowPolling(serviceId, resp.flow_id, resp.interval)
    } catch (e: any) {
      setError(e.message ?? 'Failed to start device authorization')
    }
  }

  function startDeviceFlowPolling(serviceId: string, flowId: string, interval: number) {
    pollTimerRef.current = setTimeout(async () => {
      try {
        const resp = await api.services.deviceFlowPoll(serviceId, flowId)
        if (resp.status === 'complete') {
          setDeviceFlowStatus('complete')
          stopDeviceFlowPolling()
          qc.invalidateQueries({ queryKey: ['services'] })
          onSuccess(serviceId)
          return
        }
        if (resp.status === 'pending' || resp.status === 'slow_down') {
          setDeviceFlowStatus('pending')
          const nextInterval = resp.interval ?? interval
          startDeviceFlowPolling(serviceId, flowId, nextInterval)
          return
        }
        setDeviceFlowStatus(resp.status)
        stopDeviceFlowPolling()
        setError(resp.status === 'denied' ? 'Authorization was denied.' : 'Authorization expired. Please try again.')
      } catch (e: any) {
        setDeviceFlowStatus('error')
        stopDeviceFlowPolling()
        setError(e.message ?? 'Failed to check authorization status')
      }
    }, interval * 1000)
  }

  // For the first activation, skip the alias prompt and go straight to auth.
  // For auto-identity services, always skip — the backend resolves the alias.
  // Only show alias prompt when adding a second+ account without auto-identity.
  function handleActivate(st: ServiceType) {
    setError(null)
    if (st.credentialFree) {
      handleActivateCredentialFree(st.baseId)
      return
    }
    // If already has one account and the service can't auto-detect identity,
    // prompt for a label to distinguish accounts. OAuth/PKCE/device-flow
    // services always support auto-identity because the backend resolves
    // the account identity from the credential at activation time.
    const canAutoIdentify = st.autoIdentity || st.oauth || st.pkceFlow || st.deviceFlow
    if (st.activatedCount > 0 && !canAutoIdentify) {
      setKeyInputFor(null)
      setKeyValue('')
      setDeviceFlowFor(null)
      setDeviceFlowData(null)
      setAliasInputFor(st.baseId)
      setAliasValue('')
      return
    }
    // First activation or auto-identity — go directly to auth.
    startAuth(st)
  }

  function startAuth(st: ServiceType, alias?: string) {
    const addingAccount = st.activatedCount > 0
    if (st.oauth) {
      handleActivateOAuth(st.baseId, alias, addingAccount)
    } else if (st.pkceFlow) {
      if (st.pkceClientIdRequired) {
        // Need client ID first — show inline input.
        setPkceClientIdFor(st.baseId)
        setPkceClientIdValue('')
        setPkceClientIdAlias(alias)
        return
      }
      handleActivatePKCE(st.baseId, alias)
    } else if (st.deviceFlow) {
      handleActivateDeviceFlow(st.baseId, alias)
    } else {
      setKeyInputFor(st.baseId)
      setKeyAlias(alias)
      // Initialize config with variable defaults
      const defaults: Record<string, string> = {}
      if (st.variables) {
        for (const v of st.variables) {
          defaults[v.name] = v.default ?? ''
        }
      }
      setKeyConfig(defaults)
    }
  }

  function handleSubmitPKCEClientId(st: ServiceType) {
    const clientId = pkceClientIdValue.trim()
    if (!clientId) return
    setPkceClientIdFor(null)
    handleActivatePKCE(st.baseId, pkceClientIdAlias, clientId)
  }

  const confirmAlias = (st: ServiceType) => {
    const alias = aliasValue.trim() || undefined
    setAliasInputFor(null)
    setError(null)
    startAuth(st, alias)
  }

  const isGoogleService = (id: string) => id.startsWith('google.')
  const isMicrosoftService = (id: string) => id.startsWith('microsoft.')

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center">
      {/* Backdrop */}
      <div className="absolute inset-0 bg-black/60" onClick={onClose} />

      {/* Modal */}
      <div className="relative bg-surface-1 border border-border-default rounded-lg w-full max-w-2xl mx-4 max-h-[80vh] flex flex-col shadow-xl">
        <div className="flex items-center justify-between px-6 py-4 border-b border-border-default">
          <h2 className="text-lg font-semibold text-text-primary">Connect a service</h2>
          <button
            onClick={onClose}
            className="text-text-tertiary hover:text-text-primary text-xl leading-none"
          >
            &times;
          </button>
        </div>

        <div className="px-6 pt-4 pb-2">
          <input
            type="text"
            value={search}
            onChange={e => setSearch(e.target.value)}
            placeholder="Search services..."
            className="w-full text-sm px-3 py-2 border border-border-default bg-surface-0 text-text-primary rounded-lg focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand placeholder:text-text-tertiary"
            autoFocus
          />
        </div>

        <div className="px-6 py-3 overflow-y-auto">
          {atConnectionLimit && (
            <div className="mb-3 px-3 py-2.5 rounded-md bg-warning/10 border border-warning/30 text-sm">
              <span className="font-medium text-text-primary">Connection limit reached</span>
              <span className="text-text-secondary"> ({activeConnectionCount}/{connectionLimit}). </span>
              <a href="/pricing" className="text-brand hover:text-brand/80 font-medium">Upgrade your plan</a>
              <span className="text-text-secondary"> for more connections.</span>
            </div>
          )}
          {error && <p className="text-xs text-danger mb-3">{error}</p>}

          <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
            {serviceTypes.map(st => {
              const isActivated = st.activatedCount > 0
              const isGoogleBlocked = googleOAuthMissing && isGoogleService(st.baseId)
              const hasInlineUI = aliasInputFor === st.baseId || keyInputFor === st.baseId || pkceClientIdFor === st.baseId || (deviceFlowFor === st.baseId && deviceFlowData)
              return (
                <div
                  key={st.baseId}
                  className={`rounded-xl border border-border-default bg-surface-0/50 p-5 flex flex-col items-center text-center transition-all ${hasInlineUI ? '' : 'hover:border-brand/50 hover:shadow-md hover:shadow-brand/5'}`}
                >
                  {/* Icon with optional connected indicator */}
                  <div className="relative mb-3">
                    <ServiceIconBadge iconSvg={st.iconSvg} iconUrl={st.iconUrl} serviceId={st.baseId} size={32} />
                    {isActivated && (
                      <span
                        className="absolute -top-0.5 -right-0.5 w-3.5 h-3.5 rounded-full bg-success border-2 border-surface-1"
                        title="Connected"
                      />
                    )}
                  </div>

                  {/* Name */}
                  <span className="font-semibold text-text-primary text-sm">{serviceName(st.baseId)}</span>

                  {/* Description */}
                  <p className="text-xs text-text-tertiary mt-1.5 mb-4 line-clamp-3 leading-relaxed">{st.description}</p>

                  {/* Spacer to push button to bottom */}
                  <div className="mt-auto w-full">
                    {/* Action button — consistent style for all services */}
                    {!hasInlineUI && (
                      (isGoogleBlocked || (microsoftOAuthMissing && isMicrosoftService(st.baseId))) ? (
                        <a
                          href="/dashboard/settings"
                          className="block w-full text-xs px-3 py-2 rounded-lg border border-border-strong text-text-tertiary hover:text-text-primary hover:bg-surface-2 text-center transition-colors"
                        >
                          Set up OAuth
                        </a>
                      ) : isActivated && st.credentialFree ? (
                        <span className="block w-full text-xs px-3 py-2 rounded-lg border border-border-subtle text-text-tertiary text-center">
                          Connected
                        </span>
                      ) : (
                        <button
                          onClick={() => handleActivate(st)}
                          disabled={atConnectionLimit}
                          className={`w-full text-xs px-3 py-2 rounded-lg border transition-colors ${
                            atConnectionLimit
                              ? 'border-border-subtle text-text-tertiary cursor-not-allowed'
                              : 'border-border-strong text-text-primary hover:bg-surface-2 hover:border-brand/40'
                          }`}
                          title={atConnectionLimit ? `Connection limit reached (${activeConnectionCount}/${connectionLimit})` : undefined}
                        >
                          {isActivated ? 'Add account' : 'Connect'}
                        </button>
                      )
                    )}

                    {/* Label input (only for second+ account) */}
                    {aliasInputFor === st.baseId && (
                      <div className="space-y-2 text-left">
                        <p className="text-xs text-text-tertiary">Label this connection:</p>
                        <input
                          type="text"
                          value={aliasValue}
                          onChange={e => setAliasValue(e.target.value)}
                          onKeyDown={e => e.key === 'Enter' && confirmAlias(st)}
                          placeholder="e.g. personal, work"
                          className="w-full text-xs px-2.5 py-1.5 border border-border-default bg-surface-0 text-text-primary rounded-lg focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand placeholder:text-text-tertiary"
                          autoFocus
                        />
                        <div className="flex gap-2">
                          <button
                            onClick={() => confirmAlias(st)}
                            className="text-xs px-3 py-1.5 rounded-lg bg-brand text-surface-0 hover:bg-brand-strong flex-1"
                          >
                            Continue
                          </button>
                          <button
                            onClick={() => setAliasInputFor(null)}
                            className="text-xs px-3 py-1.5 rounded-lg border border-border-strong text-text-primary hover:bg-surface-2"
                          >
                            Cancel
                          </button>
                        </div>
                      </div>
                    )}

                    {/* API key input (with optional variable fields) */}
                    {keyInputFor === st.baseId && (
                      <div className="space-y-2 text-left">
                        {st.variables && st.variables.length > 0 && st.variables.map(v => (
                          <div key={v.name}>
                            <label className="block text-xs text-text-secondary mb-0.5">
                              {v.display_name || v.name}
                              {v.required && <span className="text-danger ml-0.5">*</span>}
                            </label>
                            {v.description && (
                              <p className="text-[10px] text-text-tertiary mb-1">{v.description}</p>
                            )}
                            <input
                              type="text"
                              value={keyConfig[v.name] ?? ''}
                              onChange={e => setKeyConfig(prev => ({ ...prev, [v.name]: e.target.value }))}
                              placeholder={v.default || v.display_name || v.name}
                              className="w-full text-xs px-2.5 py-1.5 border border-border-default bg-surface-0 text-text-primary rounded-lg focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand placeholder:text-text-tertiary"
                            />
                          </div>
                        ))}
                        <div>
                          {st.keyDisplayName && (
                            <label className="block text-xs text-text-secondary mb-0.5">
                              {st.keyDisplayName}
                              <span className="text-danger ml-0.5">*</span>
                            </label>
                          )}
                          {st.keyDescription && (
                            <p className="text-[10px] text-text-tertiary mb-1 whitespace-pre-line">{st.keyDescription}</p>
                          )}
                          <input
                            type="password"
                            value={keyValue}
                            onChange={e => setKeyValue(e.target.value)}
                            onKeyDown={e => e.key === 'Enter' && handleSaveKey()}
                            placeholder={st.keyHint || "Paste your token…"}
                            className="w-full text-xs px-2.5 py-1.5 border border-border-default bg-surface-0 text-text-primary rounded-lg focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand placeholder:text-text-tertiary"
                            autoFocus={!st.variables || st.variables.length === 0}
                          />
                        </div>
                        <div className="flex gap-2">
                          <button
                            onClick={handleSaveKey}
                            disabled={saving || !keyValue.trim()}
                            className="text-xs px-3 py-1.5 rounded-lg bg-brand text-surface-0 hover:bg-brand-strong disabled:opacity-50 flex-1"
                          >
                            {saving ? 'Saving…' : 'Save'}
                          </button>
                          <button
                            onClick={() => { setKeyInputFor(null); setKeyValue(''); setKeyConfig({}) }}
                            className="text-xs px-3 py-1.5 rounded-lg border border-border-strong text-text-primary hover:bg-surface-2"
                          >
                            Cancel
                          </button>
                        </div>
                      </div>
                    )}

                    {/* PKCE client ID input */}
                    {pkceClientIdFor === st.baseId && (
                      <div className="space-y-2 text-left">
                        <p className="text-xs text-text-secondary">
                          Enter your OAuth app's Client ID to connect:
                        </p>
                        <input
                          type="text"
                          value={pkceClientIdValue}
                          onChange={e => setPkceClientIdValue(e.target.value)}
                          onKeyDown={e => e.key === 'Enter' && handleSubmitPKCEClientId(st)}
                          placeholder="Client ID"
                          className="w-full text-xs px-2.5 py-1.5 border border-border-default bg-surface-0 text-text-primary rounded-lg focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand placeholder:text-text-tertiary font-mono"
                          autoFocus
                        />
                        {st.setupUrl && (
                          <p className="text-[10px] text-text-tertiary">
                            Create an OAuth app at{' '}
                            <a href={st.setupUrl} target="_blank" rel="noopener noreferrer" className="text-brand hover:underline">{st.setupUrl}</a>
                          </p>
                        )}
                        <div className="flex gap-2">
                          <button
                            onClick={() => handleSubmitPKCEClientId(st)}
                            disabled={!pkceClientIdValue.trim()}
                            className="text-xs px-3 py-1.5 rounded-lg bg-brand text-surface-0 hover:bg-brand-strong disabled:opacity-50 flex-1"
                          >
                            Connect
                          </button>
                          <button
                            onClick={() => { setPkceClientIdFor(null); setPkceClientIdValue('') }}
                            className="text-xs px-3 py-1.5 rounded-lg border border-border-strong text-text-primary hover:bg-surface-2"
                          >
                            Cancel
                          </button>
                        </div>
                      </div>
                    )}

                    {/* Device flow status */}
                    {deviceFlowFor === st.baseId && deviceFlowData && (
                      <div className="space-y-2 text-left">
                        <p className="text-xs text-text-secondary">
                          Enter this code on the authorization page:
                        </p>
                        <div className="flex items-center gap-2">
                          <code className="text-sm font-mono font-bold tracking-widest text-text-primary bg-surface-0 px-3 py-1.5 rounded-lg border border-border-default select-all">
                            {deviceFlowData.userCode}
                          </code>
                          <button
                            onClick={() => navigator.clipboard.writeText(deviceFlowData.userCode)}
                            className="text-xs px-2 py-1 rounded-lg border border-border-strong text-text-primary hover:bg-surface-2"
                          >
                            Copy
                          </button>
                        </div>
                        {deviceFlowStatus === 'pending' && (
                          <p className="text-xs text-text-tertiary">Waiting for authorization&hellip;</p>
                        )}
                        <div className="flex gap-2">
                          <button
                            onClick={() => window.open(deviceFlowData.verificationUri, '_blank')}
                            className="text-xs px-3 py-1.5 rounded-lg border border-border-strong text-text-primary hover:bg-surface-2"
                          >
                            Open page
                          </button>
                          <button
                            onClick={() => { stopDeviceFlowPolling(); setDeviceFlowFor(null); setDeviceFlowData(null) }}
                            className="text-xs px-3 py-1.5 rounded-lg border border-border-strong text-text-primary hover:bg-surface-2"
                          >
                            Cancel
                          </button>
                        </div>
                      </div>
                    )}
                  </div>
                </div>
              )
            })}
          </div>

          {serviceTypes.length === 0 && (
            <p className="text-sm text-text-tertiary py-4">No services available.</p>
          )}
        </div>
      </div>
    </div>
  )
}

// ── Org Services View ─────────────────────────────────────────────────────────

function OrgServicesView({ orgId, orgName }: { orgId: string; orgName: string }) {
  const { data } = useQuery({
    queryKey: ['org-services', orgId],
    queryFn: () => api.orgs.services(orgId),
    enabled: !!orgId,
  })

  const services = data?.services ?? []

  return (
    <div className="p-4 sm:p-8 space-y-6">
      <div>
        <h1 className="text-2xl font-bold text-text-primary">{orgName} Accounts</h1>
        <p className="text-sm text-text-tertiary mt-1">
          Org-wide shared credentials and per-user service activation.
        </p>
      </div>

      <div className="space-y-2">
        {services.map((s) => (
          <div key={s.service_id} className="bg-surface-1 rounded-lg border border-border-default p-4 flex items-center justify-between">
            <div>
              <span className="text-sm font-medium text-text-primary">{s.name}</span>
              <span className="ml-2 text-xs text-text-secondary font-mono">{s.service_id}</span>
            </div>
            <div className="flex items-center gap-2">
              <span className={`text-xs px-1.5 py-0.5 rounded ${
                s.status === 'active' ? 'bg-success/15 text-success' : 'bg-surface-2 text-text-tertiary'
              }`}>
                {s.status}
              </span>
              <span className="text-xs text-text-tertiary">{s.credential_type}</span>
            </div>
          </div>
        ))}
        {services.length === 0 && (
          <div className="text-sm text-text-tertiary py-8 text-center">
            No services configured for this organization.
          </div>
        )}
      </div>
    </div>
  )
}

// ── Local Services Section ────────────────────────────────────────────────────

function LocalServicesSection() {
  const qc = useQueryClient()

  const { data: daemons } = useQuery({
    queryKey: ['local-daemons'],
    queryFn: () => api.localDaemon.list(),
  })

  const { data: enabledServices } = useQuery({
    queryKey: ['enabled-local-services'],
    queryFn: () => api.localDaemon.listEnabledServices(),
  })

  // Fetch capabilities for each connected daemon.
  const connectedDaemons = (daemons ?? []).filter(d => d.connected)
  const daemonCaps = useQuery({
    queryKey: ['all-daemon-caps', connectedDaemons.map(d => d.id).join(',')],
    queryFn: async () => {
      const results: Record<string, LocalService[]> = {}
      for (const d of connectedDaemons) {
        try {
          const caps = await api.localDaemon.services(d.id)
          results[d.id] = caps.services
        } catch { /* daemon may have disconnected */ }
      }
      return results
    },
    enabled: connectedDaemons.length > 0,
    staleTime: 30000,
  })

  const enableMut = useMutation({
    mutationFn: ({ daemonId, serviceId }: { daemonId: string; serviceId: string }) =>
      api.localDaemon.enableService(daemonId, serviceId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['enabled-local-services'] })
    },
  })

  const disableMut = useMutation({
    mutationFn: ({ daemonId, serviceId }: { daemonId: string; serviceId: string }) =>
      api.localDaemon.disableService(daemonId, serviceId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['enabled-local-services'] })
    },
  })

  if (!daemons || daemons.length === 0) return null

  const enabledByDaemon = new Map<string, Set<string>>()
  for (const s of enabledServices ?? []) {
    if (!enabledByDaemon.has(s.daemon_id)) enabledByDaemon.set(s.daemon_id, new Set())
    enabledByDaemon.get(s.daemon_id)!.add(s.service_id)
  }

  const caps = daemonCaps.data ?? {}
  const hasAnyServices = Object.values(caps).some(svcs => svcs.length > 0)

  if (!hasAnyServices && connectedDaemons.length === 0) return null

  return (
    <div className="space-y-3">
      <div>
        <h2 className="text-lg font-semibold text-text-primary">Local Services</h2>
        <p className="text-xs text-text-tertiary mt-0.5">
          Services from your paired local computers. Enable services to make them available to your agents.
        </p>
      </div>

      {daemons.map(daemon => {
        const daemonServices = caps[daemon.id] ?? []
        if (!daemon.connected && !enabledByDaemon.has(daemon.id)) return null

        return (
          <div key={daemon.id} className="bg-surface-1 border border-border-default rounded-lg overflow-hidden">
            <div className="px-5 py-3 flex items-center gap-2 border-b border-border-subtle">
              <span className={`inline-block w-2 h-2 rounded-full ${daemon.connected ? 'bg-success' : 'bg-text-tertiary'}`} />
              <span className="font-medium text-text-primary text-sm">{daemon.name || 'Local Computer'}</span>
              {!daemon.connected && (
                <span className="text-xs text-text-tertiary">(offline)</span>
              )}
            </div>

            {!daemon.connected && (
              <div className="px-5 py-4 text-sm text-text-tertiary">
                Daemon is offline. Enabled services will become available when it reconnects.
              </div>
            )}

            {daemon.connected && daemonServices.length === 0 && (
              <div className="px-5 py-4 text-sm text-text-tertiary">
                No services reported by this daemon.
              </div>
            )}

            {daemonServices.length > 0 && (
              <div className="divide-y divide-border-subtle">
                {daemonServices.map(svc => {
                  const daemonEnabled = enabledByDaemon.get(daemon.id)
                  const enabled = daemonEnabled?.has(svc.id) ?? false
                  const mutKey = `${daemon.id}:${svc.id}`
                  const toggling =
                    (enableMut.isPending && enableMut.variables?.daemonId === daemon.id && enableMut.variables?.serviceId === svc.id) ||
                    (disableMut.isPending && disableMut.variables?.daemonId === daemon.id && disableMut.variables?.serviceId === svc.id)

                  return (
                    <div key={mutKey} className="px-5 py-3 flex items-center justify-between">
                      <div className="min-w-0">
                        <div className="flex items-center gap-2">
                          <span className="text-sm font-medium text-text-primary">{svc.name}</span>
                          <span className="text-xs text-text-tertiary font-mono">local.{svc.id}</span>
                        </div>
                        {svc.description && (
                          <p className="text-xs text-text-tertiary mt-0.5">{svc.description}</p>
                        )}
                        <div className="flex flex-wrap gap-1 mt-1">
                          {svc.actions.map(act => (
                            <span key={act.id} className="text-xs px-1.5 py-0.5 rounded bg-surface-2 text-text-secondary">
                              {act.name}
                            </span>
                          ))}
                        </div>
                      </div>
                      <button
                        disabled={toggling}
                        onClick={() => {
                          if (enabled) {
                            disableMut.mutate({ daemonId: daemon.id, serviceId: svc.id })
                          } else {
                            enableMut.mutate({ daemonId: daemon.id, serviceId: svc.id })
                          }
                        }}
                        className={`ml-4 shrink-0 text-xs px-3 py-1.5 rounded border font-medium transition-colors ${
                          enabled
                            ? 'bg-success/10 text-success border-success/20 hover:bg-success/20'
                            : 'bg-surface-2 text-text-secondary border-border-default hover:bg-surface-3'
                        }`}
                      >
                        {enabled ? 'Enabled' : 'Enable'}
                      </button>
                    </div>
                  )
                })}
              </div>
            )}

            {(enableMut.isError || disableMut.isError) && (
              <div className="px-5 py-2 text-xs text-danger bg-danger/5 border-t border-border-subtle">
                {enableMut.isError
                  ? `Failed to enable service: ${(enableMut.error as Error).message}`
                  : `Failed to disable service: ${(disableMut.error as Error).message}`}
              </div>
            )}
          </div>
        )
      })}
    </div>
  )
}

// ── Main Page ────────────────────────────────────────────────────────────────

export default function Services() {
  const qc = useQueryClient()
  const { features, currentOrg } = useAuth()
  const orgId = currentOrg?.id
  const secretVaultUI = !orgId && !!features?.secret_vault
  const [showModal, setShowModal] = useState(false)
  const [successService, setSuccessService] = useState<string | null>(null)
  const successTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null)

  function handleConnectionSuccess(serviceId: string) {
    setShowModal(false)
    setSuccessService(serviceId)
    if (successTimerRef.current) clearTimeout(successTimerRef.current)
    successTimerRef.current = setTimeout(() => setSuccessService(null), 5000)
  }

  useEffect(() => () => { if (successTimerRef.current) clearTimeout(successTimerRef.current) }, [])

  const { data, isLoading, error } = useQuery({
    queryKey: ['services'],
    queryFn: () => api.services.list(),
    enabled: !orgId,
  })

  // In single-tenant mode, check if Google/Microsoft OAuth credentials are configured.
  const { data: googleOAuth } = useQuery({
    queryKey: ['google-oauth-status'],
    queryFn: () => api.system.getGoogleOAuth(),
    enabled: !features?.multi_tenant,
  })
  const { data: microsoftOAuth } = useQuery({
    queryKey: ['microsoft-oauth-status'],
    queryFn: () => api.system.getMicrosoftOAuth(),
    enabled: !features?.multi_tenant,
  })
  const { data: agents = [] } = useQuery({
    queryKey: ['agents'],
    queryFn: () => api.agents.list(),
    enabled: !orgId,
  })
  const { data: runtimePlaceholders } = useQuery({
    queryKey: ['runtime-placeholders'],
    queryFn: () => api.runtime.listPlaceholders(),
    enabled: secretVaultUI,
    refetchInterval: 30_000,
  })
  const { data: vaultItems, error: vaultItemsError } = useQuery({
    queryKey: ['vault-items'],
    queryFn: () => api.vault.listItems(),
    enabled: secretVaultUI,
    refetchInterval: 30_000,
  })
  // Refresh when the OAuth popup signals completion (for cases where modal isn't open).
  useEffect(() => {
    function handler(e: MessageEvent) {
      if (e.data?.type === 'clawvisor_oauth_done') {
        qc.invalidateQueries({ queryKey: ['services'] })
        qc.invalidateQueries({ queryKey: ['vault-items'] })
      }
    }
    window.addEventListener('message', handler)
    return () => window.removeEventListener('message', handler)
  }, [qc])

  if (orgId) {
    return <OrgServicesView orgId={orgId} orgName={currentOrg!.name} />
  }

  const allServices = data?.services ?? []
  const activeServices = allServices.filter(s => s.status === 'activated')
  const hasGoogleServices = allServices.some(s => s.id.startsWith('google.'))
  const googleOAuthMissing = !features?.multi_tenant && hasGoogleServices && googleOAuth != null && !googleOAuth.configured

  const hasMicrosoftServices = allServices.some(s => s.id.startsWith('microsoft.'))
  const microsoftOAuthMissing = !features?.multi_tenant && hasMicrosoftServices && microsoftOAuth != null && !microsoftOAuth.configured
  const useUnifiedVault = secretVaultUI && !vaultItemsError

  return (
    <div className="p-4 sm:p-8 space-y-6">
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-4">
        <div>
          <h1 className="text-2xl font-bold text-text-primary">Accounts</h1>
          <p className="text-sm text-text-tertiary mt-1">
            {activeServices.length > 0
              ? `${activeServices.length} connected account${activeServices.length !== 1 ? 's' : ''}`
              : 'Connect accounts so your agents can take actions.'}
          </p>
          <p className="text-xs text-text-tertiary mt-2 max-w-xl">
            Your credentials are only used when your AI agent takes actions on your behalf.
            Clawvisor itself never accesses your accounts beyond fetching basic info
            (like your name or email) to label connections.
          </p>
        </div>
        <div className="flex items-center gap-3">
          {features?.adapter_gen && (
            <NavLink
              to="/dashboard/adapter-gen"
              className="px-4 py-2 rounded-md border border-border-strong text-text-primary text-sm font-medium hover:bg-surface-2 transition-colors"
            >
              Generate integration
            </NavLink>
          )}
          <button
            onClick={() => setShowModal(true)}
            className="px-4 py-2 rounded-md bg-brand text-surface-0 text-sm font-medium hover:bg-brand-strong shadow-sm"
          >
            Connect service
          </button>
        </div>
      </div>

      {successService && (
        <div className="flex items-center gap-3 p-4 rounded-md border border-success/30 bg-success/5">
          <span className="text-success text-lg leading-none">&#10003;</span>
          <p className="text-sm font-medium text-text-primary">
            {serviceName(successService)} connected successfully
          </p>
          <button
            onClick={() => setSuccessService(null)}
            className="ml-auto text-text-tertiary hover:text-text-primary text-lg leading-none"
          >
            &times;
          </button>
        </div>
      )}

      {googleOAuthMissing && (
        <div className="flex items-start gap-3 p-4 rounded-md border border-yellow-500/30 bg-yellow-500/5">
          <span className="text-yellow-600 text-lg leading-none mt-0.5">!</span>
          <div>
            <p className="text-sm font-medium text-text-primary">Google OAuth not configured</p>
            <p className="text-xs text-text-secondary mt-0.5">
              Google services (Gmail, Calendar, Drive, Contacts) require OAuth credentials.{' '}
              <a href="/dashboard/settings" className="text-brand hover:underline">Go to Settings</a>{' '}
              to configure your Google Client ID and Client Secret.
            </p>
          </div>
        </div>
      )}

      {microsoftOAuthMissing && (
        <div className="flex items-start gap-3 p-4 rounded-md border border-yellow-500/30 bg-yellow-500/5">
          <span className="text-yellow-600 text-lg leading-none mt-0.5">!</span>
          <div>
            <p className="text-sm font-medium text-text-primary">Microsoft OAuth not configured</p>
            <p className="text-xs text-text-secondary mt-0.5">
              Microsoft services (Outlook, OneDrive, Teams) require OAuth credentials.{' '}
              <a href="/dashboard/settings" className="text-brand hover:underline">Go to Settings</a>{' '}
              to configure your Microsoft Client ID and Client Secret.
            </p>
          </div>
        </div>
      )}

      {isLoading && <div className="text-sm text-text-tertiary">Loading…</div>}
      {error && <div className="text-sm text-danger">Failed to load services.</div>}

      {!orgId && !isLoading && !error && (
        <>
          {useUnifiedVault && (
            <VaultInventorySection
              vaultItems={vaultItems?.entries ?? []}
              placeholders={runtimePlaceholders?.entries ?? []}
              agents={agents}
              services={activeServices}
              googleOAuthConfigured={!!googleOAuth?.configured}
            />
          )}
        </>
      )}

      {!isLoading && !error && activeServices.length === 0 && (
        <button
          onClick={() => setShowModal(true)}
          className="w-full border-2 border-dashed border-border-default rounded-lg py-12 flex flex-col items-center gap-3 hover:border-brand/40 hover:bg-brand/[0.02] transition-colors cursor-pointer"
        >
          <div className="w-10 h-10 rounded-full bg-brand/10 flex items-center justify-center">
            <svg width="20" height="20" viewBox="0 0 20 20" fill="none">
              <path d="M10 4v12M4 10h12" stroke="currentColor" strokeWidth="2" strokeLinecap="round" className="text-brand" />
            </svg>
          </div>
          <div className="text-center">
            <p className="text-sm font-medium text-text-primary">Connect your first service</p>
            <p className="text-xs text-text-tertiary mt-0.5">Slack, GitHub, Gmail, and more</p>
          </div>
        </button>
      )}

      {activeServices.length > 0 && !useUnifiedVault && (
        <div className="bg-surface-1 border border-border-default rounded-lg divide-y divide-border-subtle overflow-hidden">
          {activeServices.map(svc => (
            <ActiveServiceRow key={`${svc.id}:${svc.alias ?? 'default'}`} svc={svc} />
          ))}
        </div>
      )}

      {features?.local_daemon && <LocalServicesSection />}

      {showModal && (
        <AddServiceModal
          services={allServices}
          onClose={() => setShowModal(false)}
          onSuccess={handleConnectionSuccess}
          googleOAuthMissing={googleOAuthMissing}
          microsoftOAuthMissing={microsoftOAuthMissing}
          activeConnectionCount={activeServices.length}
        />
      )}
    </div>
  )
}
