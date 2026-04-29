import { useState, useCallback } from 'react'
import { useQuery } from '@tanstack/react-query'
import { api, type AuditEntry } from '../api/client'
import { useAuth } from '../hooks/useAuth'
import { formatDistanceToNow, format } from 'date-fns'
import { serviceName, actionName, formatServiceAction } from '../lib/services'

function escapeCsvField(value: string): string {
  if (value.includes(',') || value.includes('"') || value.includes('\n')) {
    return `"${value.replace(/"/g, '""')}"`
  }
  return value
}

function entriesToCsv(entries: AuditEntry[]): string {
  const headers = ['Timestamp', 'Service', 'Action', 'Decision', 'Outcome', 'Duration (ms)', 'Reason', 'Data Origin', 'Error', 'Policy', 'Safety Flagged', 'Safety Reason', 'Request ID']
  const rows = entries.map(e => [
    e.timestamp,
    serviceName(e.service),
    actionName(e.action),
    e.decision,
    e.outcome,
    String(e.duration_ms),
    e.reason ?? '',
    e.data_origin ?? '',
    e.error_msg ?? '',
    e.policy_id ?? '',
    e.safety_flagged ? 'yes' : 'no',
    e.safety_reason ?? '',
    e.request_id,
  ].map(escapeCsvField))
  return [headers.join(','), ...rows.map(r => r.join(','))].join('\n')
}

function downloadCsv(csv: string, filename: string) {
  const blob = new Blob([csv], { type: 'text/csv;charset=utf-8;' })
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  a.click()
  URL.revokeObjectURL(url)
}

const OUTCOMES = ['', 'executed', 'blocked', 'restricted', 'pending', 'denied', 'error', 'timeout']

const OUTCOME_STYLE: Record<string, string> = {
  executed: 'bg-success/15 text-success',
  blocked: 'bg-danger/15 text-danger',
  restricted: 'bg-warning/15 text-warning',
  pending: 'bg-warning/15 text-warning',
  denied: 'bg-surface-2 text-text-tertiary',
  error: 'bg-danger/15 text-danger',
  timeout: 'bg-surface-2 text-text-tertiary',
}

function OutcomeBadge({ outcome }: { outcome: string }) {
  return (
    <span className={`inline-block px-2 py-0.5 rounded text-xs font-medium ${OUTCOME_STYLE[outcome] ?? 'bg-surface-2 text-text-tertiary'}`}>
      {outcome}
    </span>
  )
}

function compactID(value?: string) {
  if (!value) return null
  return value.length > 12 ? `${value.slice(0, 8)}…${value.slice(-4)}` : value
}

