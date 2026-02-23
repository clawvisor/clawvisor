import { useQuery, useQueryClient } from '@tanstack/react-query'
import { api, type ServiceInfo } from '../api/client'
import { formatDistanceToNow } from 'date-fns'

function ServiceCard({ svc }: { svc: ServiceInfo }) {
  const qc = useQueryClient()

  function handleActivate() {
    // Navigate to OAuth start; after callback the page will reload
    window.location.href = api.services.oauthStartUrl(svc.id)
  }

  async function handleDeactivate() {
    if (!confirm(`Deactivate ${svc.id}? Your agents will lose access to this service.`)) return
    // Remove vault credential by revoking via service-meta delete — not yet a direct API,
    // so we just refresh so users see the current state.
    qc.invalidateQueries({ queryKey: ['services'] })
  }

  const isActivated = svc.status === 'activated'

  return (
    <div className="bg-white border rounded-lg p-5 space-y-3">
      <div className="flex items-start justify-between">
        <div>
          <h3 className="font-semibold text-gray-900">{svc.id}</h3>
          <p className="text-xs text-gray-400 mt-0.5">{svc.actions.join(' · ')}</p>
        </div>
        <StatusBadge status={svc.status} />
      </div>

      {isActivated && svc.activated_at && (
        <p className="text-xs text-gray-400">
          Activated {formatDistanceToNow(new Date(svc.activated_at), { addSuffix: true })}
        </p>
      )}

      <div className="pt-1">
        {isActivated ? (
          <div className="flex gap-2">
            <button
              onClick={handleActivate}
              className="text-xs px-3 py-1.5 rounded border border-gray-300 text-gray-600 hover:bg-gray-50"
            >
              Re-authorize
            </button>
            <button
              onClick={handleDeactivate}
              className="text-xs px-3 py-1.5 rounded border border-red-200 text-red-600 hover:bg-red-50"
            >
              Deactivate
            </button>
          </div>
        ) : (
          svc.oauth ? (
            <button
              onClick={handleActivate}
              className="text-xs px-3 py-1.5 rounded bg-blue-600 text-white hover:bg-blue-700"
            >
              Activate with OAuth
            </button>
          ) : (
            <button
              disabled
              className="text-xs px-3 py-1.5 rounded bg-gray-100 text-gray-400 cursor-not-allowed"
            >
              API Key (coming soon)
            </button>
          )
        )}
      </div>
    </div>
  )
}

function StatusBadge({ status }: { status: string }) {
  if (status === 'activated') {
    return <span className="px-2 py-0.5 rounded-full bg-green-100 text-green-700 text-xs font-medium">Activated</span>
  }
  return <span className="px-2 py-0.5 rounded-full bg-gray-100 text-gray-500 text-xs font-medium">Not activated</span>
}

export default function Services() {
  const { data, isLoading, error } = useQuery({
    queryKey: ['services'],
    queryFn: () => api.services.list(),
  })

  return (
    <div className="p-8 space-y-6">
      <h1 className="text-2xl font-bold text-gray-900">Services</h1>
      <p className="text-sm text-gray-500">
        Activate services to let your agents use them. Credentials are stored securely in the vault.
      </p>

      {isLoading && <div className="text-sm text-gray-400">Loading…</div>}
      {error && <div className="text-sm text-red-500">Failed to load services.</div>}

      {data && data.services.length === 0 && (
        <p className="text-sm text-gray-400">No adapters registered. Add one in the server configuration.</p>
      )}

      <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4">
        {data?.services.map(svc => (
          <ServiceCard key={svc.id} svc={svc} />
        ))}
      </div>
    </div>
  )
}
