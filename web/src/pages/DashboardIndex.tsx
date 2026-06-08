import { Navigate, useLocation } from 'react-router-dom'
import { useAttentionItems } from '../hooks/useAttentionItems'

/**
 * Dashboard index route: deep-link routing, Activity when work is waiting,
 * otherwise Home (onboarding + dashboard unified).
 */
export default function DashboardIndex() {
  const location = useLocation()
  const { attentionCount, isLoading: attentionLoading } = useAttentionItems()

  const params = new URLSearchParams(location.search)
  const action = params.get('action')
  const requestId = params.get('request_id')
  const taskId = params.get('task_id')

  if (action && requestId) {
    return <Navigate to={`/dashboard/activity${location.search}`} replace />
  }
  if (action && taskId) {
    return <Navigate to={`/dashboard/tasks${location.search}`} replace />
  }

  if (attentionLoading) {
    return <div className="p-4 sm:p-8 ds-page-loading">loading…</div>
  }

  if (attentionCount > 0) {
    return <Navigate to="/dashboard/activity" replace />
  }

  return <Navigate to="/dashboard/home" replace />
}
