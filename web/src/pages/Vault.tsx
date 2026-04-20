import { useQuery } from '@tanstack/react-query'
import { api } from '../api/client'
import VaultCredentials from '../components/VaultCredentials'

// Vault — first-class IA for credentials the proxy injects on behalf of
// agents. Replaces the section that used to live on /agents. Pulled out
// because credentials are user-scoped (not bridge-scoped) and the
// /agents page was carrying too many concerns.
export default function Vault() {
  const { data: agents } = useQuery({
    queryKey: ['agents', 'personal'],
    queryFn: () => api.agents.list(),
  })

  return (
    <div className="p-4 sm:p-8 space-y-6">
      <header>
        <h1 className="text-2xl font-bold text-text-primary">Vault</h1>
        <p className="text-sm text-text-tertiary mt-1">
          API keys + secrets the Network Proxy injects into outbound requests
          on behalf of your agents. Agents never see the raw values; the proxy
          adds the right header per matching destination.
        </p>
      </header>
      <VaultCredentials agents={agents} />
    </div>
  )
}
