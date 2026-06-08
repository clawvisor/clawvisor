import type { AuditEntry } from '../../api/client'

export const OUTCOMES = ['', 'executed', 'blocked', 'restricted', 'pending', 'denied', 'error', 'timeout'] as const
export const DECISIONS = ['', 'allow', 'approve', 'block', 'verify', 'observe'] as const
export const DATA_ORIGINS = ['', 'live', 'cached', 'synthetic'] as const

export const ACTIVITY_TYPES = [
  { value: '', label: 'All types' },
  { value: 'runtime_egress', label: 'Runtime egress' },
  { value: 'runtime_tool_use', label: 'Runtime tool use' },
  { value: 'runtime', label: 'Other runtime' },
  { value: 'service', label: 'Service activity' },
] as const

export type ActivityTypeFilter = '' | 'runtime_egress' | 'runtime_tool_use' | 'runtime' | 'service'
export type DisplayMode = Exclude<ActivityTypeFilter, ''> | 'default'

export type ActivityHistoryFilters = {
  since: string
  until: string
  agentId: string
  outcome: string
  decision: string
  activityType: ActivityTypeFilter
  service: string
  taskId: string
  dataOrigin: string
  requestId: string
  safetyOnly: boolean
  includeRuntime: boolean
}

export const DEFAULT_HISTORY_FILTERS: ActivityHistoryFilters = {
  since: '',
  until: '',
  agentId: '',
  outcome: '',
  decision: '',
  activityType: '',
  service: '',
  taskId: '',
  dataOrigin: '',
  requestId: '',
  safetyOnly: false,
  includeRuntime: true,
}

function activityType(entry: AuditEntry): ActivityTypeFilter {
  if (entry.service === 'runtime.egress') return 'runtime_egress'
  if (entry.service === 'runtime.tool_use') return 'runtime_tool_use'
  if (entry.service.startsWith('runtime.')) return 'runtime'
  return 'service'
}

export function matchesActivityType(entry: AuditEntry, filter: ActivityTypeFilter): boolean {
  if (!filter) return true
  return activityType(entry) === filter
}

export function displayMode(filter: ActivityTypeFilter): DisplayMode {
  return filter || 'default'
}

export function applyClientFilters(entries: AuditEntry[], filters: ActivityHistoryFilters): AuditEntry[] {
  return entries.filter(entry => {
    if (filters.decision && entry.decision !== filters.decision) return false
    if (filters.safetyOnly && !entry.safety_flagged) return false
    if (filters.requestId) {
      const q = filters.requestId.trim().toLowerCase()
      if (!entry.request_id.toLowerCase().includes(q)) return false
    }
    return matchesActivityType(entry, filters.activityType)
  })
}

export function toApiSince(date: string): string | undefined {
  if (!date) return undefined
  return new Date(`${date}T00:00:00`).toISOString()
}

export function toApiUntil(date: string): string | undefined {
  if (!date) return undefined
  return new Date(`${date}T23:59:59.999`).toISOString()
}

export function hasMoreFilters(filters: ActivityHistoryFilters): boolean {
  return !!(filters.since || filters.until || filters.agentId || filters.decision)
}

export function hasExpertFilters(filters: ActivityHistoryFilters): boolean {
  return !!(
    filters.taskId
    || filters.dataOrigin
    || filters.requestId
    || filters.safetyOnly
    || !filters.includeRuntime
  )
}

export function countActiveFilters(filters: ActivityHistoryFilters): number {
  let n = 0
  if (filters.since) n++
  if (filters.until) n++
  if (filters.agentId) n++
  if (filters.outcome) n++
  if (filters.decision) n++
  if (filters.activityType) n++
  if (filters.service) n++
  if (filters.taskId) n++
  if (filters.dataOrigin) n++
  if (filters.requestId) n++
  if (filters.safetyOnly) n++
  if (!filters.includeRuntime) n++
  return n
}
