// Shared helpers for Queue, Tasks, and Overview pages. Pure functions, no React.

import { serviceName, actionName } from './services'

export const STATUS_STYLES: Record<string, string> = {
  pending_approval: 'bg-orange-100 text-orange-800',
  pending_scope_expansion: 'bg-orange-100 text-orange-800',
  active: 'bg-green-100 text-green-800',
  completed: 'bg-gray-100 text-gray-600',
  expired: 'bg-gray-100 text-gray-500',
  denied: 'bg-red-100 text-red-700',
  revoked: 'bg-gray-100 text-gray-500',
}

export const STATUS_LABELS: Record<string, string> = {
  pending_approval: 'Pending Approval',
  pending_scope_expansion: 'Scope Expansion',
  active: 'Active',
  completed: 'Completed',
  expired: 'Expired',
  denied: 'Denied',
  revoked: 'Revoked',
}

export const OUTCOME_STYLE: Record<string, string> = {
  executed: 'bg-green-100 text-green-800',
  blocked: 'bg-red-100 text-red-800',
  restricted: 'bg-orange-100 text-orange-800',
  pending: 'bg-yellow-100 text-yellow-800',
  denied: 'bg-gray-100 text-gray-600',
  error: 'bg-red-100 text-red-700',
  timeout: 'bg-gray-100 text-gray-500',
}

export function summarizeActions(actions: { service: string; action: string; auto_execute: boolean }[]): string {
  const groups = new Map<string, { auto: string[]; manual: string[] }>()
  for (const a of actions) {
    const svc = serviceName(a.service)
    if (!groups.has(svc)) groups.set(svc, { auto: [], manual: [] })
    const g = groups.get(svc)!
    if (a.auto_execute) {
      g.auto.push(actionName(a.action).toLowerCase())
    } else {
      g.manual.push(actionName(a.action).toLowerCase())
    }
  }

  const parts: string[] = []
  for (const [svc, g] of groups) {
    if (g.auto.length > 0) parts.push(`Can ${joinList(g.auto)} on ${svc}`)
    if (g.manual.length > 0) parts.push(`Can ${joinList(g.manual)} on ${svc} with approval`)
  }
  return parts.join(' \u00b7 ') || 'No actions authorized'
}

export function joinList(items: string[]): string {
  if (items.length <= 1) return items[0] ?? ''
  return items.slice(0, -1).join(', ') + ' and ' + items[items.length - 1]
}
