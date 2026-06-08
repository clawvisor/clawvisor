import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { api, type ConnectionRequest } from '../../api/client'
import CountdownTimer from '../CountdownTimer'
import { invalidateAttention } from './invalidate'

export default function ConnectionAttentionCard({ connection: cr }: { connection: ConnectionRequest }) {
  const qc = useQueryClient()
  const [result, setResult] = useState<string | null>(null)

  const approveMut = useMutation({
    mutationFn: () => api.connections.approve(cr.id),
    onSuccess: () => {
      setResult('Approved')
      invalidateAttention(qc)
      qc.invalidateQueries({ queryKey: ['agents'] })
      qc.invalidateQueries({ queryKey: ['welcome'] })
    },
    onError: (err: Error) => setResult(`Failed: ${err.message}`),
  })

  const denyMut = useMutation({
    mutationFn: () => api.connections.deny(cr.id),
    onSuccess: () => {
      setResult('Denied')
      invalidateAttention(qc)
    },
    onError: (err: Error) => setResult(`Failed: ${err.message}`),
  })

  const isPending = approveMut.isPending || denyMut.isPending

  if (result) {
    return (
      <div className="dev-panel p-5">
        <div className="dev-inset p-3 font-mono text-sm text-text-tertiary">{result}</div>
      </div>
    )
  }

  return (
    <div className="dev-card-accent-brand">
      <div className="px-5 pt-5 pb-4">
        <span className="font-mono text-lg font-semibold text-text-primary">{cr.name}</span>
        {cr.description && <p className="text-sm text-text-secondary mt-1.5">{cr.description}</p>}
        <div className="flex items-center gap-2 mt-2">
          <span className="dev-badge--brand">
            <span className="w-1.5 h-1.5 rounded-full bg-brand" />
            agent connection
          </span>
          <span className="text-xs text-text-tertiary">IP: <code className="font-mono">{cr.ip_address}</code></span>
          {cr.expires_at && <CountdownTimer expiresAt={cr.expires_at} />}
        </div>
      </div>

      <div className="px-4 py-3 border-t border-border-subtle flex items-center justify-end gap-2">
        <button
          onClick={() => denyMut.mutate()}
          disabled={isPending}
          className="dev-btn-danger"
        >
          deny
        </button>
        <button
          onClick={() => approveMut.mutate()}
          disabled={isPending}
          className="dev-btn-primary"
        >
          {approveMut.isPending ? 'Approving...' : 'Approve'}
        </button>
      </div>
    </div>
  )
}
