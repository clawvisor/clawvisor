import { useEffect, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { api, type OrgMembership } from '../api/client'
import { useAuth } from '../hooks/useAuth'

export default function OrgSelector() {
  const { features, currentOrg, setCurrentOrg } = useAuth()
  const queryClient = useQueryClient()
  const [open, setOpen] = useState(false)

  const { data: memberships } = useQuery({
    queryKey: ['orgs'],
    queryFn: () => api.orgs.list(),
    enabled: features?.teams ?? false,
  })

  // Single-org users: auto-set the only available org and skip the
  // selector entirely. There's no meaningful choice to expose.
  const singleOrg = memberships?.length === 1 ? memberships[0] : null
  useEffect(() => {
    if (singleOrg && currentOrg?.id !== singleOrg.org.id) {
      setCurrentOrg(singleOrg.org)
      queryClient.invalidateQueries()
    }
  }, [singleOrg, currentOrg?.id, setCurrentOrg, queryClient])

  // Multi-org users: if currentOrg is unset or no longer points at one
  // of their memberships (stale localStorage, dropped membership), snap
  // to the first available org. Org-affiliated users never see a
  // "Personal" workspace — everything is scoped to an org.
  useEffect(() => {
    if (!memberships || memberships.length < 2) return
    const current = memberships.find((m) => m.org.id === currentOrg?.id)
    if (!current) {
      setCurrentOrg(memberships[0].org)
      queryClient.invalidateQueries()
    }
  }, [memberships, currentOrg?.id, setCurrentOrg, queryClient])

  if (!features?.teams || !memberships?.length) return null
  if (singleOrg) return null

  const handleSelect = (m: OrgMembership) => {
    setCurrentOrg(m.org)
    setOpen(false)
    // Invalidate all queries so they refetch with new X-Org-Id header
    queryClient.invalidateQueries()
  }

  return (
    <div className="relative">
      <button
        onClick={() => setOpen(!open)}
        className="w-full flex items-center gap-2 px-3 py-2 text-sm rounded-md border border-border-default bg-surface-0 hover:bg-surface-1 text-text-primary transition-colors"
      >
        <svg className="w-4 h-4 text-text-secondary shrink-0" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24">
          <path d="M17 21v-2a4 4 0 00-4-4H5a4 4 0 00-4 4v2" /><circle cx="9" cy="7" r="4" /><path d="M23 21v-2a4 4 0 00-3-3.87" /><path d="M16 3.13a4 4 0 010 7.75" />
        </svg>
        <span className="truncate">{currentOrg?.name ?? '…'}</span>
        <svg className="w-3 h-3 ml-auto text-text-secondary shrink-0" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24">
          <path d="M6 9l6 6 6-6" />
        </svg>
      </button>

      {open && (
        <>
          <div className="fixed inset-0 z-10" onClick={() => setOpen(false)} />
          <div className="absolute left-0 top-full mt-1 w-full bg-surface-0 border border-border-default rounded-md shadow-lg z-20 py-1">
            {memberships.map((m) => (
              <button
                key={m.org.id}
                onClick={() => handleSelect(m)}
                className={`w-full text-left px-3 py-2 text-sm hover:bg-surface-1 ${currentOrg?.id === m.org.id ? 'text-brand font-medium' : 'text-text-primary'}`}
              >
                <span className="truncate">{m.org.name}</span>
                <span className="ml-1 text-xs text-text-secondary">({m.role})</span>
              </button>
            ))}
          </div>
        </>
      )}
    </div>
  )
}
