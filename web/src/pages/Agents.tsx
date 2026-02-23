import { useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { api, type Agent, type AgentRole } from '../api/client'
import { formatDistanceToNow } from 'date-fns'

function RoleSelect({
  agent,
  roles,
}: {
  agent: Agent
  roles: AgentRole[]
}) {
  const qc = useQueryClient()
  const updateMut = useMutation({
    mutationFn: (roleId: string | null) => api.agents.updateRole(agent.id, roleId),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['agents'] }),
  })

  return (
    <select
      value={agent.role_id ?? ''}
      onChange={e => updateMut.mutate(e.target.value || null)}
      disabled={updateMut.isPending}
      className="text-xs rounded border border-gray-300 px-2 py-1 focus:outline-none focus:ring-1 focus:ring-blue-400 disabled:opacity-50"
    >
      <option value="">No role</option>
      {roles.map(r => (
        <option key={r.id} value={r.id}>{r.name}</option>
      ))}
    </select>
  )
}

export default function Agents() {
  const qc = useQueryClient()
  const [name, setName] = useState('')
  const [roleId, setRoleId] = useState('')
  const [newToken, setNewToken] = useState<string | null>(null)
  const [formError, setFormError] = useState<string | null>(null)

  const { data: agents, isLoading } = useQuery({
    queryKey: ['agents'],
    queryFn: () => api.agents.list(),
  })

  const { data: roles } = useQuery({
    queryKey: ['roles'],
    queryFn: () => api.roles.list(),
  })

  const createMut = useMutation({
    mutationFn: () => api.agents.create(name, roleId || undefined),
    onSuccess: (agent) => {
      qc.invalidateQueries({ queryKey: ['agents'] })
      setNewToken(agent.token ?? null)
      setName('')
      setRoleId('')
      setFormError(null)
    },
    onError: (err: Error) => setFormError(err.message),
  })

  const deleteMut = useMutation({
    mutationFn: (id: string) => api.agents.delete(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['agents'] }),
  })

  return (
    <div className="p-8 space-y-8">
      <h1 className="text-2xl font-bold text-gray-900">Agents</h1>

      {/* New token display */}
      {newToken && (
        <div className="bg-green-50 border border-green-200 rounded-lg p-4 space-y-2">
          <p className="text-sm font-medium text-green-800">Agent created — copy your token now, it won't be shown again.</p>
          <div className="flex items-center gap-2">
            <code className="flex-1 bg-white border border-green-200 rounded px-3 py-2 text-xs font-mono text-gray-800 break-all">
              {newToken}
            </code>
            <button
              onClick={() => navigator.clipboard.writeText(newToken)}
              className="text-xs px-3 py-1.5 rounded border border-green-300 text-green-700 hover:bg-green-100"
            >
              Copy
            </button>
          </div>
          <button onClick={() => setNewToken(null)} className="text-xs text-green-600 hover:underline">
            Dismiss
          </button>
        </div>
      )}

      {/* Create form */}
      <section className="bg-white border rounded-lg p-5 space-y-4">
        <h2 className="text-sm font-semibold text-gray-700">Create Agent</h2>
        {formError && <div className="text-xs text-red-600">{formError}</div>}
        <div className="flex gap-3">
          <input
            value={name}
            onChange={e => setName(e.target.value)}
            placeholder="Agent name"
            className="flex-1 text-sm rounded border border-gray-300 px-3 py-1.5 focus:outline-none focus:ring-1 focus:ring-blue-400"
          />
          {roles && roles.length > 0 && (
            <select
              value={roleId}
              onChange={e => setRoleId(e.target.value)}
              className="text-sm rounded border border-gray-300 px-2 py-1.5 focus:outline-none focus:ring-1 focus:ring-blue-400"
            >
              <option value="">No role</option>
              {roles.map(r => (
                <option key={r.id} value={r.id}>{r.name}</option>
              ))}
            </select>
          )}
          <button
            onClick={() => createMut.mutate()}
            disabled={createMut.isPending || !name.trim()}
            className="px-4 py-1.5 text-sm rounded bg-blue-600 text-white hover:bg-blue-700 disabled:opacity-50"
          >
            {createMut.isPending ? 'Creating…' : 'Create'}
          </button>
        </div>
      </section>

      {/* Roles management */}
      <section className="space-y-3">
        <div className="flex items-center justify-between">
          <h2 className="text-sm font-semibold text-gray-700">Roles</h2>
        </div>
        <RoleManager />
      </section>

      {/* Agent list */}
      {isLoading && <div className="text-sm text-gray-400">Loading…</div>}

      {!isLoading && (!agents || agents.length === 0) && (
        <div className="text-sm text-gray-400 text-center py-8">No agents yet. Create one above.</div>
      )}

      <div className="space-y-2">
        {agents?.map(agent => (
          <div key={agent.id} className="bg-white border rounded-lg px-5 py-4 flex items-center justify-between">
            <div>
              <span className="font-medium text-gray-900">{agent.name}</span>
              <p className="text-xs text-gray-400 mt-0.5">
                Created {formatDistanceToNow(new Date(agent.created_at), { addSuffix: true })} · {agent.id}
              </p>
            </div>
            <div className="flex items-center gap-3">
              {roles && (
                <RoleSelect agent={agent} roles={roles} />
              )}
              <button
                onClick={() => {
                  if (confirm(`Revoke agent "${agent.name}"? Any running agents using this token will stop working.`)) {
                    deleteMut.mutate(agent.id)
                  }
                }}
                disabled={deleteMut.isPending}
                className="text-xs px-3 py-1.5 rounded border border-red-200 text-red-600 hover:bg-red-50"
              >
                Revoke
              </button>
            </div>
          </div>
        ))}
      </div>
    </div>
  )
}

