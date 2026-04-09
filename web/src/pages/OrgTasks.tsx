import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { api, type Task } from '../api/client'
import { useAuth } from '../hooks/useAuth'

export default function OrgTasks() {
  const { currentOrg } = useAuth()
  const [status, setStatus] = useState('')
  const [offset, setOffset] = useState(0)
  const limit = 25

  const orgId = currentOrg?.id ?? ''

  const { data } = useQuery({
    queryKey: ['org-tasks', orgId, status, offset],
    queryFn: () => api.orgs.tasks(orgId, { status: status || undefined, limit, offset }),
    enabled: !!orgId,
  })

  if (!currentOrg) {
    return <p className="text-sm text-text-secondary">Select an organization to view tasks.</p>
  }

  const tasks = data?.tasks ?? []
  const total = data?.total ?? 0
  const statuses = ['', 'active', 'pending_approval', 'completed', 'expired', 'denied', 'revoked']

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <h2 className="text-lg font-semibold text-text-primary">
          Org Tasks &mdash; {currentOrg.name}
        </h2>
        <select
          value={status}
          onChange={(e) => { setStatus(e.target.value); setOffset(0) }}
          className="text-sm px-3 py-1.5 rounded-md border border-border-default bg-surface-0 text-text-primary"
        >
          {statuses.map((s) => (
            <option key={s} value={s}>{s || 'All'}</option>
          ))}
        </select>
      </div>

      <div className="space-y-2">
        {tasks.map((t: Task) => (
          <div key={t.id} className="bg-surface-1 rounded-lg border border-border-default p-3">
            <div className="flex items-center justify-between">
              <span className="text-sm font-medium text-text-primary">{t.purpose}</span>
              <span className={`text-xs px-1.5 py-0.5 rounded ${
                t.status === 'active' ? 'bg-green-100 text-green-700 dark:bg-green-900/30 dark:text-green-400' :
                t.status === 'revoked' || t.status === 'denied' ? 'bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-400' :
                'bg-surface-0 text-text-secondary'
              }`}>
                {t.status}
              </span>
            </div>
            <div className="mt-1 text-xs text-text-secondary">
              {t.authorized_actions.length} action{t.authorized_actions.length !== 1 ? 's' : ''} &middot;
              {t.request_count} request{t.request_count !== 1 ? 's' : ''} &middot;
              {t.lifetime}
            </div>
          </div>
        ))}
        {tasks.length === 0 && (
          <p className="text-sm text-text-secondary">No tasks found.</p>
        )}
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
