import { Navigate, useLocation } from 'react-router-dom'

/** Legacy route — inbox merged into Activity. */
export default function Inbox() {
  const location = useLocation()
  return <Navigate to={`/dashboard/activity${location.search}`} replace />
}
