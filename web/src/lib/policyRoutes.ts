import { serviceKey } from './serviceBasePolicies'
import type { ServiceInfo } from '../api/client'

export function policyAccountsIndexPath() {
  return '/dashboard/policy?tab=accounts'
}

export function policyAccountRulesPath(svc: Pick<ServiceInfo, 'id' | 'alias'>) {
  return `/dashboard/policy/accounts/${encodeURIComponent(serviceKey(svc))}`
}

export function findServiceByKey(services: ServiceInfo[], key: string): ServiceInfo | undefined {
  return services.find(svc => serviceKey(svc) === key)
}
