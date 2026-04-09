import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { api, type AuditEntry } from '../api/client'
import { useAuth } from '../hooks/useAuth'

export default function OrgAudit() {
  const { currentOrg } = useAuth()
  const [offset, setOffset] = useState(0)
  const limit = 50

  const orgId = currentOrg?.id ?? ''

  const { data } = useQuery({
    queryKey: ['org-audit', orgId, offset],
    queryFn: () => api.orgs.audit(orgId, { limit, offset }),
    enabled: !!orgId,
  })

  if (!currentOrg) {
    return <p className="text-sm text-text-secondary">Select an organization to view audit log.</p>
  }

  const entries = data?.entries ?? []
  const total = data?.total ?? 0

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <h2 className="text-lg font-semibold text-text-primary">
          Org Audit Log &mdash; {currentOrg.name}
        </h2>
        <span className="text-xs text-text-secondary">{total} entries</span>
      </div>

      <div className="overflow-x-auto">
        <table className="w-full text-sm">
          <thead>
            <tr className="border-b border-border-default text-left text-text-secondary">
              <th className="pb-2 pr-4 font-medium">Time</th>
              <th className="pb-2 pr-4 font-medium">Service</th>
              <th className="pb-2 pr-4 font-medium">Action</th>
              <th className="pb-2 pr-4 font-medium">Decision</th>
              <th className="pb-2 font-medium">Outcome</th>
            </tr>
          </thead>
          <tbody>
            {entries.map((e: AuditEntry) => (
              <tr key={e.id} className="border-b border-border-default/50">
                <td className="py-2 pr-4 text-text-secondary whitespace-nowrap">
                  {new Date(e.timestamp).toLocaleString()}
                </td>
                <td className="py-2 pr-4 font-mono text-text-primary">{e.service}</td>
                <td className="py-2 pr-4 text-text-primary">{e.action}</td>
                <td className="py-2 pr-4 text-text-primary">{e.decision}</td>
                <td className="py-2">
                  <span className={`text-xs px-1.5 py-0.5 rounded ${
                    e.outcome === 'executed' ? 'bg-green-100 text-green-700 dark:bg-green-900/30 dark:text-green-400' :
                    e.outcome === 'blocked' ? 'bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-400' :
                    'bg-surface-0 text-text-secondary'
                  }`}>
                    {e.outcome}
                  </span>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      {total > limit && (
        <div className="flex justify-center gap-2">
          <button
            onClick={() => setOffset(Math.max(0, offset - limit))}
            disabled={offset === 0}
            className="px-3 py-1 text-sm rounded border border-border-default disabled:opacity-50 text-text-primary"
          >
            Previous
          </button>
          <button
            onClick={() => setOffset(offset + limit)}
            disabled={offset + limit >= total}
            className="px-3 py-1 text-sm rounded border border-border-default disabled:opacity-50 text-text-primary"
          >
            Next
          </button>
        </div>
      )}
    </div>
  )
}
