import { useEffect, useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useSearchParams } from 'react-router-dom'
import { api, APIError } from '../api/client'
import { invalidateAttention } from '../components/attention/invalidate'

type DeepLinkVars = { requestId: string; taskId?: string }

/**
 * Handles ?action=approve|deny&request_id=… deep links on the Activity page.
 */
export function useAttentionDeepLinks() {
  const qc = useQueryClient()
  const [searchParams, setSearchParams] = useSearchParams()
  const [deepLinkResult, setDeepLinkResult] = useState<string | null>(null)

  const deepApproveRequest = useMutation({
    mutationFn: ({ requestId, taskId }: DeepLinkVars) =>
      api.approvals.approve(requestId, undefined, taskId),
    onSuccess: (_data, vars) => {
      setDeepLinkResult(`Request ${vars.requestId.slice(0, 8)}... approved.`)
      invalidateAttention(qc)
    },
    onError: (err: Error) => {
      if (err instanceof APIError && err.code === 'INLINE_CHAT_BOUND') {
        setDeepLinkResult('Reply approve/deny in the agent chat')
      } else {
        setDeepLinkResult(`Approve failed: ${err.message}`)
      }
    },
  })

  const deepDenyRequest = useMutation({
    mutationFn: ({ requestId, taskId }: DeepLinkVars) => api.approvals.deny(requestId, taskId),
    onSuccess: (_data, vars) => {
      setDeepLinkResult(`Request ${vars.requestId.slice(0, 8)}... denied.`)
      invalidateAttention(qc)
    },
    onError: (err: Error) => {
      if (err instanceof APIError && err.code === 'INLINE_CHAT_BOUND') {
        setDeepLinkResult('Reply approve/deny in the agent chat')
      } else {
        setDeepLinkResult(`Deny failed: ${err.message}`)
      }
    },
  })

  useEffect(() => {
    const action = searchParams.get('action')
    const requestId = searchParams.get('request_id')
    const taskId = searchParams.get('task_id') ?? undefined
    if (!action || !requestId) return

    setSearchParams({}, { replace: true })

    if (action === 'approve') deepApproveRequest.mutate({ requestId, taskId })
    else if (action === 'deny') deepDenyRequest.mutate({ requestId, taskId })
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  return { deepLinkResult, setDeepLinkResult }
}
