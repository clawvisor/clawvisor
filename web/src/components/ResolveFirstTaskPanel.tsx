import { useQuery } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { api } from '../api/client'
import ClawvisorInActionWalkthrough from './ClawvisorInActionWalkthrough'

export default function ResolveFirstTaskPanel() {
  const { data } = useQuery({
    queryKey: ['welcome'],
    queryFn: () => api.welcome.suggestions(),
    staleTime: 5 * 60_000,
    refetchOnWindowFocus: false,
  })

  return (
    <div className="space-y-6">
      <ClawvisorInActionWalkthrough example={data?.walkthrough} />

      <Link to="/dashboard/activity" className="dev-btn-ghost gap-1.5">
        <ActivityIcon className="w-3.5 h-3.5" />
        open activity
      </Link>
    </div>
  )
}

function ActivityIcon({ className }: { className?: string }) {
  return (
    <svg className={className} fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24">
      <circle cx="12" cy="12" r="10" />
      <path d="M12 6v6l4 2" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  )
}
