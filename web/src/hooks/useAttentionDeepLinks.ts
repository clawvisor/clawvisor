import { useEffect, useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useSearchParams } from 'react-router-dom'
import { api, APIError } from '../api/client'
import { useAuth } from './useAuth'
import { invalidateAttention } from '../components/attention/invalidate'

// Drives the `?action=approve|deny|expand_approve|expand_deny` query-param
// flow shared by the Inbox page and historically by Overview / Tasks.
//
// Two shapes are supported (Telegram and CLI emit both):
//   - request_id (+ optional task_id) → resolved via approvals API
//   - task_id only (any action incl. expand_*) → resolved via tasks API
//
// Task-level actions are personal-context only because the task API is
// scoped to the signed-in user; org context owners use a different surface.
// On success or failure we strip the params so a reload doesn't re-fire.
export function useAttentionDeepLinks() {
  const qc = useQueryClient()
  const { currentOrg } = useAuth()
  const [searchParams, setSearchParams] = useSearchParams()
  const [result, setResult] = useState<string | null>(null)

  const approveRequest = useMutation({
    mutationFn: ({ requestId, taskId }: { requestId: string; taskId?: string }) =>
      api.approvals.approve(requestId, undefined, taskId),
    onSuccess: (_data, vars) => {
      setResult(`Request ${vars.requestId.slice(0, 8)}... approved.`)
      invalidateAttention(qc)
    },
    onError: (err: Error) => setResult(inlineChatOr(err, 'Approve failed')),
  })
  const denyRequest = useMutation({
    mutationFn: ({ requestId, taskId }: { requestId: string; taskId?: string }) =>
      api.approvals.deny(requestId, taskId),
    onSuccess: (_data, vars) => {
      setResult(`Request ${vars.requestId.slice(0, 8)}... denied.`)
      invalidateAttention(qc)
    },
    onError: (err: Error) => setResult(inlineChatOr(err, 'Deny failed')),
  })
  const approveTask = useMutation({
    mutationFn: (taskId: string) => api.tasks.approve(taskId),
    onSuccess: () => { setResult('Task approved.'); invalidateAttention(qc) },
    onError: (err: Error) => setResult(inlineChatOr(err, 'Approve failed')),
  })
  const denyTask = useMutation({
    mutationFn: (taskId: string) => api.tasks.deny(taskId),
    onSuccess: () => { setResult('Task denied.'); invalidateAttention(qc) },
    onError: (err: Error) => setResult(inlineChatOr(err, 'Deny failed')),
  })
  const expandApproveTask = useMutation({
    mutationFn: (taskId: string) => api.tasks.expandApprove(taskId),
    onSuccess: () => { setResult('Scope expansion approved.'); invalidateAttention(qc) },
    onError: (err: Error) => setResult(`Expansion approve failed: ${err.message}`),
  })
  const expandDenyTask = useMutation({
    mutationFn: (taskId: string) => api.tasks.expandDeny(taskId),
    onSuccess: () => { setResult('Scope expansion denied.'); invalidateAttention(qc) },
    onError: (err: Error) => setResult(`Expansion deny failed: ${err.message}`),
  })

  useEffect(() => {
    const action = searchParams.get('action')
    const requestId = searchParams.get('request_id')
    const taskId = searchParams.get('task_id') ?? undefined
    if (!action || (!requestId && !taskId)) return

    // Resolve the action first; only clear the URL once a mutation has
    // actually been dispatched. Otherwise a deep link that we couldn't act
    // on (org context blocks task actions, unknown action name) would lose
    // its params silently with no recovery path.
    let dispatched = false

    if (requestId) {
      if (currentOrg) {
        setResult('Switch to your personal context to act on this request.')
      } else if (action === 'approve') {
        approveRequest.mutate({ requestId, taskId })
        dispatched = true
      } else if (action === 'deny') {
        denyRequest.mutate({ requestId, taskId })
        dispatched = true
      }
    } else if (taskId) {
      if (currentOrg) {
        setResult('Switch to your personal context to act on this task.')
      } else {
        switch (action) {
          case 'approve': approveTask.mutate(taskId); dispatched = true; break
          case 'deny': denyTask.mutate(taskId); dispatched = true; break
          case 'expand_approve': expandApproveTask.mutate(taskId); dispatched = true; break
          case 'expand_deny': expandDenyTask.mutate(taskId); dispatched = true; break
        }
      }
    }

    if (dispatched) {
      setSearchParams({}, { replace: true })
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  return { result, clear: () => setResult(null) }
}

function inlineChatOr(err: Error, prefix: string): string {
  if (err instanceof APIError && err.code === 'INLINE_CHAT_BOUND') {
    return 'Reply approve/deny in the agent chat'
  }
  return `${prefix}: ${err.message}`
}
