import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { api, type AuditEntry } from '../api/client'
import { formatDistanceToNow, format } from 'date-fns'
import { serviceName, actionName, serviceBrand, formatServiceAction } from '../lib/services'

const OUTCOMES = ['', 'executed', 'blocked', 'restricted', 'pending', 'denied', 'error', 'timeout']

const OUTCOME_STYLE: Record<string, string> = {
  executed: 'bg-green-100 text-green-800',
  blocked: 'bg-red-100 text-red-800',
  restricted: 'bg-orange-100 text-orange-800',
  pending: 'bg-yellow-100 text-yellow-800',
  denied: 'bg-gray-100 text-gray-600',
  error: 'bg-red-100 text-red-700',
  timeout: 'bg-gray-100 text-gray-500',
}

function OutcomeBadge({ outcome }: { outcome: string }) {
  return (
    <span className={`inline-block px-2 py-0.5 rounded-full text-xs font-medium ${OUTCOME_STYLE[outcome] ?? 'bg-gray-100 text-gray-600'}`}>
      {outcome}
    </span>
  )
}

function AuditRow({ entry }: { entry: AuditEntry }) {
  const [expanded, setExpanded] = useState(false)
  return (
    <>
      <tr
        className="border-t hover:bg-gray-50 cursor-pointer"
        onClick={() => setExpanded(e => !e)}
      >
        <td className="px-4 py-2 text-xs text-gray-400 whitespace-nowrap" title={format(new Date(entry.timestamp), 'PPpp')}>
          {formatDistanceToNow(new Date(entry.timestamp), { addSuffix: true })}
        </td>
        <td className="px-4 py-2 text-sm">
          <span className="inline-flex items-center gap-1.5">
            <span className={`w-2 h-2 rounded-full shrink-0 ${serviceBrand(entry.service).dot}`} />
            {serviceName(entry.service)}
          </span>
        </td>
        <td className="px-4 py-2 text-sm">{actionName(entry.action)}</td>
        <td className="px-4 py-2">
          <span className={`text-xs px-1.5 py-0.5 rounded ${entry.decision === 'block' ? 'bg-red-50 text-red-600' : entry.decision === 'approve' ? 'bg-yellow-50 text-yellow-700' : entry.decision === 'verify' ? 'bg-purple-50 text-purple-600' : 'bg-green-50 text-green-700'}`}>
            {entry.decision}
          </span>
        </td>
        <td className="px-4 py-2"><OutcomeBadge outcome={entry.outcome} /></td>
        <td className="px-4 py-2 text-xs text-gray-400">{entry.duration_ms}ms</td>
        <td className="px-4 py-2 text-xs text-gray-300">{expanded ? '▲' : '▼'}</td>
      </tr>
      {expanded && (
        <tr className="border-t bg-gray-50">
          <td colSpan={7} className="px-4 py-3">
            <div className="grid grid-cols-2 gap-4 text-xs">
              <div>
                <div className="text-gray-500 font-medium mb-1">
                  {formatServiceAction(entry.service, entry.action)}
                </div>
                <pre className="bg-white border rounded p-2 overflow-auto max-h-48 text-gray-700">
                  {JSON.stringify(entry.params_safe, null, 2)}
                </pre>
              </div>
              <div className="space-y-2">
                {entry.reason && (
                  <div className="bg-blue-50 rounded p-2">
                    <div className="text-blue-600 font-medium mb-0.5">Agent's reason</div>
                    <div className="text-gray-700">{entry.reason}</div>
                  </div>
                )}
                {entry.data_origin && (
                  <div><span className="text-gray-500">Data origin:</span> {entry.data_origin}</div>
                )}
                {entry.error_msg && (
                  <div><span className="text-gray-500">Error:</span> <span className="text-red-600">{entry.error_msg}</span></div>
                )}
                {entry.policy_id && (
                  <div><span className="text-gray-500">Policy:</span> {entry.policy_id}</div>
                )}
                {entry.safety_flagged && (
                  <div className="text-orange-600">Safety flagged{entry.safety_reason ? `: ${entry.safety_reason}` : ''}</div>
                )}
                {entry.verification && (
                  <div className="bg-purple-50 rounded p-2 space-y-1">
                    <div className="text-purple-700 font-medium mb-0.5">Intent verification</div>
                    <div className="flex gap-2 flex-wrap">
                      <span className={`inline-block px-1.5 py-0.5 rounded text-xs font-medium ${
                        entry.verification.param_scope === 'ok' ? 'bg-green-100 text-green-700' :
                        entry.verification.param_scope === 'violation' ? 'bg-red-100 text-red-700' :
                        'bg-gray-100 text-gray-500'
                      }`}>
                        params: {entry.verification.param_scope}
                      </span>
                      <span className={`inline-block px-1.5 py-0.5 rounded text-xs font-medium ${
                        entry.verification.reason_coherence === 'ok' ? 'bg-green-100 text-green-700' :
                        entry.verification.reason_coherence === 'incoherent' ? 'bg-red-100 text-red-700' :
                        'bg-orange-100 text-orange-700'
                      }`}>
                        reason: {entry.verification.reason_coherence}
                      </span>
                      {entry.verification.cached && (
                        <span className="inline-block px-1.5 py-0.5 rounded text-xs bg-gray-100 text-gray-500">cached</span>
                      )}
                    </div>
                    <div className="text-gray-700">{entry.verification.explanation}</div>
                    <div className="text-gray-400 text-[10px]">{entry.verification.model} &middot; {entry.verification.latency_ms}ms</div>
                  </div>
                )}
                <div className="text-gray-400 font-mono">{entry.request_id}</div>
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
  const [outcomeFilter, setOutcomeFilter] = useState('')
  const [serviceFilter, setServiceFilter] = useState('')
  const [offset, setOffset] = useState(0)

  const { data, isLoading, refetch } = useQuery({
    queryKey: ['audit', { outcome: outcomeFilter, service: serviceFilter, offset }],
    queryFn: () => api.audit.list({
      outcome: outcomeFilter || undefined,
      service: serviceFilter || undefined,
      limit: PAGE_SIZE,
      offset,
    }),
    refetchInterval: 30_000,
  })

  const entries = data?.entries ?? []
  const total = data?.total ?? 0

  return (
    <div className="p-8 space-y-4">
      <div className="flex items-center justify-between">
        <h1 className="text-2xl font-bold text-gray-900">Gateway Log</h1>
        <button
          onClick={() => refetch()}
          className="text-sm text-blue-600 hover:underline"
        >
          Refresh
        </button>
      </div>

      {/* Filters */}
      <div className="flex gap-3 flex-wrap">
        <select
          value={outcomeFilter}
          onChange={e => { setOutcomeFilter(e.target.value); setOffset(0) }}
          className="text-sm rounded border border-gray-300 px-2 py-1.5 focus:outline-none focus:ring-1 focus:ring-blue-400"
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
          className="text-sm rounded border border-gray-300 px-2 py-1.5 focus:outline-none focus:ring-1 focus:ring-blue-400"
        />
      </div>

      {isLoading && <div className="text-sm text-gray-400">Loading…</div>}

      {!isLoading && entries.length === 0 && (
        <div className="text-sm text-gray-400 py-8 text-center">
          {outcomeFilter || serviceFilter
            ? 'No entries match your filters.'
            : "No activity yet. Your agent's requests will be logged here."}
        </div>
      )}

      {entries.length > 0 && (
        <div className="bg-white border rounded-lg overflow-hidden">
          <table className="w-full">
            <thead className="bg-gray-50 text-xs text-gray-500 font-medium">
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
        <div className="flex items-center justify-between text-sm text-gray-500">
          <span>Showing {offset + 1}–{Math.min(offset + PAGE_SIZE, total)} of {total}</span>
          <div className="flex gap-2">
            <button
              disabled={offset === 0}
              onClick={() => setOffset(Math.max(0, offset - PAGE_SIZE))}
              className="px-3 py-1 rounded border border-gray-300 disabled:opacity-40 hover:bg-gray-50"
            >
              Previous
            </button>
            <button
              disabled={offset + PAGE_SIZE >= total}
              onClick={() => setOffset(offset + PAGE_SIZE)}
              className="px-3 py-1 rounded border border-gray-300 disabled:opacity-40 hover:bg-gray-50"
            >
              Next
            </button>
          </div>
        </div>
      )}
    </div>
  )
}