function AuditRow({ entry }: { entry: AuditEntry }) {
  const [expanded, setExpanded] = useState(false)
  return (
    <>
      <tr
        className="border-t border-border-default hover:bg-surface-2 cursor-pointer"
        onClick={() => setExpanded(e => !e)}
      >
        <td className="px-4 py-2 text-xs text-text-tertiary whitespace-nowrap" title={format(new Date(entry.timestamp), 'PPpp')}>
          {formatDistanceToNow(new Date(entry.timestamp), { addSuffix: true })}
        </td>
        <td className="px-4 py-2 text-sm">
          <span className="inline-flex items-center gap-1.5">
            <span className="w-2 h-2 rounded-full shrink-0 bg-text-tertiary" />
            {serviceName(entry.service)}
          </span>
        </td>
        <td className="px-4 py-2 text-sm">{actionName(entry.action)}</td>
        <td className="px-4 py-2">
          <span className={`text-xs px-1.5 py-0.5 rounded ${entry.decision === 'block' ? 'bg-danger/10 text-danger' : entry.decision === 'approve' ? 'bg-warning/10 text-warning' : entry.decision === 'verify' ? 'bg-brand/10 text-brand' : 'bg-success/10 text-success'}`}>
            {entry.decision}
          </span>
        </td>
        <td className="px-4 py-2"><OutcomeBadge outcome={entry.outcome} /></td>
        <td className="px-4 py-2 text-xs text-text-tertiary">{entry.duration_ms}ms</td>
        <td className="px-4 py-2 text-xs text-text-secondary">{expanded ? '▲' : '▼'}</td>
      </tr>
      {expanded && (
        <tr className="border-t border-border-default bg-surface-2">
          <td colSpan={7} className="px-4 py-3">
            <div className="grid grid-cols-1 sm:grid-cols-2 gap-4 text-xs">
              <div>
                <div className="text-text-tertiary font-medium mb-1">
                  {formatServiceAction(entry.service, entry.action)}
                </div>
                <pre className="bg-surface-1 border border-border-default rounded p-2 overflow-auto max-h-48 text-text-secondary">
                  {JSON.stringify(entry.params_safe, null, 2)}
                </pre>
              </div>
              <div className="space-y-2">
                {entry.reason && (
                  <div className="bg-brand/10 rounded p-2">
                    <div className="text-brand font-medium mb-0.5">Agent's reason</div>
                    <div className="text-text-secondary">{entry.reason}</div>
                  </div>
                )}
                {entry.data_origin && (
                  <div><span className="text-text-tertiary">Data origin:</span> {entry.data_origin}</div>
                )}
                {entry.error_msg && (
                  <div><span className="text-text-tertiary">Error:</span> <span className="text-danger">{entry.error_msg}</span></div>
                )}
                {entry.policy_id && (
                  <div><span className="text-text-tertiary">Policy:</span> {entry.policy_id}</div>
                )}
                {entry.safety_flagged && (
                  <div className="text-warning">Safety flagged{entry.safety_reason ? `: ${entry.safety_reason}` : ''}</div>
                )}
                {entry.verification && (
                  <div className="bg-brand/10 rounded p-2 space-y-1">
                    <div className="text-brand font-medium mb-0.5">Intent verification</div>
                    <div className="flex gap-2 flex-wrap">
                      <span className={`inline-block px-1.5 py-0.5 rounded text-xs font-medium ${
                        entry.verification.param_scope === 'ok' ? 'bg-success/15 text-success' :
                        entry.verification.param_scope === 'violation' ? 'bg-danger/15 text-danger' :
                        'bg-surface-2 text-text-tertiary'
                      }`}>
                        params: {entry.verification.param_scope}
                      </span>
                      <span className={`inline-block px-1.5 py-0.5 rounded text-xs font-medium ${
                        entry.verification.reason_coherence === 'ok' ? 'bg-success/15 text-success' :
                        entry.verification.reason_coherence === 'incoherent' ? 'bg-danger/15 text-danger' :
                        'bg-warning/15 text-warning'
                      }`}>
                        reason: {entry.verification.reason_coherence}
                      </span>
                      {entry.verification.cached && (
                        <span className="inline-block px-1.5 py-0.5 rounded text-xs bg-surface-2 text-text-tertiary">cached</span>
                      )}
                    </div>
                    <div className="text-text-secondary">{entry.verification.explanation}</div>
                    <div className="text-text-tertiary text-[10px]">{entry.verification.model} &middot; {entry.verification.latency_ms}ms</div>
                  </div>
                )}
                {(entry.session_id || entry.approval_id || entry.lease_id || entry.matched_task_id || entry.lease_task_id) && (
                  <div className="bg-surface-1 border border-border-default rounded p-2 space-y-1">
                    <div className="text-text-primary font-medium">Runtime context</div>
                    {entry.session_id && <div><span className="text-text-tertiary">Session:</span> <code className="font-mono">{compactID(entry.session_id)}</code></div>}
                    {entry.approval_id && <div><span className="text-text-tertiary">Approval:</span> <code className="font-mono">{compactID(entry.approval_id)}</code></div>}
                    {entry.lease_id && <div><span className="text-text-tertiary">Lease:</span> <code className="font-mono">{compactID(entry.lease_id)}</code></div>}
                    {entry.matched_task_id && <div><span className="text-text-tertiary">Matched task:</span> <code className="font-mono">{compactID(entry.matched_task_id)}</code></div>}
                    {entry.lease_task_id && <div><span className="text-text-tertiary">Lease task:</span> <code className="font-mono">{compactID(entry.lease_task_id)}</code></div>}
                    {entry.resolution_confidence && <div><span className="text-text-tertiary">Confidence:</span> {entry.resolution_confidence}</div>}
                    {entry.intent_verdict && <div><span className="text-text-tertiary">Intent verdict:</span> {entry.intent_verdict}</div>}
                  </div>
                )}
                {(entry.would_block || entry.would_review || entry.would_prompt_inline) && (
                  <div className="bg-warning/10 rounded p-2 space-y-1">
                    <div className="text-warning font-medium">Observation mode</div>
                    <div className="flex gap-2 flex-wrap">
                      {entry.would_block && <span className="inline-block px-1.5 py-0.5 rounded text-xs font-medium bg-danger/15 text-danger">would block</span>}
                      {entry.would_review && <span className="inline-block px-1.5 py-0.5 rounded text-xs font-medium bg-warning/15 text-warning">would review</span>}
                      {entry.would_prompt_inline && <span className="inline-block px-1.5 py-0.5 rounded text-xs font-medium bg-brand/15 text-brand">would prompt inline</span>}
                    </div>
                    <div className="text-text-tertiary">
                      {entry.used_active_task_context && 'Used active task context. '}
                      {entry.used_lease_bias && 'Used lease bias. '}
                      {entry.used_conv_judge_resolution && 'Used conversation judge resolution.'}
                    </div>
                  </div>
                )}
                <div className="text-text-tertiary font-mono">{entry.request_id}</div>
              </div>
            </div>
          </td>
        </tr>
      )}
    </>
  )
}

const PAGE_SIZE = 50

