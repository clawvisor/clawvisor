import { useQuery } from '@tanstack/react-query'
import { useAuth } from '../hooks/useAuth'
import { api } from '../api/client'

export default function OrgMCPServers() {
  const { currentOrg } = useAuth()
  const orgId = currentOrg?.id ?? ''

  const { data: servers } = useQuery({
    queryKey: ['org-mcp-servers', orgId],
    queryFn: () => api.orgs.mcpServers(orgId),
    enabled: !!orgId,
  })

  if (!currentOrg) {
    return <p className="text-sm text-text-secondary">Select an organization to manage MCP servers.</p>
  }

  return (
    <div className="space-y-4">
      <h2 className="text-lg font-semibold text-text-primary">
        MCP Servers &mdash; {currentOrg.name}
      </h2>
      <p className="text-sm text-text-secondary">
        Register external MCP servers for your organization. Tool calls are proxied through the gateway
        with credential injection and audit logging.
      </p>

      <div className="space-y-2">
        {servers?.map((s) => (
          <div key={s.id} className="bg-surface-1 rounded-lg border border-border-default p-3">
            <div className="flex items-center justify-between">
              <span className="text-sm font-medium text-text-primary">{s.name}</span>
              <span className="text-xs text-text-secondary">auth: {s.auth_type}</span>
            </div>
            <div className="mt-1 text-xs text-text-secondary font-mono truncate">{s.url}</div>
            {s.description && (
              <div className="mt-1 text-xs text-text-secondary">{s.description}</div>
            )}
          </div>
        ))}
        {(!servers || servers.length === 0) && (
          <p className="text-sm text-text-secondary">No MCP servers registered.</p>
        )}
      </div>
    </div>
  )
}
