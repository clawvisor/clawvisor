import { useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api } from '../api/client'
import { useAuth } from '../hooks/useAuth'

export default function OrgSettings() {
  const { currentOrg, setCurrentOrg } = useAuth()
  const queryClient = useQueryClient()
  const [name, setName] = useState('')
  const [slug, setSlug] = useState('')
  const [editName, setEditName] = useState('')

  const { data: memberships, refetch: refetchOrgs } = useQuery({
    queryKey: ['orgs'],
    queryFn: () => api.orgs.list(),
  })

  const createOrg = useMutation({
    mutationFn: () => api.orgs.create(name, slug),
    onSuccess: () => {
      setName('')
      setSlug('')
      refetchOrgs()
      queryClient.invalidateQueries({ queryKey: ['orgs'] })
    },
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
    <div className="space-y-8">
      <div>
        <h2 className="text-lg font-semibold text-text-primary mb-4">Organizations</h2>

        {/* Create new org */}
        <div className="bg-surface-1 rounded-lg border border-border-default p-4 mb-6">
          <h3 className="text-sm font-medium text-text-primary mb-3">Create Organization</h3>
          <div className="flex gap-3">
            <input
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="Organization name"
              className="flex-1 px-3 py-2 text-sm rounded-md border border-border-default bg-surface-0 text-text-primary"
            />
            <input
              value={slug}
              onChange={(e) => setSlug(e.target.value.toLowerCase().replace(/[^a-z0-9-]/g, ''))}
              placeholder="slug"
              className="w-40 px-3 py-2 text-sm rounded-md border border-border-default bg-surface-0 text-text-primary"
            />
            <button
              onClick={() => createOrg.mutate()}
              disabled={!name || !slug || createOrg.isPending}
              className="px-4 py-2 text-sm font-medium rounded-md bg-accent-primary text-white hover:opacity-90 disabled:opacity-50"
            >
              Create
            </button>
          </div>
        </div>

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
                    className="px-3 py-1.5 text-xs rounded-md border border-red-300 text-red-600 hover:bg-red-50 dark:border-red-800 dark:text-red-400 dark:hover:bg-red-900/20"
                  >
                    Delete
                  </button>
                )}
              </div>
            </div>
          ))}
          {(!memberships || memberships.length === 0) && (
            <p className="text-sm text-text-secondary">No organizations yet. Create one to get started.</p>
          )}
        </div>
      </div>
    </div>
  )
}
