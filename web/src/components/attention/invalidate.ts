import type { QueryClient } from '@tanstack/react-query'

// Shared invalidation for any action that resolves an attention item. The
// inbox derives its list from these query keys, so every approve/deny/resolve
// path must invalidate the same set or the item will linger until polling.
export function invalidateAttention(qc: QueryClient) {
  qc.invalidateQueries({ queryKey: ['overview'] })
  qc.invalidateQueries({ queryKey: ['queue'] })
  qc.invalidateQueries({ queryKey: ['runtime-approvals'] })
  qc.invalidateQueries({ queryKey: ['tasks'] })
}
