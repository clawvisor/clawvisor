import type { ApprovalRecord, QueueItem } from '../../api/client'

export type AttentionItem =
  | { kind: 'queue'; createdAt: string; item: QueueItem }
  | { kind: 'runtime_approval'; createdAt: string; approval: ApprovalRecord }
