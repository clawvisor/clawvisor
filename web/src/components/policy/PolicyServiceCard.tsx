import { Link, useNavigate } from 'react-router-dom'
import type { OrgRestriction, Restriction, ServiceInfo } from '../../api/client'
import { ServiceIconBadge } from '../ServiceIcon'
import { serviceName } from '../../lib/services'
import { serviceKey, type ServiceBasePolicy } from '../../lib/serviceBasePolicies'
import { policyAccountRulesPath } from '../../lib/policyRoutes'

export default function PolicyServiceCard({
  svc,
  restrictions,
  activated,
  highlighted,
  basePolicy,
  previewOnly,
}: {
  svc: ServiceInfo
  restrictions: (Restriction | OrgRestriction)[]
  activated: boolean
  highlighted?: boolean
  basePolicy?: ServiceBasePolicy
  previewOnly?: boolean
}) {
  const navigate = useNavigate()
  const svcKey = serviceKey(svc)
  const rulesPath = policyAccountRulesPath(svc)
  const blockedCount = restrictions.filter(r => r.service === svcKey).length
  const summary = previewOnly && basePolicy
    ? basePolicy.description
    : blockedCount > 0
      ? `${blockedCount} action${blockedCount === 1 ? '' : 's'} blocked`
      : activated
        ? 'No actions blocked — agents can use all service actions unless a task requires approval.'
        : 'Connect this service to manage account-level restrictions.'

  const isDormant = !activated && !previewOnly
  const className = `lib-card lib-cat-policy${highlighted ? ' lib-card-highlight' : ''}${isDormant ? ' lib-card-dormant' : ''}`

  function openRules() {
    navigate(rulesPath)
  }

  const cardBody = (
    <div className="lib-card-content">
      <div className="lib-card-head">
        <div className="lib-icon">
          <ServiceIconBadge
            iconSvg={svc.icon_svg}
            iconUrl={svc.icon_url}
            serviceId={svc.id}
            size={18}
          />
        </div>
        <div className="min-w-0">
          <div className="lib-card-title">{serviceName(svc.id, svc.alias)}</div>
          {previewOnly && (
            <span className="mt-1 inline-flex rounded-full border border-border-default bg-surface-0 px-2 py-0.5 text-2xs font-medium uppercase tracking-wide text-text-tertiary">
              Base policy
            </span>
          )}
        </div>
      </div>
      <p className="lib-description">{summary}</p>
      <div className="lib-card-actions" onClick={e => e.stopPropagation()}>
        {!isDormant && (
          <Link to={rulesPath} className="dev-btn-ghost">
            Manage rules
          </Link>
        )}
        {previewOnly && (
          <Link to="/dashboard/accounts" className="dev-btn-ghost">
            Connect Gmail
          </Link>
        )}
        {isDormant && (
          <Link to="/dashboard/accounts" className="dev-btn-ghost">
            Connect now
          </Link>
        )}
      </div>
    </div>
  )

  return isDormant ? (
    <div className={className}>{cardBody}</div>
  ) : (
    <div
      role="button"
      tabIndex={0}
      className={className}
      onClick={openRules}
      onKeyDown={e => {
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault()
          openRules()
        }
      }}
    >
      {cardBody}
    </div>
  )
}
