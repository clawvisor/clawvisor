import { useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { api, type Restriction, type ServiceInfo } from '../api/client'
import { serviceName, actionName } from '../lib/services'

function Toggle({
  checked,
  disabled,
  loading,
  onChange,
}: {
  checked: boolean
  disabled?: boolean
  loading?: boolean
  onChange: (checked: boolean) => void
}) {
  return (
    <button
      role="switch"
      aria-checked={checked}
      disabled={disabled || loading}
      onClick={() => onChange(!checked)}
      className={`relative inline-flex h-5 w-9 shrink-0 rounded-full transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-offset-2 ${
        disabled ? 'opacity-40 cursor-not-allowed' : 'cursor-pointer'
      } ${loading ? 'opacity-60' : ''} ${checked ? 'bg-danger' : 'bg-border-strong'}`}
    >
      <span
        className={`pointer-events-none inline-block h-4 w-4 rounded-full bg-white shadow transform transition-transform mt-0.5 ${
          checked ? 'translate-x-[18px] ml-0' : 'translate-x-0.5'
        }`}
      />
    </button>
  )
}

function ActionRow({
  serviceId,
  action,
  restrictionId,
  disabled,
}: {
  serviceId: string
  action: string
  restrictionId: string | null
  disabled: boolean
}) {
  const qc = useQueryClient()
  const [reason, setReason] = useState('')
  const [showReason, setShowReason] = useState(false)

  const createMut = useMutation({
    mutationFn: (r: string) => api.restrictions.create(serviceId, action, r),
    onSuccess: () => {
      setReason('')
      setShowReason(false)
      qc.invalidateQueries({ queryKey: ['restrictions'] })
    },
  })

  const deleteMut = useMutation({
    mutationFn: () => api.restrictions.delete(restrictionId!),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['restrictions'] })
    },
  })

  const isBlocked = !!restrictionId
  const loading = createMut.isPending || deleteMut.isPending

  function handleToggle(checked: boolean) {
    if (checked) {
      setShowReason(true)
    } else if (restrictionId) {
      deleteMut.mutate()
    }
  }

  function handleConfirmBlock() {
    createMut.mutate(reason.trim())
  }

  return (
    <div className={`flex items-center gap-3 py-2 px-4 ${loading ? 'opacity-60' : ''}`}>
      <Toggle
        checked={isBlocked}
        disabled={disabled}
        loading={loading}
        onChange={handleToggle}
      />
      <span className={`text-sm flex-1 ${isBlocked ? 'text-danger font-medium' : 'text-text-secondary'}`}>
        {actionName(action)}
      </span>
      {isBlocked && !showReason && (
        <span className="text-xs text-danger">Blocked</span>
      )}
      {showReason && !isBlocked && (
        <div className="flex items-center gap-2">
          <input
            type="text"
            value={reason}
            onChange={e => setReason(e.target.value)}
            onKeyDown={e => e.key === 'Enter' && handleConfirmBlock()}
            placeholder="Reason (optional)"
            className="text-xs rounded border border-border-default bg-surface-0 text-text-primary px-2 py-1 focus:outline-none focus:ring-1 focus:ring-danger/30 focus:border-danger w-44 placeholder:text-text-tertiary"
            autoFocus
          />
          <button
            onClick={handleConfirmBlock}
            disabled={createMut.isPending}
            className="text-xs px-2 py-1 rounded bg-danger text-surface-0 hover:bg-red-500 disabled:opacity-50"
          >
            Block
          </button>
          <button
            onClick={() => setShowReason(false)}
            className="text-xs text-text-tertiary hover:text-text-primary"
          >
            Cancel
          </button>
        </div>
      )}
    </div>
  )
}

function WildcardToggle({
  serviceId,
  restrictionId,
}: {
  serviceId: string
  restrictionId: string | null
}) {
  const qc = useQueryClient()
  const [reason, setReason] = useState('')
  const [showReason, setShowReason] = useState(false)

  const createMut = useMutation({
    mutationFn: (r: string) => api.restrictions.create(serviceId, '*', r),
    onSuccess: () => {
      setReason('')
      setShowReason(false)
      qc.invalidateQueries({ queryKey: ['restrictions'] })
    },
  })

  const deleteMut = useMutation({
    mutationFn: () => api.restrictions.delete(restrictionId!),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['restrictions'] })
    },
  })

  const isBlocked = !!restrictionId
  const loading = createMut.isPending || deleteMut.isPending

  function handleToggle(checked: boolean) {
    if (checked) {
      setShowReason(true)
    } else if (restrictionId) {
      deleteMut.mutate()
    }
  }

  function handleConfirmBlock() {
    createMut.mutate(reason.trim())
  }

  return (
    <div className={`flex items-center gap-3 ${loading ? 'opacity-60' : ''}`}>
      <Toggle checked={isBlocked} loading={loading} onChange={handleToggle} />
      <span className={`text-xs font-medium ${isBlocked ? 'text-danger' : 'text-text-tertiary'}`}>
        Block all actions
      </span>
      {showReason && !isBlocked && (
        <div className="flex items-center gap-2 ml-2">
          <input
            type="text"
            value={reason}
            onChange={e => setReason(e.target.value)}
            onKeyDown={e => e.key === 'Enter' && handleConfirmBlock()}
            placeholder="Reason (optional)"
            className="text-xs rounded border border-border-default bg-surface-0 text-text-primary px-2 py-1 focus:outline-none focus:ring-1 focus:ring-danger/30 focus:border-danger w-44 placeholder:text-text-tertiary"
            autoFocus
          />
          <button
            onClick={handleConfirmBlock}
            disabled={createMut.isPending}
            className="text-xs px-2 py-1 rounded bg-danger text-surface-0 hover:bg-red-500 disabled:opacity-50"
          >
            Block
          </button>
          <button
            onClick={() => setShowReason(false)}
            className="text-xs text-text-tertiary hover:text-text-primary"
          >
            Cancel
          </button>
        </div>
      )}
    </div>
  )
}

