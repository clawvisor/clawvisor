import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { api, type OrgRestriction, type Restriction, type ServiceInfo } from '../../api/client'
import { actionName, serviceName } from '../../lib/services'
import { missingBasePolicyBlocks, serviceKey, type ServiceBasePolicy } from '../../lib/serviceBasePolicies'
import Toggle from './Toggle'

function ActionRow({
  serviceId,
  action,
  restrictionId,
  disabled,
  orgId,
}: {
  serviceId: string
  action: string
  restrictionId: string | null
  disabled: boolean
  orgId?: string
}) {
  const qc = useQueryClient()
  const [reason, setReason] = useState('')
  const [showReason, setShowReason] = useState(false)

  const createMut = useMutation({
    mutationFn: async (r: string) => {
      if (orgId) await api.orgs.restrictions.create(orgId, serviceId, action, r)
      else await api.restrictions.create(serviceId, action, r)
    },
    onSuccess: () => {
      setReason('')
      setShowReason(false)
      qc.invalidateQueries({ queryKey: ['restrictions'] })
    },
  })

  const deleteMut = useMutation({
    mutationFn: async () => {
      if (orgId) await api.orgs.restrictions.delete(orgId, restrictionId!)
      else await api.restrictions.delete(restrictionId!)
    },
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
  orgId,
}: {
  serviceId: string
  restrictionId: string | null
  orgId?: string
}) {
  const qc = useQueryClient()
  const [reason, setReason] = useState('')
  const [showReason, setShowReason] = useState(false)

  const createMut = useMutation({
    mutationFn: async (r: string) => {
      if (orgId) await api.orgs.restrictions.create(orgId, serviceId, '*', r)
      else await api.restrictions.create(serviceId, '*', r)
    },
    onSuccess: () => {
      setReason('')
      setShowReason(false)
      qc.invalidateQueries({ queryKey: ['restrictions'] })
    },
  })

  const deleteMut = useMutation({
    mutationFn: async () => {
      if (orgId) await api.orgs.restrictions.delete(orgId, restrictionId!)
      else await api.restrictions.delete(restrictionId!)
    },
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
        <div className="flex flex-wrap items-center gap-2 ml-2">
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

export function ServiceGroup({
  svc,
  restrictions,
  orgId,
  basePolicy,
  onApplyBasePolicy,
  applyingBasePolicy,
  embedded,
}: {
  svc: ServiceInfo
  restrictions: (Restriction | OrgRestriction)[]
  orgId?: string
  basePolicy?: ServiceBasePolicy
  onApplyBasePolicy?: () => void
  applyingBasePolicy?: boolean
  embedded?: boolean
}) {
  const svcKey = serviceKey(svc)
  const missingBase = basePolicy
    ? missingBasePolicyBlocks(svcKey, basePolicy, restrictions)
    : []

  const lookup = new Map<string, string>()
  for (const r of restrictions) {
    if (r.service === svcKey) {
      lookup.set(`${r.service}:${r.action}`, r.id)
    }
  }

  const wildcardId = lookup.get(`${svcKey}:*`) ?? null
  const hasWildcard = !!wildcardId

  const header = (
    <div className={`flex items-center justify-between gap-3 ${embedded ? '' : 'px-4 py-3'}`}>
      <div className="min-w-0">
        {!embedded && (
          <>
            <h3 className="text-sm font-semibold text-text-primary">{serviceName(svc.id, svc.alias)}</h3>
            <p className="text-xs text-text-tertiary">{svcKey}</p>
          </>
        )}
        {embedded && (
          <p className="text-xs uppercase tracking-wider text-text-tertiary">Action restrictions</p>
        )}
      </div>
      <div className="flex items-center gap-2 shrink-0">
        {missingBase.length > 0 && onApplyBasePolicy && (
          <button
            type="button"
            onClick={onApplyBasePolicy}
            disabled={applyingBasePolicy}
            className="dev-btn-ghost text-xs whitespace-nowrap"
          >
            {applyingBasePolicy ? 'Applying…' : 'Apply base policy'}
          </button>
        )}
        <WildcardToggle serviceId={svcKey} restrictionId={wildcardId} orgId={orgId} />
      </div>
    </div>
  )

  const actionList = (
    <div className={embedded ? 'rounded-md border border-border-default divide-y divide-border-subtle overflow-hidden' : 'border-t border-border-default divide-y divide-border-subtle'}>
      {svc.actions.map(action => (
        <ActionRow
          key={action.id}
          serviceId={svcKey}
          action={action.id}
          restrictionId={lookup.get(`${svcKey}:${action.id}`) ?? null}
          disabled={hasWildcard}
          orgId={orgId}
        />
      ))}
    </div>
  )

  if (embedded) {
    return (
      <div className="space-y-3">
        {basePolicy && missingBase.length > 0 && (
          <p className="text-sm text-text-secondary leading-relaxed">{basePolicy.description}</p>
        )}
        {header}
        {actionList}
      </div>
    )
  }

  return (
    <div className="bg-surface-1 border border-border-default rounded-md overflow-hidden">
      {header}
      {basePolicy && missingBase.length > 0 && (
        <div className="px-4 py-2 border-t border-border-subtle bg-surface-0 text-xs text-text-tertiary">
          {basePolicy.description}
        </div>
      )}
      {actionList}
    </div>
  )
}
