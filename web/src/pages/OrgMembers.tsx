import { useState } from 'react'
import { useQuery, useMutation } from '@tanstack/react-query'
import { api, type OrgMember } from '../api/client'
import { useAuth } from '../hooks/useAuth'

export default function OrgMembers() {
  const { currentOrg } = useAuth()
  const [inviteEmail, setInviteEmail] = useState('')
  const [inviteRole, setInviteRole] = useState('member')

  const orgId = currentOrg?.id ?? ''

  const { data: members, refetch: refetchMembers } = useQuery({
    queryKey: ['org-members', orgId],
    queryFn: () => api.orgs.members.list(orgId),
    enabled: !!orgId,
  })

  const { data: invites, refetch: refetchInvites } = useQuery({
    queryKey: ['org-invites', orgId],
    queryFn: () => api.orgs.invites.list(orgId),
    enabled: !!orgId,
  })

  const createInvite = useMutation({
    mutationFn: () => api.orgs.invites.create(orgId, inviteEmail, inviteRole),
    onSuccess: () => {
      setInviteEmail('')
      refetchInvites()
    },
  })

  const removeMember = useMutation({
    mutationFn: (userId: string) => api.orgs.members.remove(orgId, userId),
    onSuccess: () => refetchMembers(),
  })

  const updateRole = useMutation({
    mutationFn: ({ userId, role }: { userId: string; role: string }) =>
      api.orgs.members.updateRole(orgId, userId, role),
    onSuccess: () => refetchMembers(),
  })

  const revokeInvite = useMutation({
    mutationFn: (inviteId: string) => api.orgs.invites.delete(orgId, inviteId),
    onSuccess: () => refetchInvites(),
  })

  if (!currentOrg) {
    return <p className="text-sm text-text-secondary">Select an organization to manage members.</p>
  }

  const roleOptions: { value: string; label: string }[] = [
    { value: 'member', label: 'Member' },
    { value: 'admin', label: 'Admin' },
  ]

  return (
    <div className="space-y-8">
      <div>
        <h2 className="text-lg font-semibold text-text-primary mb-4">
          Members &mdash; {currentOrg.name}
        </h2>

        {/* Invite */}
        <div className="bg-surface-1 rounded-lg border border-border-default p-4 mb-6">
          <h3 className="text-sm font-medium text-text-primary mb-3">Invite Member</h3>
          <div className="flex gap-3">
            <input
              value={inviteEmail}
              onChange={(e) => setInviteEmail(e.target.value)}
              placeholder="email@example.com"
              type="email"
              className="flex-1 px-3 py-2 text-sm rounded-md border border-border-default bg-surface-0 text-text-primary"
            />
            <select
              value={inviteRole}
              onChange={(e) => setInviteRole(e.target.value)}
              className="px-3 py-2 text-sm rounded-md border border-border-default bg-surface-0 text-text-primary"
            >
              {roleOptions.map((o) => (
                <option key={o.value} value={o.value}>{o.label}</option>
              ))}
            </select>
            <button
              onClick={() => createInvite.mutate()}
              disabled={!inviteEmail || createInvite.isPending}
              className="px-4 py-2 text-sm font-medium rounded-md bg-accent-primary text-white hover:opacity-90 disabled:opacity-50"
            >
              Invite
            </button>
          </div>
        </div>

        {/* Members list */}
        <div className="space-y-2 mb-6">
          {members?.map((m: OrgMember) => (
            <div key={m.id} className="bg-surface-1 rounded-lg border border-border-default p-3 flex items-center justify-between">
              <div>
                <span className="text-sm text-text-primary">{m.email ?? m.user_id}</span>
                <span className="ml-2 text-xs px-1.5 py-0.5 rounded bg-surface-0 text-text-secondary">{m.role}</span>
              </div>
              <div className="flex items-center gap-2">
                {m.role !== 'owner' && (
                  <select
                    value={m.role}
                    onChange={(e) => updateRole.mutate({ userId: m.user_id, role: e.target.value })}
                    className="text-xs px-2 py-1 rounded border border-border-default bg-surface-0 text-text-primary"
                  >
                    <option value="member">Member</option>
                    <option value="admin">Admin</option>
                  </select>
                )}
                {m.role !== 'owner' && (
                  <button
                    onClick={() => removeMember.mutate(m.user_id)}
                    className="text-xs px-2 py-1 rounded border border-red-300 text-red-600 hover:bg-red-50 dark:border-red-800 dark:text-red-400 dark:hover:bg-red-900/20"
                  >
                    Remove
                  </button>
                )}
              </div>
            </div>
          ))}
        </div>

        {/* Pending invites */}
        {invites && invites.length > 0 && (
          <div>
            <h3 className="text-sm font-medium text-text-primary mb-3">Pending Invites</h3>
            <div className="space-y-2">
              {invites.map((inv) => (
                <div key={inv.id} className="bg-surface-1 rounded-lg border border-border-default p-3 flex items-center justify-between">
                  <div>
                    <span className="text-sm text-text-primary">{inv.email}</span>
                    <span className="ml-2 text-xs text-text-secondary">as {inv.role}</span>
                  </div>
                  <button
                    onClick={() => revokeInvite.mutate(inv.id)}
                    className="text-xs px-2 py-1 rounded border border-border-default hover:bg-surface-0 text-text-secondary"
                  >
                    Revoke
                  </button>
                </div>
              ))}
            </div>
          </div>
        )}
      </div>
    </div>
  )
}
