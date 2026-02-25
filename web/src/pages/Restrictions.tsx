import { useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api, type Restriction, type ServiceInfo } from '../api/client'
import { serviceName, actionName, serviceBrand } from '../lib/services'

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
      } ${loading ? 'opacity-60' : ''} ${checked ? 'bg-red-500' : 'bg-gray-300'}`}
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
      <span className={`text-sm flex-1 ${isBlocked ? 'text-red-700 font-medium' : 'text-gray-700'}`}>
        {actionName(action)}
      </span>
      {isBlocked && !showReason && (
        <span className="text-xs text-red-400">Blocked</span>
      )}
      {showReason && !isBlocked && (
        <div className="flex items-center gap-2">
          <input
            type="text"
            value={reason}
            onChange={e => setReason(e.target.value)}
            onKeyDown={e => e.key === 'Enter' && handleConfirmBlock()}
            placeholder="Reason (optional)"
            className="text-xs rounded border border-gray-300 px-2 py-1 focus:outline-none focus:ring-1 focus:ring-red-400 w-44"
            autoFocus
          />
          <button
            onClick={handleConfirmBlock}
            disabled={createMut.isPending}
            className="text-xs px-2 py-1 rounded bg-red-600 text-white hover:bg-red-700 disabled:opacity-50"
          >
            Block
          </button>
          <button
            onClick={() => setShowReason(false)}
            className="text-xs text-gray-400 hover:text-gray-600"
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
      <span className={`text-xs font-medium ${isBlocked ? 'text-red-600' : 'text-gray-500'}`}>
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
            className="text-xs rounded border border-gray-300 px-2 py-1 focus:outline-none focus:ring-1 focus:ring-red-400 w-44"
            autoFocus
          />
          <button
            onClick={handleConfirmBlock}
            disabled={createMut.isPending}
            className="text-xs px-2 py-1 rounded bg-red-600 text-white hover:bg-red-700 disabled:opacity-50"
          >
            Block
          </button>
          <button
            onClick={() => setShowReason(false)}
            className="text-xs text-gray-400 hover:text-gray-600"
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
  const brand = serviceBrand(svc.id)
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
    <div className={`bg-white border rounded-lg overflow-hidden border-l-4 ${brand.border}`}>
      <div className="px-4 py-3 flex items-center justify-between">
        <div>
          <h3 className="text-sm font-semibold text-gray-900">{serviceName(svc.id, svc.alias)}</h3>
          <p className="text-xs text-gray-400">{svcKey}</p>
        </div>
        <WildcardToggle serviceId={svcKey} restrictionId={wildcardId} />
      </div>
      <div className="border-t divide-y divide-gray-100">
        {svc.actions.map(action => (
          <ActionRow
            key={action}
            serviceId={svcKey}
            action={action}
            restrictionId={lookup.get(`${svcKey}:${action}`) ?? null}
            disabled={hasWildcard}
          />
        ))}
      </div>
    </div>
  )
}

export default function Restrictions() {
  const { data: servicesData, isLoading: servicesLoading } = useQuery({
    queryKey: ['services'],
    queryFn: () => api.services.list(),
  })

  const { data: restrictions, isLoading: restrictionsLoading } = useQuery({
    queryKey: ['restrictions'],
    queryFn: () => api.restrictions.list(),
  })

  const isLoading = servicesLoading || restrictionsLoading
  const services = servicesData?.services ?? []
  const allRestrictions = restrictions ?? []

  return (
    <div className="p-8 space-y-6">
      <h1 className="text-2xl font-bold text-gray-900">Restrictions</h1>
      <p className="text-sm text-gray-500">
        Block specific service actions. Any agent request matching a restriction is rejected immediately.
      </p>

      {isLoading && <div className="text-sm text-gray-400">Loading...</div>}

      {!isLoading && services.length === 0 && (
        <div className="text-sm text-gray-400 py-8 text-center">
          No services registered. Add adapters in the server configuration to manage restrictions.
        </div>
      )}

      <div className="space-y-4">
        {services.map(svc => (
          <ServiceGroup
            key={svc.alias ? `${svc.id}:${svc.alias}` : svc.id}
            svc={svc}
            restrictions={allRestrictions}
          />
        ))}
      </div>
    </div>
  )
}
