import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { api, type ConnectionRequest } from '../../api/client'
import CountdownTimer from '../CountdownTimer'
import { invalidateAttention } from './invalidate'

export default function ConnectionAttentionCard({ connection: cr }: { connection: ConnectionRequest }) {
  const qc = useQueryClient()
  const [resolved, setResolved] = useState<string | null>(null)
  // Errors are shown inline alongside the existing action buttons so a failed
  // approve/deny stays retryable. Replacing the card body with a permanent
  // error message (the prior behavior) made transient failures unrecoverable.
  const [error, setError] = useState<string | null>(null)

  const approveMut = useMutation({
    mutationFn: () => api.connections.approve(cr.id),
    onSuccess: () => {
      setResolved('Approved')
      invalidateAttention(qc)
      qc.invalidateQueries({ queryKey: ['agents'] })
      qc.invalidateQueries({ queryKey: ['welcome'] })
    },
    onError: (err: Error) => setError(err.message),
  })

  const denyMut = useMutation({
    mutationFn: () => api.connections.deny(cr.id),
    onSuccess: () => {
      setResolved('Denied')
      invalidateAttention(qc)
    },
    onError: (err: Error) => setError(err.message),
  })

  const isPending = approveMut.isPending || denyMut.isPending

  if (resolved) {
    return (
      <div className="border border-border-default rounded-md bg-surface-1 p-5">
        <div className="p-3 bg-surface-2 rounded text-sm text-text-tertiary">{resolved}</div>
      </div>
    )
  }

  return (
    <div className="bg-surface-1 border border-border-default rounded-md border-l-[3px] border-l-brand overflow-hidden">
      <div className="px-5 pt-5 pb-4">
        <span className="font-mono text-lg font-semibold text-text-primary">{cr.name}</span>
        {cr.description && (
          <p className="text-sm text-text-secondary mt-1.5">{cr.description}</p>
        )}
        <div className="flex items-center gap-2 mt-2">
          <span className="inline-flex items-center gap-1.5 text-xs font-mono font-medium px-2 py-0.5 rounded bg-brand/15 text-brand">
            <span className="w-1.5 h-1.5 rounded-full bg-brand" />
            agent connection
          </span>
          <span className="text-xs text-text-tertiary">IP: <code className="font-mono">{cr.ip_address}</code></span>
          {cr.expires_at && <CountdownTimer expiresAt={cr.expires_at} />}
        </div>
      </div>

      {error && (
        <div className="px-5 pb-3">
          <div className="rounded border border-danger/30 bg-danger/10 px-3 py-2 text-xs text-danger flex items-center justify-between gap-3">
            <span>{error}</span>
            <button onClick={() => setError(null)} className="text-xs underline hover:opacity-80">Dismiss</button>
          </div>
        </div>
      )}

      <div className="px-4 py-3 border-t border-border-subtle flex items-center justify-end gap-2">
        <button
          onClick={() => denyMut.mutate()}
          disabled={isPending}
          className="rounded px-4 py-1.5 text-sm font-medium bg-danger/10 text-danger border border-danger/20 hover:bg-danger/20 disabled:opacity-50 min-h-[44px] sm:min-h-0"
        >
          Deny
        </button>
        <button
          onClick={() => approveMut.mutate()}
          disabled={isPending}
          className="bg-brand text-surface-0 font-medium rounded px-5 py-1.5 text-sm hover:bg-brand-strong disabled:opacity-50 min-h-[44px] sm:min-h-0"
        >
          {approveMut.isPending ? 'Approving...' : 'Approve'}
        </button>
      </div>
    </div>
  )
}
