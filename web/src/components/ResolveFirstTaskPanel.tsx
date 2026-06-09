import { useQuery } from '@tanstack/react-query'
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
    <ClawvisorInActionWalkthrough example={data?.walkthrough} />
  )
}
