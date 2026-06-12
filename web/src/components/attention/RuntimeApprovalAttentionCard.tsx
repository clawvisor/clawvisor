import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { api, type ApprovalRecord } from '../../api/client'
import CountdownTimer from '../CountdownTimer'
import {
  runtimeSummary,
  runtimePayload,
  runtimeApprovalPrimary,
  runtimeApprovalReason,
  runtimeApprovalDetail,
} from './runtimeHelpers'
import { invalidateAttention } from './invalidate'

export default function RuntimeApprovalAttentionCard({ approval }: { approval: ApprovalRecord }) {
  const qc = useQueryClient()
  const [resolved, setResolved] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)

  const summary = runtimeSummary(approval)
  const payload = runtimePayload(approval)
  const primary = runtimeApprovalPrimary(payload, summary, approval.kind)
  const reason = runtimeApprovalReason(payload, summary)
  const detail = runtimeApprovalDetail(payload)
  const allowLabel = approval.resolution_transport === 'release_held_tool_use' ? 'Release Tool Call' : 'Allow Once'

  const resolveMut = useMutation({
    mutationFn: (resolution: 'allow_once' | 'deny') =>
      api.runtime.resolveApproval(approval.id, resolution),
    onSuccess: (_res, resolution) => {
      setResolved(resolution === 'deny' ? 'Denied' : 'Allowed once')
      invalidateAttention(qc)
    },
    onError: (err: Error) => setError(err.message),
  })

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
        <span className="font-mono text-lg font-semibold text-text-primary break-all">{primary}</span>
        {reason && <p className="text-sm text-text-secondary mt-1.5">{reason}</p>}
        <div className="flex flex-wrap items-center gap-2 mt-2">
          <span className="inline-flex items-center gap-1.5 text-xs font-mono font-medium px-2 py-0.5 rounded bg-brand/15 text-brand">
            <span className="w-1.5 h-1.5 rounded-full bg-brand" />
            {approval.resolution_transport === 'release_held_tool_use' ? 'inline runtime approval' : 'runtime retry approval'}
          </span>
          {approval.session_id && (
            <span className="text-xs text-text-tertiary">session <code className="font-mono">{approval.session_id.slice(0, 8)}</code></span>
          )}
          {approval.expires_at && <CountdownTimer expiresAt={approval.expires_at} />}
        </div>
        {payload && (
          <div className="mt-3 bg-surface-0 border border-border-subtle rounded p-3 space-y-1">
            {detail && <div className="text-[11px] font-mono text-text-tertiary break-all">{detail}</div>}
            {payload.host && <div className="text-[11px] font-mono text-text-tertiary">host: {payload.host}</div>}
            {payload.path && <div className="text-[11px] font-mono text-text-tertiary">path: {payload.path}</div>}
            {payload.query && Object.keys(payload.query).length > 0 && (
              <div className="text-[11px] font-mono text-text-tertiary break-all">query: {JSON.stringify(payload.query)}</div>
            )}
          </div>
        )}
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
          onClick={() => resolveMut.mutate('deny')}
          disabled={resolveMut.isPending}
          className="rounded px-4 py-1.5 text-sm font-medium bg-danger/10 text-danger border border-danger/20 hover:bg-danger/20 disabled:opacity-50 min-h-[44px] sm:min-h-0"
        >
          Deny
        </button>
        <button
          onClick={() => resolveMut.mutate('allow_once')}
          disabled={resolveMut.isPending}
          className="bg-brand text-surface-0 font-medium rounded px-5 py-1.5 text-sm hover:bg-brand-strong disabled:opacity-50 min-h-[44px] sm:min-h-0"
        >
          {resolveMut.isPending ? 'Updating...' : allowLabel}
        </button>
      </div>
    </div>
  )
}
