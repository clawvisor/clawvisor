import { useState } from 'react'
import { useQuery, useMutation } from '@tanstack/react-query'
import { api, type OrgMember } from '../api/client'
import { useAuth } from '../hooks/useAuth'

export default function OrgMembers() {
  const { currentOrg } = useAuth()
  const [inviteEmail, setInviteEmail] = useState('')
  const [inviteRole, setInviteRole] = useState('member')

  const orgId = currentOrg?.id ?? ''

  // Caller's role in this org. The backend only lets admins/owners
  // list invites or send new ones (requireOrgAdmin on /api/orgs/{id}/invites),
  // so we need the role here to (1) avoid the 403 on the invites query
  // for plain members, and (2) hide the invite UI from non-admins
  // entirely. Shared query key with OrgSelector so the cache dedupes.
  const { data: memberships } = useQuery({
    queryKey: ['orgs'],
    queryFn: () => api.orgs.list(),
    staleTime: 60_000,
  })
  const myRole = memberships?.find((m) => m.org.id === orgId)?.role
  const canManageInvites = myRole === 'owner' || myRole === 'admin'
  // Role changes are owner-only on the server (the role-update endpoint
  // requires owner). Admins can still remove members. Splitting these
  // capabilities locally keeps admins from hitting a guaranteed 403
  // every time they touch the role select.
  const canChangeRoles = myRole === 'owner'

  const { data: members, refetch: refetchMembers } = useQuery({
    queryKey: ['org-members', orgId],
    queryFn: () => api.orgs.members.list(orgId),
    enabled: !!orgId,
  })

  const { data: invites, refetch: refetchInvites } = useQuery({
    queryKey: ['org-invites', orgId],
    queryFn: () => api.orgs.invites.list(orgId),
    enabled: !!orgId && canManageInvites,
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
    return (
      <div className="p-4 sm:p-8">
        <p className="text-sm text-text-secondary">Select an organization to manage members.</p>
      </div>
    )
  }

  const roleOptions: { value: string; label: string }[] = [
    { value: 'member', label: 'Member' },
    { value: 'admin', label: 'Admin' },
  ]

  return (
    <div className="p-4 sm:p-8 space-y-10">
      <h1 className="text-2xl font-bold text-text-primary">Members</h1>
      <div className="space-y-8">

        {/* Invite — admin/owner only. The backend requires admin+ on
            the create/list/delete invite endpoints, so showing this
            UI to plain members would surface 403s as broken inputs. */}
        {canManageInvites && (
          <div className="bg-surface-1 rounded-lg border border-border-default p-4 mb-6">
            <h3 className="text-sm font-medium text-text-primary mb-3">Invite Member</h3>
            <div className="flex gap-3">
              <input
                value={inviteEmail}
                onChange={(e) => setInviteEmail(e.target.value)}
                placeholder="email@example.com"
                type="email"
                className="flex-1 px-3 py-2 text-sm rounded-md border border-border-default bg-surface-0 text-text-primary focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand placeholder:text-text-tertiary"
              />
              <select
                value={inviteRole}
                onChange={(e) => setInviteRole(e.target.value)}
                className="px-3 py-2 text-sm rounded-md border border-border-default bg-surface-0 text-text-primary focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand placeholder:text-text-tertiary"
              >
                {roleOptions.map((o) => (
                  <option key={o.value} value={o.value}>{o.label}</option>
                ))}
              </select>
              <button
                onClick={() => createInvite.mutate()}
                disabled={!inviteEmail || createInvite.isPending}
                className="px-4 py-2 text-sm font-medium rounded-md bg-brand text-white hover:bg-brand-strong disabled:opacity-50"
              >
                Invite
              </button>
            </div>
            {createInvite.isError && (
              <p className="mt-2 text-xs text-danger">{(createInvite.error as Error)?.message}</p>
            )}
          </div>
        )}

        {/* Members list */}
        <div className="space-y-2 mb-6">
          {members?.map((m: OrgMember) => (
            <div key={m.id} className="bg-surface-1 rounded-lg border border-border-default p-3 flex items-center justify-between">
              <div>
                <span className="text-sm text-text-primary">{m.email ?? m.user_id}</span>
                <span className="ml-2 text-xs px-1.5 py-0.5 rounded bg-surface-0 text-text-secondary">{m.role}</span>
              </div>
              {/* Role change is owner-only (the role-change endpoint
                  is gated on owner); remove is admin+. Splitting the
                  two prevents admins from seeing and triggering a
                  control that would always 403. */}
              {canManageInvites && (
                <div className="flex items-center gap-2">
                  {m.role !== 'owner' && canChangeRoles && (
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
                      className="text-xs px-2 py-1 rounded border border-danger/40 text-danger hover:bg-danger/10"
                    >
                      Remove
                    </button>
                  )}
                </div>
              )}
            </div>
          ))}
        </div>

        {/* Pending invites — only fetched when canManageInvites is
            true, so this also implicitly hides for non-admins. */}
        {canManageInvites && invites && invites.length > 0 && (
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
