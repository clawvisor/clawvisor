import type { QueueItem, ApprovalRecord } from '../../api/client'

export type AttentionItem =
  | { kind: 'queue'; id: string; createdAt: string; item: QueueItem }
  | { kind: 'runtime_approval'; id: string; createdAt: string; approval: ApprovalRecord }
