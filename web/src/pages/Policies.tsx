import { useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { api, type PolicyRecord } from '../api/client'
import { formatDistanceToNow } from 'date-fns'

export default function Policies() {
  const qc = useQueryClient()
  const [roleFilter, setRoleFilter] = useState('')

  const { data: roles } = useQuery({
    queryKey: ['roles'],
    queryFn: () => api.roles.list(),
  })

  const { data: policies, isLoading } = useQuery({
    queryKey: ['policies', roleFilter],
    queryFn: () => api.policies.list(roleFilter || undefined),
  })

  const deleteMut = useMutation({
    mutationFn: (id: string) => api.policies.delete(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['policies'] }),
  })

  async function handleDelete(p: PolicyRecord) {
    if (!confirm(`Delete policy "${p.name || p.slug}"?`)) return
    deleteMut.mutate(p.id)
  }

  return (
    <div className="p-8 space-y-6">
      <div className="flex items-center justify-between">
        <h1 className="text-2xl font-bold text-gray-900">Policies</h1>
        <Link
          to="/dashboard/policies/new"
          className="px-4 py-2 bg-blue-600 text-white text-sm rounded hover:bg-blue-700"
        >
          New Policy
        </Link>
      </div>

      {/* Role filter */}
      {roles && roles.length > 0 && (
        <div className="flex items-center gap-2 text-sm">
          <span className="text-gray-500">Filter by role:</span>
          <button
            onClick={() => setRoleFilter('')}
            className={`px-2 py-0.5 rounded-full border text-xs ${roleFilter === '' ? 'border-blue-400 text-blue-700 bg-blue-50' : 'border-gray-300 text-gray-500 hover:bg-gray-50'}`}
          >
            All agents (global)
          </button>
          {roles.map(r => (
            <button
              key={r.id}
              onClick={() => setRoleFilter(roleFilter === r.name ? '' : r.name)}
              className={`px-2 py-0.5 rounded-full border text-xs ${roleFilter === r.name ? 'border-blue-400 text-blue-700 bg-blue-50' : 'border-gray-300 text-gray-500 hover:bg-gray-50'}`}
            >
              {r.name}
            </button>
          ))}
        </div>
      )}

      {isLoading && <div className="text-sm text-gray-400">Loading…</div>}

      {!isLoading && (!policies || policies.length === 0) && (
        <div className="text-center py-12 text-gray-400">
          <p className="text-sm">No policies yet.</p>
          <Link to="/dashboard/policies/new" className="text-blue-600 text-sm hover:underline mt-1 inline-block">
            Create your first policy
          </Link>
        </div>
      )}

      <div className="space-y-2">
        {policies?.map(p => (
          <div key={p.id} className="bg-white border rounded-lg px-5 py-4 flex items-center justify-between">
            <div>
              <span className="font-medium text-gray-900">{p.name || p.slug}</span>
              {p.name && p.slug !== p.name && (
                <span className="ml-2 text-xs text-gray-400 font-mono">{p.slug}</span>
              )}
              {p.role_id ? (
                <span className="ml-2 text-xs bg-purple-50 text-purple-700 px-1.5 py-0.5 rounded">
                  {roles?.find(r => r.id === p.role_id)?.name ?? 'role'}
                </span>
              ) : (
                <span className="ml-2 text-xs text-gray-400">all agents</span>
              )}
              <p className="text-xs text-gray-400 mt-0.5">
                Updated {formatDistanceToNow(new Date(p.updated_at), { addSuffix: true })}
              </p>
            </div>
            <div className="flex gap-2">
              <Link
                to={`/dashboard/policies/${p.id}`}
                className="text-xs px-3 py-1.5 rounded border border-gray-300 text-gray-600 hover:bg-gray-50"
              >
                Edit
              </Link>
              <button
                onClick={() => handleDelete(p)}
                disabled={deleteMut.isPending}
                className="text-xs px-3 py-1.5 rounded border border-red-200 text-red-600 hover:bg-red-50"
              >
                Delete
              </button>
            </div>
          </div>
        ))}
      </div>
    </div>
  )
}
