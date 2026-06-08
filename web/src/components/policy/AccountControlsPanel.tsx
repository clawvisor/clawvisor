import { useMemo } from 'react'
import { Link } from 'react-router-dom'
import type { OrgRestriction, Restriction, ServiceInfo } from '../../api/client'
import {
  GMAIL_BASE_POLICY,
  getServiceBasePolicy,
  type ServiceBasePolicy,
} from '../../lib/serviceBasePolicies'
import PolicyServiceCard from './PolicyServiceCard'

export default function AccountControlsPanel({
  isLoading,
  allServices,
  activated,
  unactivated,
  restrictions,
}: {
  isLoading: boolean
  allServices: ServiceInfo[]
  activated: ServiceInfo[]
  unactivated: ServiceInfo[]
  restrictions: (Restriction | OrgRestriction)[]
}) {
  const gmailService = allServices.find(svc => svc.id === GMAIL_BASE_POLICY.serviceId)

  const gridServices = useMemo(() => {
    const items: Array<{
      svc: ServiceInfo
      activated: boolean
      highlighted?: boolean
      previewOnly?: boolean
      basePolicy?: ServiceBasePolicy
    }> = []

    if (activated.length === 0 && gmailService) {
      items.push({
        svc: gmailService,
        activated: false,
        highlighted: true,
        previewOnly: true,
        basePolicy: GMAIL_BASE_POLICY,
      })
    }

    for (const svc of activated) {
      items.push({
        svc,
        activated: true,
        basePolicy: getServiceBasePolicy(svc.id),
      })
    }

    for (const svc of unactivated) {
      if (gmailService && svc.id === gmailService.id && activated.length === 0) continue
      items.push({ svc, activated: false })
    }

    return items
  }, [activated, gmailService, unactivated])

  return (
    <div className="space-y-4">
      {isLoading && <div className="text-sm text-text-tertiary">Loading...</div>}

      {!isLoading && allServices.length === 0 && (
        <div className="lib-error">No services registered. Add adapters in the server configuration to manage policy.</div>
      )}

      {!isLoading && allServices.length > 0 && gridServices.length === 0 && (
        <div className="lib-error">
          Activate a service first to manage account controls.{' '}
          <Link to="/dashboard/accounts" className="text-brand hover:underline">Go to Accounts</Link>
        </div>
      )}

      {!isLoading && gridServices.length > 0 && (
        <>
          <span className="ds-data text-text-tertiary shrink-0 tabular-nums">
            {gridServices.length} shown
            {unactivated.length > 0 && (
              <> · {unactivated.length} not connected</>
            )}
          </span>
          <section className="lib-grid">
            {gridServices.map(({ svc, activated: isActivated, highlighted, previewOnly, basePolicy }) => (
              <PolicyServiceCard
                key={svc.alias ? `${svc.id}:${svc.alias}` : svc.id}
                svc={svc}
                restrictions={restrictions}
                activated={isActivated}
                highlighted={highlighted}
                basePolicy={basePolicy}
                previewOnly={previewOnly}
              />
            ))}
          </section>
        </>
      )}
    </div>
  )
}
