import { useQuery } from '@tanstack/react-query'
import { useAuth } from '../hooks/useAuth'

interface OrgService {
  service_id: string
  name: string
  status: 'active' | 'inactive'
  credential_type: 'shared' | 'per_user' | 'none'
}

export default function OrgServices() {
  const { currentOrg } = useAuth()
  const orgId = currentOrg?.id ?? ''

  const { data: services } = useQuery({
    queryKey: ['org-services', orgId],
    queryFn: async () => {
      const res = await fetch(`/api/orgs/${orgId}/services`)
      if (!res.ok) throw new Error('Failed to fetch org services')
      return res.json() as Promise<{ services: OrgService[] }>
    },
    enabled: !!orgId,
  })

  if (!currentOrg) {
    return <p className="text-sm text-text-secondary">Select an organization to manage services.</p>
  }

  return (
    <div className="space-y-4">
      <h2 className="text-lg font-semibold text-text-primary">
        Org Services &mdash; {currentOrg.name}
      </h2>
      <p className="text-sm text-text-secondary">
        Manage org-wide shared credentials and per-user service activation.
      </p>

      <div className="space-y-2">
        {services?.services?.map((s: OrgService) => (
          <div key={s.service_id} className="bg-surface-1 rounded-lg border border-border-default p-3 flex items-center justify-between">
            <div>
              <span className="text-sm font-medium text-text-primary">{s.name}</span>
              <span className="ml-2 text-xs text-text-secondary font-mono">{s.service_id}</span>
            </div>
            <div className="flex items-center gap-2">
              <span className={`text-xs px-1.5 py-0.5 rounded ${
                s.status === 'active' ? 'bg-green-100 text-green-700 dark:bg-green-900/30 dark:text-green-400' : 'bg-surface-0 text-text-secondary'
              }`}>
                {s.status}
              </span>
              <span className="text-xs text-text-secondary">{s.credential_type}</span>
            </div>
          </div>
        ))}
        {(!services?.services || services.services.length === 0) && (
          <p className="text-sm text-text-secondary">No services configured for this organization.</p>
        )}
      </div>
    </div>
  )
}
