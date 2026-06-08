import TaskCard from '../TaskCard'
import ApprovalAttentionCard from './ApprovalAttentionCard'
import RuntimeApprovalAttentionCard from './RuntimeApprovalAttentionCard'
import ConnectionAttentionCard from './ConnectionAttentionCard'
import type { AttentionItem } from './types'

export default function AttentionInbox({
  items,
  agentMap,
}: {
  items: AttentionItem[]
  agentMap: Map<string, string>
}) {
  if (items.length === 0) {
    return (
      <div className="dev-empty--success">
        <span className="dev-badge--success">ok</span>
        <span className="font-mono">queue clear — no pending items</span>
      </div>
    )
  }

  return (
    <div className="space-y-3">
      {items.map(item => {
        if (item.kind === 'runtime_approval') {
          return <RuntimeApprovalAttentionCard key={item.approval.id} approval={item.approval} />
        }
        if (item.item.type === 'approval') {
          return <ApprovalAttentionCard key={item.item.id} item={item.item} />
        }
        if (item.item.type === 'connection' && item.item.connection) {
          return <ConnectionAttentionCard key={item.item.id} connection={item.item.connection} />
        }
        if (item.item.task) {
          return (
            <TaskCard
              key={item.item.id}
              task={item.item.task}
              agentName={agentMap.get(item.item.task.agent_id) ?? item.item.task.agent_id.slice(0, 8)}
            />
          )
        }
        return null
      })}
    </div>
  )
}
