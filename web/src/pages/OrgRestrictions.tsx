import { useState } from 'react'
import { useQuery, useMutation } from '@tanstack/react-query'
import { api } from '../api/client'
import { useAuth } from '../hooks/useAuth'

export default function OrgRestrictions() {
  const { currentOrg } = useAuth()
  const [service, setService] = useState('')
  const [action, setAction] = useState('*')
  const [reason, setReason] = useState('')

  const orgId = currentOrg?.id ?? ''

  const { data: restrictions, refetch } = useQuery({
    queryKey: ['org-restrictions', orgId],
    queryFn: () => api.orgs.restrictions.list(orgId),
    enabled: !!orgId,
  })

  const create = useMutation({
    mutationFn: () => api.orgs.restrictions.create(orgId, service, action, reason),
    onSuccess: () => {
      setService('')
      setAction('*')
      setReason('')
      refetch()
    },
  })

  const remove = useMutation({
    mutationFn: (id: string) => api.orgs.restrictions.delete(orgId, id),
    onSuccess: () => refetch(),
  })

  if (!currentOrg) {
    return <p className="text-sm text-text-secondary">Select an organization to manage restrictions.</p>
  }

  return (
    <div className="space-y-6">
      <h2 className="text-lg font-semibold text-text-primary">
        Org Restrictions &mdash; {currentOrg.name}
      </h2>

      <div className="bg-surface-1 rounded-lg border border-border-default p-4">
        <h3 className="text-sm font-medium text-text-primary mb-3">Add Restriction</h3>
        <div className="flex gap-3 flex-wrap">
          <input
            value={service}
            onChange={(e) => setService(e.target.value)}
            placeholder="Service (e.g. google.gmail)"
            className="flex-1 min-w-[200px] px-3 py-2 text-sm rounded-md border border-border-default bg-surface-0 text-text-primary"
          />
          <input
            value={action}
            onChange={(e) => setAction(e.target.value)}
            placeholder="Action (* for all)"
            className="w-40 px-3 py-2 text-sm rounded-md border border-border-default bg-surface-0 text-text-primary"
          />
          <input
            value={reason}
            onChange={(e) => setReason(e.target.value)}
            placeholder="Reason (optional)"
            className="flex-1 min-w-[200px] px-3 py-2 text-sm rounded-md border border-border-default bg-surface-0 text-text-primary"
          />
          <button
            onClick={() => create.mutate()}
            disabled={!service || create.isPending}
            className="px-4 py-2 text-sm font-medium rounded-md bg-accent-primary text-white hover:opacity-90 disabled:opacity-50"
          >
            Add
          </button>
        </div>
      </div>

      <div className="space-y-2">
        {restrictions?.map((r) => (
          <div key={r.id} className="bg-surface-1 rounded-lg border border-border-default p-3 flex items-center justify-between">
            <div>
              <span className="text-sm font-mono text-text-primary">{r.service}:{r.action}</span>
              {r.reason && <span className="ml-2 text-xs text-text-secondary">{r.reason}</span>}
            </div>
            <button
              onClick={() => remove.mutate(r.id)}
              className="text-xs px-2 py-1 rounded border border-red-300 text-red-600 hover:bg-red-50 dark:border-red-800 dark:text-red-400 dark:hover:bg-red-900/20"
            >
              Remove
            </button>
          </div>
        ))}
        {(!restrictions || restrictions.length === 0) && (
          <p className="text-sm text-text-secondary">No org-level restrictions.</p>
        )}
      </div>
    </div>
  )
}