function ServiceGroup({
  svc,
  restrictions,
}: {
  svc: ServiceInfo
  restrictions: Restriction[]
}) {
  // The restriction service key includes the alias when present (e.g. "google.gmail:personal").
  const svcKey = svc.alias ? `${svc.id}:${svc.alias}` : svc.id

  // Build lookup: "service:action" → restriction ID
  const lookup = new Map<string, string>()
  for (const r of restrictions) {
    if (r.service === svcKey) {
      lookup.set(`${r.service}:${r.action}`, r.id)
    }
  }

  const wildcardId = lookup.get(`${svcKey}:*`) ?? null
  const hasWildcard = !!wildcardId

  return (
    <div className="bg-surface-1 border border-border-default rounded-md overflow-hidden">
      <div className="px-4 py-3 flex items-center justify-between">
        <div>
          <h3 className="text-sm font-semibold text-text-primary">{serviceName(svc.id, svc.alias)}</h3>
          <p className="text-xs text-text-tertiary">{svcKey}</p>
        </div>
        <WildcardToggle serviceId={svcKey} restrictionId={wildcardId} />
      </div>
      <div className="border-t border-border-default divide-y divide-border-subtle">
        {svc.actions.map(action => (
          <ActionRow
            key={action.id}
            serviceId={svcKey}
            action={action.id}
            restrictionId={lookup.get(`${svcKey}:${action.id}`) ?? null}
            disabled={hasWildcard}
          />
        ))}
      </div>
    </div>
  )
}

export default function Restrictions() {
  const [showAll, setShowAll] = useState(false)

  const { data: servicesData, isLoading: servicesLoading } = useQuery({
    queryKey: ['services'],
    queryFn: () => api.services.list(),
  })

  const { data: restrictions, isLoading: restrictionsLoading } = useQuery({
    queryKey: ['restrictions'],
    queryFn: () => api.restrictions.list(),
  })

  const isLoading = servicesLoading || restrictionsLoading
  const allServices = servicesData?.services ?? []
  const allRestrictions = restrictions ?? []

  const activated = allServices.filter(s => s.status === 'activated')
  const unactivated = allServices.filter(s => s.status !== 'activated')

  return (
    <div className="p-8 space-y-6">
      <h1 className="text-2xl font-bold text-text-primary">Restrictions</h1>
      <p className="text-sm text-text-tertiary">
        Block specific service actions. Any agent request matching a restriction is rejected immediately.
      </p>

      {isLoading && <div className="text-sm text-text-tertiary">Loading...</div>}

      {!isLoading && allServices.length === 0 && (
        <div className="text-sm text-text-tertiary py-8 text-center">
          No services registered. Add adapters in the server configuration to manage restrictions.
        </div>
      )}

      {!isLoading && allServices.length > 0 && activated.length === 0 && (
        <div className="text-sm text-text-tertiary py-8 text-center">
          Activate a service first to manage restrictions.{' '}
          <Link to="/dashboard/services" className="text-brand hover:underline">Go to Services</Link>
        </div>
      )}

      <div className="space-y-4">
        {activated.map(svc => (
          <ServiceGroup
            key={svc.alias ? `${svc.id}:${svc.alias}` : svc.id}
            svc={svc}
            restrictions={allRestrictions}
          />
        ))}
      </div>

      {unactivated.length > 0 && (
        <div className="space-y-4">
          <button
            onClick={() => setShowAll(s => !s)}
            className="text-sm text-text-tertiary hover:text-text-primary"
          >
            {showAll ? 'Hide unactivated services' : `Show all services (${unactivated.length} not activated)`}
          </button>
          {showAll && (
            <div className="space-y-4 opacity-50">
              {unactivated.map(svc => (
                <ServiceGroup
                  key={svc.alias ? `${svc.id}:${svc.alias}` : svc.id}
                  svc={svc}
                  restrictions={allRestrictions}
                />
              ))}
            </div>
          )}
        </div>
      )}
    </div>
  )
}
