import { useEffect, useMemo, useState, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import { api, type Agent, type AuditEntry } from '../../api/client'
import { AuditRow } from '../../pages/Audit'
import {
  ACTIVITY_TYPES,
  applyClientFilters,
  countActiveFilters,
  DATA_ORIGINS,
  DECISIONS,
  DEFAULT_HISTORY_FILTERS,
  displayMode,
  hasExpertFilters,
  hasMoreFilters,
  OUTCOMES,
  toApiSince,
  toApiUntil,
  type ActivityHistoryFilters,
} from './activityFilters'

const PAGE_SIZE = 50
const COL_COUNT = 7

const filterInputClass = 'ds-input min-w-0 !h-auto !py-1.5 !text-xs'

function FilterLabel({ children }: { children: ReactNode }) {
  return <span className="ds-overline">{children}</span>
}

export default function ActivityHistoryTable({
  orgId,
  agents,
  runtimeActivityUI,
  runtimePolicyUI,
  fullRuntimeActive,
  onCreateRule,
  onMute,
}: {
  orgId?: string
  agents: Agent[]
  runtimeActivityUI: boolean
  runtimePolicyUI: boolean
  fullRuntimeActive: boolean
  onCreateRule: (entry: AuditEntry) => void
  onMute: (entry: AuditEntry) => void
}) {
  const [filters, setFilters] = useState<ActivityHistoryFilters>(DEFAULT_HISTORY_FILTERS)
  const [showMore, setShowMore] = useState(false)
  const [showExpert, setShowExpert] = useState(false)
  const [offset, setOffset] = useState(0)

  const agentMap = useMemo(() => new Map(agents.map(a => [a.id, a.name])), [agents])
  const activityTypeOptions = useMemo(
    () => runtimeActivityUI ? ACTIVITY_TYPES : ACTIVITY_TYPES.filter(o => o.value === '' || o.value === 'service'),
    [runtimeActivityUI],
  )

  const apiFilter = useMemo(() => ({
    outcome: filters.outcome || undefined,
    service: filters.service || undefined,
    agent_id: filters.agentId || undefined,
    task_id: filters.taskId || undefined,
    data_origin: filters.dataOrigin || undefined,
    since: toApiSince(filters.since),
    until: toApiUntil(filters.until),
    include_runtime: runtimeActivityUI ? filters.includeRuntime : undefined,
    limit: PAGE_SIZE,
    offset,
  }), [filters, offset, runtimeActivityUI])

  const { data, isLoading } = useQuery({
    queryKey: ['audit-history', orgId ?? 'personal', apiFilter],
    queryFn: () => orgId
      ? api.orgs.audit(orgId, apiFilter)
      : api.audit.list(apiFilter),
    refetchInterval: 30_000,
  })

  const rawEntries = data?.entries ?? []
  const entries = useMemo(() => applyClientFilters(rawEntries, filters), [rawEntries, filters])
  const total = data?.total ?? 0
  const mode = displayMode(filters.activityType)
  const activeFilterCount = countActiveFilters(filters)
  const moreFiltersActive = hasMoreFilters(filters)
  const expertFiltersActive = hasExpertFilters(filters)
  const showMorePanel = showMore || moreFiltersActive
  const showExpertPanel = showExpert || expertFiltersActive

  const summaryHeading = mode === 'runtime_egress'
    ? 'Runtime egress'
    : mode === 'runtime_tool_use'
      ? 'Runtime tool use'
      : 'Summary'

  const emptyMessage = activeFilterCount > 0
    ? 'No entries match your filters.'
    : 'No activity recorded yet.'

  useEffect(() => {
    if (!runtimeActivityUI && filters.activityType && filters.activityType !== 'service') {
      setFilters(f => ({ ...f, activityType: '' }))
    }
  }, [filters.activityType, runtimeActivityUI])

  useEffect(() => {
    if (moreFiltersActive) setShowMore(true)
  }, [moreFiltersActive])

  useEffect(() => {
    if (expertFiltersActive) setShowExpert(true)
  }, [expertFiltersActive])

  function patchFilters(patch: Partial<ActivityHistoryFilters>) {
    setFilters(f => ({ ...f, ...patch }))
    setOffset(0)
  }

  function clearFilters() {
    setFilters(DEFAULT_HISTORY_FILTERS)
    setOffset(0)
    setShowMore(false)
    setShowExpert(false)
  }

  return (
    <section className="dev-panel overflow-hidden">
      {/* Table header */}
      <div className="flex flex-wrap items-baseline justify-between gap-3 px-4 py-3 border-b border-border-default bg-surface-1">
        <div>
          <h2 className="page-section-title mb-0.5">Activity history</h2>
          <p className="text-xs text-text-tertiary">
            {total} event{total === 1 ? '' : 's'}
            {activeFilterCount > 0 && ` · ${activeFilterCount} filter${activeFilterCount === 1 ? '' : 's'}`}
          </p>
        </div>
        {total > PAGE_SIZE && (
          <p className="text-xs text-text-tertiary">
            page {Math.floor(offset / PAGE_SIZE) + 1} · {offset + 1}–{Math.min(offset + PAGE_SIZE, total)}
          </p>
        )}
      </div>

      {/* Filter toolbar */}
      <div className="px-4 py-3 border-b border-border-default bg-surface-0/50 space-y-3">
        <div className="grid gap-3 sm:grid-cols-3">
          <label className="space-y-1">
            <FilterLabel>Outcome</FilterLabel>
            <select
              value={filters.outcome}
              onChange={e => patchFilters({ outcome: e.target.value })}
              className={filterInputClass}
            >
              <option value="">All outcomes</option>
              {OUTCOMES.filter(Boolean).map(o => (
                <option key={o} value={o}>{o}</option>
              ))}
            </select>
          </label>
          <label className="space-y-1">
            <FilterLabel>Activity type</FilterLabel>
            <select
              value={filters.activityType}
              onChange={e => patchFilters({ activityType: e.target.value as ActivityHistoryFilters['activityType'] })}
              className={filterInputClass}
            >
              {activityTypeOptions.map(o => (
                <option key={o.value} value={o.value}>{o.label}</option>
              ))}
            </select>
          </label>
          <label className="space-y-1">
            <FilterLabel>Service</FilterLabel>
            <input
              value={filters.service}
              onChange={e => patchFilters({ service: e.target.value })}
              placeholder="e.g. github"
              className={filterInputClass}
            />
          </label>
        </div>

        {!showMorePanel ? (
          <button type="button" onClick={() => setShowMore(true)} className="dev-text-link">
            more filters
          </button>
        ) : (
          <div className="space-y-3 pt-1 border-t border-border-subtle">
            <div className="flex items-center justify-between gap-2">
              <span className="ds-overline">More filters</span>
              {!moreFiltersActive && (
                <button type="button" onClick={() => setShowMore(false)} className="dev-text-link">hide</button>
              )}
            </div>
            <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
              <label className="space-y-1">
                <FilterLabel>From</FilterLabel>
                <input type="date" value={filters.since} onChange={e => patchFilters({ since: e.target.value })} className={filterInputClass} />
              </label>
              <label className="space-y-1">
                <FilterLabel>To</FilterLabel>
                <input type="date" value={filters.until} onChange={e => patchFilters({ until: e.target.value })} className={filterInputClass} />
              </label>
              <label className="space-y-1">
                <FilterLabel>Agent</FilterLabel>
                <select value={filters.agentId} onChange={e => patchFilters({ agentId: e.target.value })} className={filterInputClass}>
                  <option value="">All agents</option>
                  {agents.map(a => <option key={a.id} value={a.id}>{a.name}</option>)}
                </select>
              </label>
              <label className="space-y-1">
                <FilterLabel>Decision</FilterLabel>
                <select value={filters.decision} onChange={e => patchFilters({ decision: e.target.value })} className={filterInputClass}>
                  <option value="">All decisions</option>
                  {DECISIONS.filter(Boolean).map(d => <option key={d} value={d}>{d}</option>)}
                </select>
              </label>
            </div>

            {!showExpertPanel ? (
              <button type="button" onClick={() => setShowExpert(true)} className="dev-text-link">
                advanced filters
              </button>
            ) : (
              <div className="space-y-3 pt-1 border-t border-border-subtle">
                <div className="flex items-center justify-between gap-2">
                  <span className="ds-overline">Advanced</span>
                  {!expertFiltersActive && (
                    <button type="button" onClick={() => setShowExpert(false)} className="dev-text-link">hide</button>
                  )}
                </div>
                <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
                  <label className="space-y-1">
                    <FilterLabel>Task ID</FilterLabel>
                    <input value={filters.taskId} onChange={e => patchFilters({ taskId: e.target.value })} placeholder="partial or full id" className={filterInputClass} />
                  </label>
                  <label className="space-y-1">
                    <FilterLabel>Data origin</FilterLabel>
                    <select value={filters.dataOrigin} onChange={e => patchFilters({ dataOrigin: e.target.value })} className={filterInputClass}>
                      {DATA_ORIGINS.map(o => <option key={o || 'all'} value={o}>{o || 'All origins'}</option>)}
                    </select>
                  </label>
                  <label className="space-y-1">
                    <FilterLabel>Request ID</FilterLabel>
                    <input value={filters.requestId} onChange={e => patchFilters({ requestId: e.target.value })} placeholder="search request id" className={filterInputClass} />
                  </label>
                </div>
                <div className="flex flex-wrap items-center gap-4">
                  <label className="flex items-center gap-2 text-sm text-text-secondary cursor-pointer">
                    <input type="checkbox" checked={filters.includeRuntime} onChange={e => patchFilters({ includeRuntime: e.target.checked })} disabled={!runtimeActivityUI} className="rounded-sm border-border-default" />
                    Include runtime activity
                  </label>
                  <label className="flex items-center gap-2 text-sm text-text-secondary cursor-pointer">
                    <input type="checkbox" checked={filters.safetyOnly} onChange={e => patchFilters({ safetyOnly: e.target.checked })} className="rounded-sm border-border-default" />
                    Safety flagged only
                  </label>
                </div>
              </div>
            )}
          </div>
        )}

        {activeFilterCount > 0 && (
          <div className="flex justify-end">
            <button type="button" onClick={clearFilters} className="dev-text-link">clear all filters</button>
          </div>
        )}
      </div>

      {/* Data table — always rendered */}
      <div className="overflow-x-auto">
        <table className="w-full min-w-[800px]">
          <thead className="ds-table-head">
            <tr>
              <th className="px-4 py-2.5 text-left font-medium">Time</th>
              <th className="px-4 py-2.5 text-left font-medium">{summaryHeading}</th>
              <th className="px-4 py-2.5 text-left font-medium">Agent</th>
              <th className="px-4 py-2.5 text-left font-medium">Decision</th>
              <th className="px-4 py-2.5 text-left font-medium">Outcome</th>
              <th className="px-4 py-2.5 text-left font-medium">Duration</th>
              <th className="px-4 py-2.5 w-8" />
            </tr>
          </thead>
          <tbody>
            {isLoading && (
              <tr>
                <td colSpan={COL_COUNT} className="px-4 py-10 text-center ds-page-loading">
                  loading history…
                </td>
              </tr>
            )}
            {!isLoading && entries.length === 0 && (
              <tr>
                <td colSpan={COL_COUNT} className="px-4 py-10 text-center ds-page-loading">
                  {emptyMessage}
                </td>
              </tr>
            )}
            {!isLoading && entries.map(entry => (
              <AuditRow
                key={entry.id}
                entry={entry}
                mode={mode}
                agentName={entry.agent_id ? agentMap.get(entry.agent_id) : undefined}
                canCreateRule={runtimePolicyUI || !entry.service.startsWith('runtime.')}
                canMute={fullRuntimeActive}
                onCreateRule={onCreateRule}
                onMute={onMute}
              />
            ))}
          </tbody>
        </table>
      </div>

      {/* Table footer / pagination */}
      {total > PAGE_SIZE && (
        <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-2 px-4 py-2.5 border-t border-border-default bg-surface-1 text-xs text-text-tertiary">
          <span>showing {offset + 1}–{Math.min(offset + PAGE_SIZE, total)} of {total}</span>
          <div className="flex gap-2">
            <button
              type="button"
              disabled={offset === 0}
              onClick={() => setOffset(Math.max(0, offset - PAGE_SIZE))}
              className="dev-btn-ghost disabled:opacity-40"
            >
              previous
            </button>
            <button
              type="button"
              disabled={offset + PAGE_SIZE >= total}
              onClick={() => setOffset(offset + PAGE_SIZE)}
              className="dev-btn-ghost disabled:opacity-40"
            >
              next
            </button>
          </div>
        </div>
      )}
    </section>
  )
}
