import { useQuery } from '@tanstack/react-query'
import { useAuth } from '../hooks/useAuth'

interface CustomAdapter {
  id: string
  service_id: string
  name: string
  auth_type: string
  created_at: string
}

export default function OrgAdapters() {
  const { currentOrg } = useAuth()
  const orgId = currentOrg?.id ?? ''

  const { data: adapters } = useQuery({
    queryKey: ['org-adapters', orgId],
    queryFn: async () => {
      const res = await fetch(`/api/orgs/${orgId}/adapters`)
      if (!res.ok) throw new Error('Failed to fetch custom adapters')
      return res.json() as Promise<{ adapters: CustomAdapter[] }>
    },
    enabled: !!orgId,
  })

  if (!currentOrg) {
    return <p className="text-sm text-text-secondary">Select an organization to manage custom adapters.</p>
  }

  return (
    <div className="space-y-4">
      <h2 className="text-lg font-semibold text-text-primary">
        Custom Adapters &mdash; {currentOrg.name}
      </h2>
      <p className="text-sm text-text-secondary">
        Register custom HTTP API adapters for your organization's internal APIs.
        Service IDs must start with <code className="font-mono text-xs bg-surface-0 px-1 rounded">custom.</code>
      </p>

      <div className="space-y-2">
        {adapters?.adapters?.map((a: CustomAdapter) => (
          <div key={a.id} className="bg-surface-1 rounded-lg border border-border-default p-3 flex items-center justify-between">
            <div>
              <span className="text-sm font-medium text-text-primary">{a.name}</span>
              <span className="ml-2 text-xs text-text-secondary font-mono">{a.service_id}</span>
            </div>
            <span className="text-xs text-text-secondary">auth: {a.auth_type}</span>
          </div>
        ))}
        {(!adapters?.adapters || adapters.adapters.length === 0) && (
          <p className="text-sm text-text-secondary">No custom adapters registered.</p>
        )}
      </div>
    </div>
  )
}
