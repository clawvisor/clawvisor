import type { QueryClient } from '@tanstack/react-query'

/** Invalidate all caches that feed the attention inbox and sidebar badge. */
export function invalidateAttention(qc: QueryClient) {
  qc.invalidateQueries({ queryKey: ['overview'] })
  qc.invalidateQueries({ queryKey: ['inbox'] })
  qc.invalidateQueries({ queryKey: ['queue'] })
  qc.invalidateQueries({ queryKey: ['tasks'] })
  qc.invalidateQueries({ queryKey: ['runtime-approvals'] })
}
