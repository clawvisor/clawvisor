import { useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api } from '../api/client'
import type { ConnectionRequest } from '../api/client'
import { formatDistanceToNow } from 'date-fns'
import CountdownTimer from '../components/CountdownTimer'

export default function Pairing() {
  const { data: connections, isLoading } = useQuery({
    queryKey: ['connections'],
    queryFn: () => api.connections.list(),
    refetchInterval: 10_000,
  })

  const pending = connections ?? []

  return (
    <div className="p-8 space-y-8">
      <div>
        <h1 className="text-2xl font-bold text-text-primary">Agent Pairing</h1>
        <p className="text-sm text-text-tertiary mt-1">
          When an agent requests to connect, it appears here for you to approve or deny.
        </p>
      </div>

      {/* Pending connection requests */}
      <section>
        <h2 className="text-lg font-semibold text-text-primary mb-3">
          Pending Requests
          {pending.length > 0 && (
            <span className="ml-2 bg-warning text-surface-0 text-xs font-bold rounded px-2.5 py-0.5 font-mono">
              {pending.length}
            </span>
          )}
        </h2>

        {isLoading && <div className="text-sm text-text-tertiary">Loading...</div>}

        {!isLoading && pending.length === 0 && (
          <div className="rounded-md border border-border-default bg-surface-1 px-5 py-8 text-center space-y-2">
            <p className="text-sm text-text-tertiary">No pending connection requests.</p>
            <p className="text-xs text-text-tertiary">
              Agents can request access by posting to <code className="bg-surface-2 px-1 rounded">/api/agents/connect</code>
            </p>
          </div>
        )}

        <div className="space-y-3">
          {pending.map(cr => (
            <ConnectionCard key={cr.id} request={cr} />
          ))}
        </div>
      </section>

      {/* Instructions for agents */}
      <section className="bg-surface-1 border border-border-default rounded-md p-5 space-y-3">
        <h2 className="text-sm font-semibold text-text-secondary">How agents connect</h2>
        <div className="text-xs text-text-tertiary space-y-2">
          <p>An agent initiates pairing by sending a connection request to this daemon:</p>
          <pre className="bg-surface-0 border border-border-subtle rounded p-3 overflow-x-auto font-mono text-text-secondary">
{`curl -X POST "$CLAWVISOR_URL/api/agents/connect" \\
  -H "Content-Type: application/json" \\
  -d '{"name": "my-agent", "description": "What this agent does"}'`}
          </pre>
          <p>The request appears above. Once you approve it, the agent receives a bearer token by polling:</p>
          <pre className="bg-surface-0 border border-border-subtle rounded p-3 overflow-x-auto font-mono text-text-secondary">
{`curl "$CLAWVISOR_URL/api/agents/connect/<connection_id>/status"`}
          </pre>
          <p>Connection requests expire after 5 minutes.</p>
        </div>
      </section>
    </div>
  )
}

function ConnectionCard({ request: cr }: { request: ConnectionRequest }) {
  const qc = useQueryClient()
  const [result, setResult] = useState<string | null>(null)

  const approveMut = useMutation({
    mutationFn: () => api.connections.approve(cr.id),
    onSuccess: () => {
      setResult('Approved')
      qc.invalidateQueries({ queryKey: ['connections'] })
      qc.invalidateQueries({ queryKey: ['agents'] })
    },
    onError: (err: Error) => setResult(`Failed: ${err.message}`),
  })

  const denyMut = useMutation({
    mutationFn: () => api.connections.deny(cr.id),
    onSuccess: () => {
      setResult('Denied')
      qc.invalidateQueries({ queryKey: ['connections'] })
    },
    onError: (err: Error) => setResult(`Failed: ${err.message}`),
  })

  const isPending = approveMut.isPending || denyMut.isPending

  if (result) {
    return (
      <div className="border border-border-default rounded-md bg-surface-1 px-5 py-4">
        <div className="flex items-center justify-between">
          <span className="font-medium text-text-primary">{cr.name}</span>
          <span className={`text-xs font-medium px-2 py-0.5 rounded ${
            result === 'Approved' ? 'bg-success/10 text-success' :
            result === 'Denied' ? 'bg-danger/10 text-danger' :
            'bg-surface-2 text-text-tertiary'
          }`}>
            {result}
          </span>
        </div>
      </div>
    )
  }

  return (
    <div className="bg-surface-1 border border-border-default rounded-md border-l-[3px] border-l-brand overflow-hidden">
      <div className="px-5 pt-5 pb-4">
        <div className="flex items-center justify-between">
          <span className="font-mono text-lg font-semibold text-text-primary">{cr.name}</span>
          <CountdownTimer expiresAt={cr.expires_at} />
        </div>
        {cr.description && (
          <p className="text-sm text-text-secondary mt-1.5">{cr.description}</p>
        )}
        <div className="flex items-center gap-3 mt-2 text-xs text-text-tertiary">
          <span>IP: <code className="font-mono">{cr.ip_address}</code></span>
          <span>Requested {formatDistanceToNow(new Date(cr.created_at), { addSuffix: true })}</span>
        </div>
      </div>

      <div className="px-4 py-3 border-t border-border-subtle flex items-center justify-end gap-2">
        <button
          onClick={() => denyMut.mutate()}
          disabled={isPending}
          className="rounded px-4 py-1.5 text-sm font-medium bg-danger/10 text-danger border border-danger/20 hover:bg-danger/20 disabled:opacity-50"
        >
          Deny
        </button>
        <button
          onClick={() => approveMut.mutate()}
          disabled={isPending}
          className="bg-brand text-surface-0 font-medium rounded px-5 py-1.5 text-sm hover:bg-brand-strong disabled:opacity-50"
        >
          {approveMut.isPending ? 'Approving...' : 'Approve'}
        </button>
      </div>
    </div>
  )
}
