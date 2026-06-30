import { useState } from 'react'
import { useQuery, useMutation } from '@tanstack/react-query'
import { api } from '../api/client'
import { useAuth } from '../hooks/useAuth'

export default function OrgSettings() {
  const { currentOrg, setCurrentOrg } = useAuth()
  const [editName, setEditName] = useState('')

  // Org creation is intentionally not exposed in the user dashboard;
  // it's a Clawvisor-admin operation handled in cmd/admin. End users
  // join orgs via invite links instead.
  const { data: memberships, refetch: refetchOrgs } = useQuery({
    queryKey: ['orgs'],
    queryFn: () => api.orgs.list(),
  })

  const updateOrg = useMutation({
    mutationFn: (id: string) => api.orgs.update(id, editName),
    onSuccess: (org) => {
      if (currentOrg?.id === org.id) setCurrentOrg(org)
      refetchOrgs()
    },
  })

  const deleteOrg = useMutation({
    mutationFn: (id: string) => api.orgs.delete(id),
    onSuccess: (_, id) => {
      if (currentOrg?.id === id) setCurrentOrg(null)
      refetchOrgs()
    },
  })

  return (
    <div className="p-4 sm:p-8 space-y-10">
      <h1 className="text-2xl font-bold text-text-primary">Organizations</h1>
      <div className="space-y-8">
        <div>

        {/* Org list */}
        <div className="space-y-3">
          {memberships?.map((m) => (
            <div key={m.org.id} className="bg-surface-1 rounded-lg border border-border-default p-4 flex items-center justify-between">
              <div>
                <div className="font-medium text-text-primary">{m.org.name}</div>
                <div className="text-xs text-text-secondary mt-0.5">
                  {m.org.slug} &middot; {m.role}
                </div>
              </div>
              <div className="flex items-center gap-2">
                {m.role === 'owner' && (
                  <button
                    onClick={() => {
                      const newName = prompt('New name:', m.org.name)
                      if (newName && newName !== m.org.name) {
                        setEditName(newName)
                        updateOrg.mutate(m.org.id)
                      }
                    }}
                    className="px-3 py-1.5 text-xs rounded-md border border-border-default hover:bg-surface-0 text-text-primary"
                  >
                    Rename
                  </button>
                )}
                {m.role === 'owner' && (
                  <button
                    onClick={() => {
                      if (confirm(`Delete "${m.org.name}"? This cannot be undone.`)) {
                        deleteOrg.mutate(m.org.id)
                      }
                    }}
                    className="px-3 py-1.5 text-xs rounded-md border border-danger/40 text-danger hover:bg-danger/10"
                  >
                    Delete
                  </button>
                )}
              </div>
            </div>
          ))}
          {(!memberships || memberships.length === 0) && (
            <p className="text-sm text-text-secondary">
              You're not a member of any organization yet. Ask your administrator to invite you.
            </p>
          )}
        </div>
      </div>
      </div>
    </div>
  )
}
