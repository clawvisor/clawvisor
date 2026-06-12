import type { Task } from '../../api/client'
import TaskCard from '../TaskCard'

// Thin wrapper so the inbox can swap card families without breaking callers.
// The full task UX (scopes, audit trail, cost) lives in TaskCard until the
// PRD Phase-3 decomposition splits it further.
export default function TaskAttentionCard({ task, agentName }: { task: Task; agentName: string }) {
  return <TaskCard task={task} agentName={agentName} />
}
