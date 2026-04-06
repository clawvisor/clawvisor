import { useQuery, useMutation } from '@tanstack/react-query'
import { api, type Agent } from '../api/client'
import { useAuth } from '../hooks/useAuth'

export default function OrgAgents() {
  const { currentOrg } = useAuth()
  const orgId = currentOrg?.id ?? ''

  const { data: agents, refetch } = useQuery({
    queryKey: ['org-agents', orgId],
    queryFn: () => api.orgs.agents(orgId),
    enabled: !!orgId,
  })

  const deleteAgent = useMutation({
    mutationFn: (agentId: string) =>
      api.orgs.agents(orgId).then(() => {
        // Delete is handled by the org admin endpoint
        return fetch(`/api/orgs/${orgId}/agents/${agentId}`, { method: 'DELETE' })
      }),
    onSuccess: () => refetch(),
  })

  if (!currentOrg) {
    return <p className="text-sm text-text-secondary">Select an organization to view agents.</p>
  }

  return (
    <div className="space-y-4">
      <h2 className="text-lg font-semibold text-text-primary">
        Org Agents &mdash; {currentOrg.name}
      </h2>

      <div className="space-y-2">
        {agents?.map((a: Agent) => (
          <div key={a.id} className="bg-surface-1 rounded-lg border border-border-default p-3 flex items-center justify-between">
            <div>
              <span className="text-sm font-medium text-text-primary">{a.name}</span>
              <span className="ml-2 text-xs text-text-secondary">
                Created {new Date(a.created_at).toLocaleDateString()}
              </span>
            </div>
            <button
              onClick={() => {
                if (confirm(`Delete agent "${a.name}"?`)) {
                  deleteAgent.mutate(a.id)
                }
              }}
              className="text-xs px-2 py-1 rounded border border-red-300 text-red-600 hover:bg-red-50 dark:border-red-800 dark:text-red-400 dark:hover:bg-red-900/20"
            >
              Delete
            </button>
          </div>
        ))}
        {(!agents || agents.length === 0) && (
          <p className="text-sm text-text-secondary">No agents in this organization.</p>
        )}
      </div>
    </div>
  )
}