export default function Audit() {
  const { currentOrg } = useAuth()
  const orgId = currentOrg?.id
  const [outcomeFilter, setOutcomeFilter] = useState('')
  const [serviceFilter, setServiceFilter] = useState('')
  const [offset, setOffset] = useState(0)

  const filter = {
    outcome: outcomeFilter || undefined,
    service: serviceFilter || undefined,
    limit: PAGE_SIZE,
    offset,
  }

  const { data, isLoading, refetch } = useQuery({
    queryKey: ['audit', orgId ?? 'personal', { outcome: outcomeFilter, service: serviceFilter, offset }],
    queryFn: () => orgId
      ? api.orgs.audit(orgId, filter)
      : api.audit.list(filter),
    refetchInterval: 30_000,
  })

  const entries = data?.entries ?? []
  const total = data?.total ?? 0
  const [exporting, setExporting] = useState(false)

  const handleExport = useCallback(async () => {
    setExporting(true)
    try {
      const allEntries: AuditEntry[] = []
      const batchSize = 200
      let batchOffset = 0
      while (true) {
        const batchFilter = {
          outcome: outcomeFilter || undefined,
          service: serviceFilter || undefined,
          limit: batchSize,
          offset: batchOffset,
        }
        const batch = orgId
          ? await api.orgs.audit(orgId, batchFilter)
          : await api.audit.list(batchFilter)
        allEntries.push(...batch.entries)
        if (allEntries.length >= batch.total || batch.entries.length < batchSize) break
        batchOffset += batchSize
      }
      const csv = entriesToCsv(allEntries)
      const dateStr = format(new Date(), 'yyyy-MM-dd')
      downloadCsv(csv, `audit-log-${dateStr}.csv`)
    } finally {
      setExporting(false)
    }
  }, [outcomeFilter, serviceFilter, orgId])

  return (
    <div className="p-4 sm:p-8 space-y-4">
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-2">
        <h1 className="text-2xl font-bold text-text-primary">{orgId ? `${currentOrg!.name} Audit` : 'Audit'}</h1>
        <div className="flex items-center gap-3">
          <button
            onClick={handleExport}
            disabled={exporting || entries.length === 0}
            className="text-sm text-brand hover:underline disabled:opacity-40 disabled:no-underline"
          >
            {exporting ? 'Exporting…' : 'Export CSV'}
          </button>
          <button
            onClick={() => refetch()}
            className="text-sm text-brand hover:underline"
          >
            Refresh
          </button>
        </div>
      </div>

      {/* Filters */}
      <div className="flex gap-3 flex-wrap">
        <select
          value={outcomeFilter}
          onChange={e => { setOutcomeFilter(e.target.value); setOffset(0) }}
          className="text-sm rounded border border-border-default bg-surface-0 text-text-primary px-2 py-1.5 focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand"
        >
          <option value="">All outcomes</option>
          {OUTCOMES.filter(Boolean).map(o => (
            <option key={o} value={o}>{o}</option>
          ))}
        </select>
        <input
          value={serviceFilter}
          onChange={e => { setServiceFilter(e.target.value); setOffset(0) }}
          placeholder="Filter by service…"
          className="text-sm rounded border border-border-default bg-surface-0 text-text-primary px-2 py-1.5 focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand placeholder:text-text-tertiary"
        />
      </div>

      {isLoading && <div className="text-sm text-text-tertiary">Loading…</div>}

      {!isLoading && entries.length === 0 && (
        <div className="text-sm text-text-tertiary py-8 text-center">
          {outcomeFilter || serviceFilter
            ? 'No entries match your filters.'
            : "No activity yet. Your agent's requests will be logged here."}
        </div>
      )}

      {entries.length > 0 && (
        <div className="bg-surface-1 border border-border-default rounded-md overflow-x-auto">
          <table className="w-full min-w-[640px]">
            <thead className="bg-surface-2 text-xs text-text-tertiary font-medium">
              <tr>
                <th className="px-4 py-2 text-left">Time</th>
                <th className="px-4 py-2 text-left">Service</th>
                <th className="px-4 py-2 text-left">Action</th>
                <th className="px-4 py-2 text-left">Authorization</th>
                <th className="px-4 py-2 text-left">Outcome</th>
                <th className="px-4 py-2 text-left">Duration</th>
                <th className="px-4 py-2"></th>
              </tr>
            </thead>
            <tbody>
              {entries.map(e => <AuditRow key={e.id} entry={e} />)}
            </tbody>
          </table>
        </div>
      )}

      {/* Pagination */}
      {total > PAGE_SIZE && (
        <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-2 text-sm text-text-tertiary">
          <span>Showing {offset + 1}–{Math.min(offset + PAGE_SIZE, total)} of {total}</span>
          <div className="flex gap-2">
            <button
              disabled={offset === 0}
              onClick={() => setOffset(Math.max(0, offset - PAGE_SIZE))}
              className="px-3 py-1 rounded border border-border-strong disabled:opacity-40 hover:bg-surface-2"
            >
              Previous
            </button>
            <button
              disabled={offset + PAGE_SIZE >= total}
              onClick={() => setOffset(offset + PAGE_SIZE)}
              className="px-3 py-1 rounded border border-border-strong disabled:opacity-40 hover:bg-surface-2"
            >
              Next
            </button>
          </div>
        </div>
      )}
    </div>
  )
}