function RoleManager() {
  const qc = useQueryClient()
  const [name, setName] = useState('')
  const [desc, setDesc] = useState('')

  const { data: roles } = useQuery({
    queryKey: ['roles'],
    queryFn: () => api.roles.list(),
  })

  const createMut = useMutation({
    mutationFn: () => api.roles.create(name, desc),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ['roles'] }); setName(''); setDesc('') },
  })

  const deleteMut = useMutation({
    mutationFn: (id: string) => api.roles.delete(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['roles'] }),
  })

  return (
    <div className="bg-white border rounded-lg p-4 space-y-3">
      <div className="flex gap-2">
        <input
          value={name}
          onChange={e => setName(e.target.value)}
          placeholder="Role name"
          className="text-sm rounded border border-gray-300 px-2 py-1.5 focus:outline-none focus:ring-1 focus:ring-blue-400 w-36"
        />
        <input
          value={desc}
          onChange={e => setDesc(e.target.value)}
          placeholder="Description (optional)"
          className="text-sm rounded border border-gray-300 px-2 py-1.5 focus:outline-none focus:ring-1 focus:ring-blue-400 flex-1"
        />
        <button
          onClick={() => createMut.mutate()}
          disabled={createMut.isPending || !name.trim()}
          className="text-xs px-3 py-1.5 rounded bg-gray-700 text-white hover:bg-gray-800 disabled:opacity-50"
        >
          Add Role
        </button>
      </div>
      {roles && roles.length > 0 && (
        <div className="flex flex-wrap gap-2">
          {roles.map(r => (
            <span key={r.id} className="inline-flex items-center gap-1 bg-purple-50 text-purple-700 text-xs px-2 py-0.5 rounded-full">
              {r.name}
              {r.description && <span className="text-purple-400">· {r.description}</span>}
              <button
                onClick={() => deleteMut.mutate(r.id)}
                className="ml-1 text-purple-400 hover:text-purple-700"
                title="Delete role"
              >
                ×
              </button>
            </span>
          ))}
        </div>
      )}
    </div>
  )
}
