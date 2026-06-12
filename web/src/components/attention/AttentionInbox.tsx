import { useMemo } from 'react'
import { Link } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { api, type Agent } from '../../api/client'
import type { AttentionItem } from './types'
import ApprovalAttentionCard from './ApprovalAttentionCard'
import ConnectionAttentionCard from './ConnectionAttentionCard'
import RuntimeApprovalAttentionCard from './RuntimeApprovalAttentionCard'
import TaskAttentionCard from './TaskAttentionCard'

interface Props {
  items: AttentionItem[]
  isLoading: boolean
}

export default function AttentionInbox({ items, isLoading }: Props) {
  const { data: agentsData } = useQuery({
    queryKey: ['agents'],
    queryFn: () => api.agents.list(),
  })

  const agentMap = useMemo(() => {
    const m = new Map<string, string>()
    for (const a of (agentsData ?? []) as Agent[]) m.set(a.id, a.name)
    return m
  }, [agentsData])

  if (isLoading && items.length === 0) {
    return (
      <div className="rounded-md border border-border-subtle bg-surface-1 px-5 py-8 text-center text-sm text-text-tertiary">
        Loading inbox…
      </div>
    )
  }

  if (items.length === 0) {
    return (
      <div className="rounded-md border border-success/30 bg-success/10 px-5 py-4 flex items-center gap-3">
        <svg className="w-5 h-5 text-success shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
          <path d="M22 11.08V12a10 10 0 1 1-5.93-9.14" />
          <polyline points="22 4 12 14.01 9 11.01" />
        </svg>
        <span className="text-success font-medium flex-1">All clear — nothing needs your attention</span>
        <Link to="/dashboard/activity" className="text-sm text-success hover:underline">
          View activity
        </Link>
      </div>
    )
  }

  return (
    <div className="space-y-3">
      {items.map(item => {
        if (item.kind === 'runtime_approval') {
          return <RuntimeApprovalAttentionCard key={item.id} approval={item.approval} />
        }
        const q = item.item
        if (q.type === 'approval') {
          return <ApprovalAttentionCard key={item.id} item={q} />
        }
        if (q.type === 'connection' && q.connection) {
          return <ConnectionAttentionCard key={item.id} connection={q.connection} />
        }
        if (q.task) {
          return (
            <TaskAttentionCard
              key={item.id}
              task={q.task}
              agentName={agentMap.get(q.task.agent_id) ?? q.task.agent_id.slice(0, 8)}
            />
          )
        }
        return null
      })}
    </div>
  )
}
