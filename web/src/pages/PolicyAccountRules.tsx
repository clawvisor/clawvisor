import { Link, Navigate, useParams } from 'react-router-dom'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '../api/client'
import { useAuth } from '../hooks/useAuth'
import { ServiceIconBadge } from '../components/ServiceIcon'
import { ServiceGroup } from '../components/policy/ServiceRestrictions'
import { actionName, serviceName } from '../lib/services'
import {
  basePolicyApplied,
  getServiceBasePolicy,
  missingBasePolicyBlocks,
  serviceKey,
} from '../lib/serviceBasePolicies'
import { findServiceByKey, policyAccountsIndexPath } from '../lib/policyRoutes'
import Toggle from '../components/policy/Toggle'
import './library.css'
import './policy.css'

export default function PolicyAccountRules() {
  const { serviceKey: serviceKeyParam = '' } = useParams()
  const decodedKey = decodeURIComponent(serviceKeyParam)
  const { currentOrg } = useAuth()
  const orgId = currentOrg?.id
  const qc = useQueryClient()

  const { data: servicesData, isLoading: servicesLoading } = useQuery({
    queryKey: ['services'],
    queryFn: () => api.services.list(),
  })
  const { data: restrictions, isLoading: restrictionsLoading } = useQuery({
    queryKey: ['restrictions', orgId ?? 'personal'],
    queryFn: async () => orgId
      ? api.orgs.restrictions.list(orgId)
      : api.restrictions.list(),
  })

  const svc = findServiceByKey(servicesData?.services ?? [], decodedKey)
  const basePolicy = svc ? getServiceBasePolicy(svc.id) : undefined
  const isActivated = svc?.status === 'activated'
  const previewOnly = !!svc && !isActivated && !!basePolicy
  const isLoading = servicesLoading || restrictionsLoading

  const applyBasePolicyMut = useMutation({
    mutationFn: async () => {
      if (!svc || !basePolicy) return
      const svcKey = serviceKey(svc)
      const missing = missingBasePolicyBlocks(svcKey, basePolicy, restrictions ?? [])
      for (const block of missing) {
        if (orgId) await api.orgs.restrictions.create(orgId, svcKey, block.action, block.reason)
        else await api.restrictions.create(svcKey, block.action, block.reason)
      }
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['restrictions'] })
      qc.invalidateQueries({ queryKey: ['library-policy-explored'] })
    },
  })

  if (!isLoading && !svc) {
    return <Navigate to={policyAccountsIndexPath()} replace />
  }

  const blocked = new Set(basePolicy?.blocks.map(block => block.action) ?? [])
  const canApplyBasePolicy = !!svc && !!basePolicy && isActivated
    && !basePolicyApplied(serviceKey(svc), basePolicy, restrictions ?? [])

  return (
    <div className="lib-page">
      <nav aria-label="Breadcrumb" className="flex items-center gap-2 text-sm flex-wrap">
        <Link to="/dashboard/policy" className="text-brand hover:underline">
          Policy
        </Link>
        <span className="text-text-tertiary" aria-hidden>/</span>
        <Link to={policyAccountsIndexPath()} className="text-brand hover:underline">
          Account Controls
        </Link>
        <span className="text-text-tertiary" aria-hidden>/</span>
        <span className="text-text-primary">
          {svc ? serviceName(svc.id, svc.alias) : '…'}
        </span>
      </nav>

      {isLoading && (
        <p className="text-sm text-text-tertiary">Loading…</p>
      )}

      {!isLoading && svc && (
        <>
          <header className="lib-hero">
            <div className="flex items-center gap-3">
              <div className="shrink-0">
                <ServiceIconBadge
                  iconSvg={svc.icon_svg}
                  iconUrl={svc.icon_url}
                  serviceId={svc.id}
                  size={36}
                />
              </div>
              <div className="min-w-0">
                <p className="ds-overline normal-case tracking-normal mb-1">Account policy</p>
                <h2 className="text-lg font-semibold text-text-primary leading-tight">
                  {serviceName(svc.id, svc.alias)}
                </h2>
              </div>
            </div>
          </header>

          {previewOnly && basePolicy ? (
            <div className="rounded-md border border-border-default bg-surface-1 p-5 space-y-5">
              <p className="text-sm text-text-secondary leading-relaxed">{basePolicy.description}</p>
              <div>
                <p className="text-xs uppercase tracking-wider text-text-tertiary mb-3">Base policy</p>
                <div className="rounded-md border border-border-default divide-y divide-border-subtle overflow-hidden bg-surface-0">
                  {svc.actions.map(action => {
                    const isBlocked = blocked.has(action.id)
                    const block = basePolicy.blocks.find(entry => entry.action === action.id)
                    return (
                      <div key={action.id} className="flex items-center gap-3 py-2 px-4 bg-surface-0">
                        <Toggle checked={isBlocked} disabled onChange={() => {}} />
                        <span className={`text-sm flex-1 ${isBlocked ? 'text-danger font-medium' : 'text-text-secondary'}`}>
                          {actionName(action.id, svc.id)}
                        </span>
                        {isBlocked && (
                          <span className="text-xs text-danger" title={block?.reason}>
                            Blocked
                          </span>
                        )}
                      </div>
                    )
                  })}
                </div>
              </div>
              <Link to="/dashboard/accounts" className="dev-btn-primary inline-flex w-fit">
                Connect Gmail →
              </Link>
            </div>
          ) : (
            <ServiceGroup
              svc={svc}
              restrictions={restrictions ?? []}
              orgId={orgId}
              basePolicy={basePolicy}
              onApplyBasePolicy={canApplyBasePolicy ? () => applyBasePolicyMut.mutate() : undefined}
              applyingBasePolicy={applyBasePolicyMut.isPending}
            />
          )}
        </>
      )}
    </div>
  )
}
