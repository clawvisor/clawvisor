// Shared service/action display utilities. Zero React dependencies.
// Metadata is read from the API response (ServiceInfo) when available,
// falling back to hardcoded defaults for services not yet loaded.

import type { ServiceInfo } from '../api/client'

// ── Metadata cache (populated from API responses) ──────────────────────────

const _names: Record<string, string> = {}
const _descriptions: Record<string, string> = {}
const _actionNames: Record<string, string> = {} // "service:action" → display name

// populateFromServices caches metadata from the API response.
// Call this whenever a services list is fetched.
export function populateFromServices(services: ServiceInfo[]) {
  for (const svc of services) {
    const base = svc.id.includes(':') ? svc.id.slice(0, svc.id.indexOf(':')) : svc.id
    if (svc.name) _names[base] = svc.name
    if (svc.description) _descriptions[base] = svc.description
    if (svc.actions) {
      for (const a of svc.actions) {
        if (a.display_name) {
          _actionNames[`${base}:${a.id}`] = a.display_name
          // Also store by action ID alone (for legacy lookups).
          if (!_actionNames[a.id]) {
            _actionNames[a.id] = a.display_name
          }
        }
      }
    }
  }
}

// ── Hardcoded fallbacks (for services not yet loaded from API) ─────────────

const FALLBACK_NAMES: Record<string, string> = {
  'google.gmail': 'Gmail',
  'google.calendar': 'Google Calendar',
  'google.drive': 'Google Drive',
  'google.contacts': 'Google Contacts',
  'github': 'GitHub',
  'apple.imessage': 'iMessage',
  'slack': 'Slack',
  'notion': 'Notion',
  'linear': 'Linear',
  'stripe': 'Stripe',
  'twilio': 'Twilio',
}

const FALLBACK_DESCRIPTIONS: Record<string, string> = {
  'google.gmail': 'Read, search, send, and draft email',
  'google.calendar': 'View and manage calendar events',
  'google.drive': 'List, search, and manage files',
  'google.contacts': 'Search and view contacts',
  'github': 'Issues, PRs, and code review',
  'apple.imessage': 'Search and read iMessage threads',
  'slack': 'Channels, messages, and search',
  'notion': 'Pages, databases, and search',
  'linear': 'Issues, projects, and teams',
  'stripe': 'Customers, charges, and subscriptions',
  'twilio': 'SMS, WhatsApp, and messaging',
}

// ── Public API ─────────────────────────────────────────────────────────────

export function serviceName(id: string, alias?: string): string {
  const colonIdx = id.indexOf(':')
  if (colonIdx >= 0) {
    const base = id.slice(0, colonIdx)
    const a = id.slice(colonIdx + 1)
    const name = _names[base] ?? FALLBACK_NAMES[base] ?? base
    return `${name} (${a})`
  }
  const name = _names[id] ?? FALLBACK_NAMES[id] ?? id
  if (alias && alias !== 'default') {
    return `${name} (${alias})`
  }
  return name
}

export function serviceDescription(id: string): string {
  const base = id.includes(':') ? id.slice(0, id.indexOf(':')) : id
  return _descriptions[base] ?? FALLBACK_DESCRIPTIONS[base] ?? ''
}

export function actionName(action: string, service?: string): string {
  if (action === '*') return 'All actions'
  // Try service-qualified lookup first, then plain action ID.
  if (service) {
    const base = service.includes(':') ? service.slice(0, service.indexOf(':')) : service
    const qualified = _actionNames[`${base}:${action}`]
    if (qualified) return qualified
  }
  return _actionNames[action] ?? action.replace(/_/g, ' ')
}

export function formatServiceAction(service: string, action: string): string {
  return `${serviceName(service)}: ${actionName(action, service)}`
}
