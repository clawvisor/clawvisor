import { useMemo, useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import {
  api,
  type Team,
  type TeamMember,
  type TeamSpendCap,
  type OrgMember,
} from '../api/client'
import { useAuth } from '../hooks/useAuth'

type TabKey = 'members' | 'spend_caps'
type WindowKind = 'daily' | 'monthly'

function microsToUsdString(micros: number): string {
  return (micros / 1_000_000).toFixed(2)
}

function usdStringToMicros(usd: string): number {
  const n = parseFloat(usd)
  if (!isFinite(n) || n <= 0) return 0
  return Math.round(n * 1_000_000)
}

export default function OrgTeams() {
  const { currentOrg } = useAuth()
  const orgId = currentOrg?.id ?? ''
  const queryClient = useQueryClient()

  const [selectedTeamId, setSelectedTeamId] = useState<string | null>(null)
  const [creatingTeam, setCreatingTeam] = useState(false)
  const [newTeamName, setNewTeamName] = useState('')
  const [activeTab, setActiveTab] = useState<TabKey>('members')

  // Caller's role in this org. Team list + per-team member list are
  // member-readable, but create/delete team, add/remove members, and
  // spend cap CRUD all require admin+. Hide the write affordances
  // from plain members rather than surfacing 403s mid-action.
  const { data: memberships } = useQuery({
    queryKey: ['orgs'],
    queryFn: () => api.orgs.list(),
    staleTime: 60_000,
  })
  const myRole = memberships?.find((m) => m.org.id === orgId)?.role
  const canManage = myRole === 'owner' || myRole === 'admin'

  const { data: teams } = useQuery({
    queryKey: ['teams', orgId],
    queryFn: () => api.orgs.teams.list(orgId),
    enabled: !!orgId,
  })

  // Single server call returns all (team, user) pairs for the org. Drives
  // both the per-team member count badges AND the org-wide assignment
  // section below, so we don't N+1 fan out one request per team.
  const { data: allTeamMembers } = useQuery({
    queryKey: ['team-members-all', orgId],
    queryFn: () => api.orgs.teams.members.listAll(orgId),
    enabled: !!orgId,
  })

  const counts = useMemo<Record<string, number>>(() => {
    if (!allTeamMembers) return {}
    const out: Record<string, number> = {}
    for (const m of allTeamMembers) out[m.team_id] = (out[m.team_id] ?? 0) + 1
    return out
  }, [allTeamMembers])

  const createTeam = useMutation({
    mutationFn: (name: string) => api.orgs.teams.create(orgId, name),
    onSuccess: (team) => {
      setNewTeamName('')
      setCreatingTeam(false)
      setSelectedTeamId(team.id)
      queryClient.invalidateQueries({ queryKey: ['teams', orgId] })
    },
  })

  if (!currentOrg) {
    return <p className="text-sm text-text-secondary">Select an organization to manage teams.</p>
  }

  const selectedTeam = teams?.find((t) => t.id === selectedTeamId) ?? null

  return (
    <div className="p-4 sm:p-8 space-y-10">
      <div className="flex items-center justify-between">
        <h1 className="text-2xl font-bold text-text-primary">Teams</h1>
        {canManage && !creatingTeam && (
          <button
            onClick={() => setCreatingTeam(true)}
            className="px-4 py-2 text-sm font-medium rounded-md bg-brand text-white hover:bg-brand-strong"
          >
            Create team
          </button>
        )}
      </div>

      {creatingTeam && (
        <div className="bg-surface-1 rounded-md border border-border-default p-4">
          <h3 className="text-sm font-medium text-text-primary mb-3">New team</h3>
          <div className="flex gap-3">
            <input
              value={newTeamName}
              onChange={(e) => setNewTeamName(e.target.value)}
              placeholder="Team name"
              className="flex-1 px-3 py-2 text-sm rounded-md border border-border-default bg-surface-0 text-text-primary focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand placeholder:text-text-tertiary"
            />
            <button
              onClick={() => createTeam.mutate(newTeamName)}
              disabled={!newTeamName.trim() || createTeam.isPending}
              className="px-4 py-2 text-sm font-medium rounded-md bg-brand text-white hover:bg-brand-strong disabled:opacity-50"
            >
              Create
            </button>
            <button
              onClick={() => { setCreatingTeam(false); setNewTeamName('') }}
              className="px-4 py-2 text-sm rounded-md border border-border-default text-text-secondary hover:bg-surface-0"
            >
              Cancel
            </button>
          </div>
          {createTeam.isError && (
            <div className="mt-2 text-xs text-danger">
              {(createTeam.error as Error)?.message}
            </div>
          )}
        </div>
      )}

      {teams && teams.length === 0 && !creatingTeam && (
        <div className="bg-surface-1 rounded-md border border-border-default p-6">
          <p className="text-sm text-text-secondary">
            Teams let you group agents and apply per-team spend caps that compose with the org cap.
            Create your first team to get started.
          </p>
        </div>
      )}

      {teams && teams.length > 0 && (
        <OrgMembersTeamAssignment
          orgId={orgId}
          teams={teams}
          allTeamMembers={allTeamMembers ?? []}
          canManage={canManage}
        />
      )}

      {teams && teams.length > 0 && (
        <div className="grid grid-cols-1 lg:grid-cols-3 gap-6">
          {/* Teams list */}
          <div className="space-y-2 lg:col-span-1">
            {teams.map((t) => (
              <button
                key={t.id}
                onClick={() => { setSelectedTeamId(t.id); setActiveTab('members') }}
                className={`w-full text-left bg-surface-1 rounded-md border p-3 hover:border-brand transition-colors ${
                  selectedTeamId === t.id ? 'border-brand' : 'border-border-default'
                }`}
              >
                <div className="flex items-center justify-between">
                  <span className="text-sm font-medium text-text-primary">{t.name}</span>
                  <span className="text-xs px-1.5 py-0.5 rounded bg-surface-0 text-text-secondary">
                    {counts[t.id] ?? 0} {(counts[t.id] ?? 0) === 1 ? 'member' : 'members'}
                  </span>
                </div>
                <div className="mt-1 text-xs text-text-secondary">Manage</div>
              </button>
            ))}
          </div>

          {/* Detail */}
          <div className="lg:col-span-2">
            {selectedTeam ? (
              <TeamDetail
                key={selectedTeam.id}
                orgId={orgId}
                team={selectedTeam}
                activeTab={activeTab}
                onTabChange={setActiveTab}
                onDeleted={() => setSelectedTeamId(null)}
                canManage={canManage}
              />
            ) : (
              <div className="bg-surface-1 rounded-md border border-border-default p-6">
                <p className="text-sm text-text-secondary">Select a team to manage members and spend caps.</p>
              </div>
            )}
          </div>
        </div>
      )}
    </div>
  )
}

// ── Team detail ──────────────────────────────────────────────────────────────

interface TeamDetailProps {
  orgId: string
  team: Team
  activeTab: TabKey
  onTabChange: (tab: TabKey) => void
  onDeleted: () => void
  canManage: boolean
}

function TeamDetail({ orgId, team, activeTab, onTabChange, onDeleted, canManage }: TeamDetailProps) {
  const queryClient = useQueryClient()
  const [renaming, setRenaming] = useState(false)
  const [renameValue, setRenameValue] = useState(team.name)

  const updateTeam = useMutation({
    mutationFn: (name: string) => api.orgs.teams.update(orgId, team.id, name),
    onSuccess: () => {
      setRenaming(false)
      queryClient.invalidateQueries({ queryKey: ['teams', orgId] })
    },
  })

  const deleteTeam = useMutation({
    mutationFn: () => api.orgs.teams.delete(orgId, team.id),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ['teams', orgId] })
      // team-members-all powers the org-wide assignment grid; without
      // invalidating it the grid will keep showing the deleted team's
      // memberships until a hard reload.
      queryClient.invalidateQueries({ queryKey: ['team-members-all', orgId] })
      onDeleted()
    },
  })

  const onDelete = () => {
    const ok = window.confirm(
      `Delete team "${team.name}"? All members will be unassigned, any agents on this team will have their team cleared, and team-level spend caps will be removed. The team itself is soft-deleted and can be re-created with the same name.`,
    )
    if (ok) deleteTeam.mutate()
  }

  return (
    <div className="bg-surface-1 rounded-md border border-border-default">
      {/* Header */}
      <div className="p-4 border-b border-border-default">
        <div className="flex items-center justify-between gap-3">
          {renaming ? (
            <div className="flex-1 flex gap-2">
              <input
                value={renameValue}
                onChange={(e) => setRenameValue(e.target.value)}
                className="flex-1 px-3 py-1.5 text-sm rounded-md border border-border-default bg-surface-0 text-text-primary focus:outline-none focus:ring-1 focus:ring-brand/30 focus:border-brand placeholder:text-text-tertiary"
              />
              <button
                onClick={() => updateTeam.mutate(renameValue)}
                disabled={!renameValue.trim() || updateTeam.isPending}
                className="px-3 py-1.5 text-sm font-medium rounded-md bg-brand text-white hover:bg-brand-strong disabled:opacity-50"
              >
                Save
              </button>
              <button
                onClick={() => { setRenaming(false); setRenameValue(team.name) }}
                className="px-3 py-1.5 text-sm rounded-md border border-border-default text-text-secondary hover:bg-surface-0"
              >
                Cancel
              </button>
            </div>
          ) : (
            <>
              <h3 className="text-base font-semibold text-text-primary">{team.name}</h3>
              {canManage && (
                <div className="flex items-center gap-2">
                  <button
                    onClick={() => { setRenameValue(team.name); setRenaming(true) }}
                    className="text-xs px-2 py-1 rounded border border-border-default text-text-secondary hover:bg-surface-0"
                  >
                    Rename
                  </button>
                  <button
                    onClick={onDelete}
                    disabled={deleteTeam.isPending}
                    className="text-xs px-2 py-1 rounded border border-danger/40 text-danger hover:bg-danger/10 disabled:opacity-50"
                  >
                    Delete
                  </button>
                </div>
              )}
            </>
          )}
        </div>
        {updateTeam.isError && (
          <div className="mt-2 text-xs text-danger">
            {(updateTeam.error as Error)?.message}
          </div>
        )}
        {deleteTeam.isError && (
          <div className="mt-2 text-xs text-danger">
            {(deleteTeam.error as Error)?.message}
          </div>
        )}

        {/* Tabs */}
        <div className="mt-4 flex gap-1 border-b border-border-default -mb-4">
          {(['members', 'spend_caps'] as TabKey[]).map((tab) => (
            <button
              key={tab}
              onClick={() => onTabChange(tab)}
              className={`px-3 py-2 text-sm border-b-2 -mb-px ${
                activeTab === tab
                  ? 'border-brand text-text-primary font-medium'
                  : 'border-transparent text-text-secondary hover:text-text-primary'
              }`}
            >
              {tab === 'members' ? 'Members' : 'Spend caps'}
            </button>
          ))}
        </div>
      </div>

      <div className="p-4">
        {activeTab === 'members' ? (
          <MembersTab orgId={orgId} teamId={team.id} canManage={canManage} />
        ) : (
          <SpendCapsTab orgId={orgId} teamId={team.id} canManage={canManage} />
        )}
      </div>
    </div>
  )
}

// ── Members tab ──────────────────────────────────────────────────────────────

function MembersTab({ orgId, teamId, canManage }: { orgId: string; teamId: string; canManage: boolean }) {
  const queryClient = useQueryClient()
  const [addUserId, setAddUserId] = useState('')
  const [addRole, setAddRole] = useState<'lead' | 'member'>('member')
  const [removeErrors, setRemoveErrors] = useState<Record<string, string>>({})

  const { data: members } = useQuery({
    queryKey: ['team', orgId, teamId, 'members'],
    queryFn: () => api.orgs.teams.members.list(orgId, teamId),
    enabled: !!orgId && !!teamId,
  })

  const { data: orgMembers } = useQuery({
    queryKey: ['org-members', orgId],
    queryFn: () => api.orgs.members.list(orgId),
    enabled: !!orgId,
  })

  const invalidate = () => {
    queryClient.invalidateQueries({ queryKey: ['team', orgId, teamId, 'members'] })
    queryClient.invalidateQueries({ queryKey: ['team-members-all', orgId] })
  }

  const addMember = useMutation({
    mutationFn: () => api.orgs.teams.members.add(orgId, teamId, addUserId, addRole),
    onSuccess: () => {
      setAddUserId('')
      setAddRole('member')
      invalidate()
    },
  })

  const removeMember = useMutation({
    mutationFn: (userId: string) => api.orgs.teams.members.remove(orgId, teamId, userId),
    onSuccess: (_d, userId) => {
      setRemoveErrors((prev) => {
        const next = { ...prev }
        delete next[userId]
        return next
      })
      invalidate()
    },
    onError: (err, userId) =>
      setRemoveErrors((prev) => ({
        ...prev,
        [userId]: (err as Error)?.message ?? 'Failed to remove member.',
      })),
  })

  const memberUserIds = new Set((members ?? []).map((m) => m.user_id))
  const addable: OrgMember[] = (orgMembers ?? []).filter((m) => !memberUserIds.has(m.user_id))

  const labelFor = (userId: string): string => {
    const om = orgMembers?.find((m) => m.user_id === userId)
    return om?.email ?? userId
  }

  return (
    <div className="space-y-4">
      {/* Add member — admin/owner only */}
      {canManage && (
        <div className="bg-surface-0 rounded-md border border-border-default p-3">
          <h4 className="text-sm font-medium text-text-primary mb-2">Add member</h4>
          <div className="flex gap-2">
            <select
              value={addUserId}
              onChange={(e) => setAddUserId(e.target.value)}
              className="flex-1 px-3 py-2 text-sm rounded-md border border-border-default bg-surface-1 text-text-primary"
            >
              <option value="">Select org member…</option>
              {addable.map((m) => (
                <option key={m.user_id} value={m.user_id}>
                  {m.email ?? m.user_id}
                </option>
              ))}
            </select>
            <select
              value={addRole}
              onChange={(e) => setAddRole(e.target.value as 'lead' | 'member')}
              className="px-3 py-2 text-sm rounded-md border border-border-default bg-surface-1 text-text-primary"
            >
              <option value="member">Member</option>
              <option value="lead">Lead</option>
            </select>
            <button
              onClick={() => addMember.mutate()}
              disabled={!addUserId || addMember.isPending}
              className="px-4 py-2 text-sm font-medium rounded-md bg-brand text-white hover:bg-brand-strong disabled:opacity-50"
            >
              Add
            </button>
          </div>
          {addable.length === 0 && orgMembers && orgMembers.length > 0 && (
            <p className="mt-2 text-xs text-text-secondary">All org members are on this team.</p>
          )}
          {addMember.isError && (
            <div className="mt-2 text-xs text-danger">
              {(addMember.error as Error)?.message}
            </div>
          )}
        </div>
      )}

      {/* Members list */}
      <div className="space-y-2">
        {members?.map((m: TeamMember) => (
          <div key={m.user_id} className="bg-surface-0 rounded-md border border-border-default p-3">
            <div className="flex items-center justify-between">
              <div>
                <span className="text-sm text-text-primary">{labelFor(m.user_id)}</span>
                <span className="ml-2 text-xs px-1.5 py-0.5 rounded bg-surface-1 text-text-secondary">{m.role}</span>
                <span className="ml-2 text-xs text-text-secondary">
                  added {new Date(m.created_at).toLocaleDateString()}
                </span>
              </div>
              {canManage && (
                <button
                  onClick={() => removeMember.mutate(m.user_id)}
                  disabled={removeMember.isPending}
                  className="text-xs px-2 py-1 rounded border border-danger/40 text-danger hover:bg-danger/10 disabled:opacity-50"
                >
                  Remove
                </button>
              )}
            </div>
            {removeErrors[m.user_id] && (
              <div className="mt-2 text-xs text-danger">{removeErrors[m.user_id]}</div>
            )}
          </div>
        ))}
        {members && members.length === 0 && (
          <p className="text-sm text-text-secondary">No members yet.</p>
        )}
      </div>
    </div>
  )
}

// ── Spend caps tab ───────────────────────────────────────────────────────────

function SpendCapsTab({ orgId, teamId, canManage }: { orgId: string; teamId: string; canManage: boolean }) {
  const queryClient = useQueryClient()

  const { data: caps } = useQuery({
    queryKey: ['team', orgId, teamId, 'spend-caps'],
    queryFn: () => api.orgs.teams.spendCaps.list(orgId, teamId),
    enabled: !!orgId && !!teamId,
  })

  const invalidate = () =>
    queryClient.invalidateQueries({ queryKey: ['team', orgId, teamId, 'spend-caps'] })

  const byWindow: Record<WindowKind, TeamSpendCap | undefined> = {
    daily: caps?.find((c) => c.window_kind === 'daily'),
    monthly: caps?.find((c) => c.window_kind === 'monthly'),
  }

  return (
    <div className="space-y-3">
      <p className="text-xs text-text-secondary">
        Team spend caps compose with the org cap — the lower limit wins.
        Hard caps block spending; soft caps emit a violation event but allow the call.
      </p>
      {(['daily', 'monthly'] as WindowKind[]).map((w) => (
        <SpendCapRow
          key={w}
          orgId={orgId}
          teamId={teamId}
          windowKind={w}
          cap={byWindow[w]}
          onChange={invalidate}
          canManage={canManage}
        />
      ))}
    </div>
  )
}

interface SpendCapRowProps {
  orgId: string
  teamId: string
  windowKind: WindowKind
  cap: TeamSpendCap | undefined
  onChange: () => void
  canManage: boolean
}

function SpendCapRow({ orgId, teamId, windowKind, cap, onChange, canManage }: SpendCapRowProps) {
  const [editing, setEditing] = useState(false)
  const [usdValue, setUsdValue] = useState(cap ? microsToUsdString(cap.cap_micros) : '')
  const [enforcement, setEnforcement] = useState<'soft' | 'hard'>(cap?.enforcement ?? 'soft')
  const [error, setError] = useState<string | null>(null)

  const save = useMutation({
    mutationFn: () => {
      const micros = usdStringToMicros(usdValue)
      if (micros <= 0) throw new Error('Cap must be greater than 0')
      return api.orgs.teams.spendCaps.put(orgId, teamId, windowKind, micros, enforcement)
    },
    onSuccess: () => {
      setEditing(false)
      setError(null)
      onChange()
    },
    onError: (e: Error) => setError(e.message),
  })

  const remove = useMutation({
    mutationFn: () => api.orgs.teams.spendCaps.delete(orgId, teamId, windowKind),
    onSuccess: () => {
      setEditing(false)
      setUsdValue('')
      setEnforcement('soft')
      setError(null)
      onChange()
    },
    onError: (e: Error) => setError(e.message ?? 'Failed to delete spend cap.'),
  })

  const startEdit = () => {
    setUsdValue(cap ? microsToUsdString(cap.cap_micros) : '')
    setEnforcement(cap?.enforcement ?? 'soft')
    setError(null)
    setEditing(true)
  }

  const label = windowKind === 'daily' ? 'Daily' : 'Monthly'

  if (!editing) {
    return (
      <div className="bg-surface-0 rounded-md border border-border-default p-3">
        <div className="flex items-center justify-between">
          <div>
            <span className="text-sm font-medium text-text-primary">{label}</span>
            {cap ? (
              <>
                <span className="ml-3 text-sm text-text-primary">${microsToUsdString(cap.cap_micros)}</span>
                <span className="ml-2 text-xs px-1.5 py-0.5 rounded bg-surface-1 text-text-secondary">{cap.enforcement}</span>
              </>
            ) : (
              <span className="ml-3 text-sm text-text-secondary">no cap</span>
            )}
          </div>
          {canManage && (
            <div className="flex items-center gap-2">
              <button
                onClick={startEdit}
                className="text-xs px-2 py-1 rounded border border-border-default text-text-secondary hover:bg-surface-1"
              >
                {cap ? 'Edit' : 'Set cap'}
              </button>
              {cap && (
                <button
                  onClick={() => remove.mutate()}
                  disabled={remove.isPending}
                  className="text-xs px-2 py-1 rounded border border-danger/40 text-danger hover:bg-danger/10 disabled:opacity-50"
                >
                  Delete
                </button>
              )}
            </div>
          )}
        </div>
        {error && <p className="mt-2 text-xs text-danger">{error}</p>}
      </div>
    )
  }

  return (
    <div className="bg-surface-0 rounded-md border border-border-default p-3">
      <div className="flex items-center gap-2">
        <span className="text-sm font-medium text-text-primary w-20">{label}</span>
        <span className="text-sm text-text-secondary">$</span>
        <input
          type="number"
          step="0.01"
          min="0"
          value={usdValue}
          onChange={(e) => setUsdValue(e.target.value)}
          placeholder="100.00"
          className="w-28 px-2 py-1.5 text-sm rounded-md border border-border-default bg-surface-1 text-text-primary"
        />
        <select
          value={enforcement}
          onChange={(e) => setEnforcement(e.target.value as 'soft' | 'hard')}
          className="px-2 py-1.5 text-sm rounded-md border border-border-default bg-surface-1 text-text-primary"
        >
          <option value="soft">Soft</option>
          <option value="hard">Hard</option>
        </select>
        <button
          onClick={() => save.mutate()}
          disabled={save.isPending}
          className="text-xs px-3 py-1.5 rounded-md bg-brand text-white font-medium hover:bg-brand-strong disabled:opacity-50"
        >
          Save
        </button>
        <button
          onClick={() => { setEditing(false); setError(null) }}
          className="text-xs px-3 py-1.5 rounded-md border border-border-default text-text-secondary hover:bg-surface-1"
        >
          Cancel
        </button>
      </div>
      {error && <p className="mt-2 text-xs text-danger">{error}</p>}
    </div>
  )
}

// ── Org-wide member→team assignment grid ──────────────────────────────
//
// Surfaces all org members at the top of the Teams page so an admin can
// see each member's current team memberships and add/remove them
// inline. Without this, the only way to assign a member to a team was
// to drill into the team's Members tab — fine for one assignment but
// not for sweeping through the roster.

interface OrgMembersTeamAssignmentProps {
  orgId: string
  teams: Team[]
  allTeamMembers: TeamMember[]
  canManage: boolean
}

function OrgMembersTeamAssignment({ orgId, teams, allTeamMembers, canManage }: OrgMembersTeamAssignmentProps) {
  const queryClient = useQueryClient()

  const { data: orgMembers } = useQuery({
    queryKey: ['org-members', orgId],
    queryFn: () => api.orgs.members.list(orgId),
    enabled: !!orgId,
  })

  // Per-row error keyed by user_id so an add/remove failure on one row
  // doesn't taint another.
  const [rowErrors, setRowErrors] = useState<Record<string, string>>({})

  const invalidate = () => {
    queryClient.invalidateQueries({ queryKey: ['team-members-all', orgId] })
    // The drilled-in team views read from a different key set; invalidate
    // them too so the right pane stays in sync if a team is open. Scope
    // the invalidation to the current org so we don't churn unrelated
    // org caches if the user has switched between orgs.
    queryClient.invalidateQueries({ queryKey: ['team', orgId] })
  }

  const setRowError = (userId: string, msg: string | null) => {
    setRowErrors((prev) => {
      const next = { ...prev }
      if (msg) next[userId] = msg
      else delete next[userId]
      return next
    })
  }

  const addToTeam = useMutation({
    mutationFn: ({ userId, teamId }: { userId: string; teamId: string }) =>
      api.orgs.teams.members.add(orgId, teamId, userId, 'member'),
    onSuccess: (_d, vars) => { setRowError(vars.userId, null); invalidate() },
    onError: (err, vars) =>
      setRowError(vars.userId, (err as Error)?.message ?? 'Failed to add to team.'),
  })

  const removeFromTeam = useMutation({
    mutationFn: ({ userId, teamId }: { userId: string; teamId: string }) =>
      api.orgs.teams.members.remove(orgId, teamId, userId),
    onSuccess: (_d, vars) => { setRowError(vars.userId, null); invalidate() },
    onError: (err, vars) =>
      setRowError(vars.userId, (err as Error)?.message ?? 'Failed to remove from team.'),
  })

  // Build (userId → set of teamIds the user belongs to) once for cheap lookup.
  const teamsByUser = useMemo<Record<string, Set<string>>>(() => {
    const out: Record<string, Set<string>> = {}
    for (const m of allTeamMembers) {
      if (!out[m.user_id]) out[m.user_id] = new Set()
      out[m.user_id].add(m.team_id)
    }
    return out
  }, [allTeamMembers])

  if (!orgMembers || orgMembers.length === 0) {
    return null
  }
  // Read-only members: don't render this whole grid. The per-team
  // tabs already show membership; without write affordances there's
  // nothing useful for a non-admin to do here.
  if (!canManage) {
    return null
  }

  return (
    <div className="bg-surface-1 rounded-md border border-border-default">
      <div className="p-4 border-b border-border-default">
        <h3 className="text-base font-semibold text-text-primary">Org members</h3>
        <p className="mt-1 text-xs text-text-secondary">
          Add or remove team assignments for any member of the organization. Changes
          take effect immediately.
        </p>
      </div>
      <div className="divide-y divide-border-default">
        {orgMembers.map((m: OrgMember) => {
          const currentTeams = teamsByUser[m.user_id] ?? new Set<string>()
          const availableToAdd = teams.filter((t) => !currentTeams.has(t.id))
          return (
            <div key={m.user_id} className="p-4">
              <div className="flex items-start justify-between gap-4">
                <div className="min-w-0 flex-1">
                  <div className="flex items-center gap-2">
                    <span className="text-sm text-text-primary truncate">{m.email ?? m.user_id}</span>
                    <span className="text-xs px-1.5 py-0.5 rounded bg-surface-0 text-text-secondary shrink-0">
                      {m.role}
                    </span>
                  </div>
                  <div className="mt-2 flex flex-wrap gap-1.5 items-center">
                    {teams
                      .filter((t) => currentTeams.has(t.id))
                      .map((t) => (
                        <span
                          key={t.id}
                          className="inline-flex items-center gap-1 px-2 py-0.5 rounded-full text-xs bg-brand/10 text-brand border border-brand/30"
                        >
                          {t.name}
                          <button
                            type="button"
                            onClick={() =>
                              removeFromTeam.mutate({ userId: m.user_id, teamId: t.id })
                            }
                            disabled={removeFromTeam.isPending}
                            className="hover:text-brand/80 disabled:opacity-50"
                            aria-label={`Remove ${m.email ?? m.user_id} from ${t.name}`}
                          >
                            ×
                          </button>
                        </span>
                      ))}
                    {currentTeams.size === 0 && (
                      <span className="text-xs text-text-secondary">Not on any team.</span>
                    )}
                  </div>
                </div>
                {availableToAdd.length > 0 && (
                  <div className="shrink-0">
                    <select
                      value=""
                      onChange={(e) => {
                        const teamId = e.target.value
                        if (teamId) addToTeam.mutate({ userId: m.user_id, teamId })
                      }}
                      disabled={addToTeam.isPending}
                      className="text-xs px-2 py-1 rounded-md border border-border-default bg-surface-0 text-text-primary disabled:opacity-50"
                    >
                      <option value="">+ Add to team…</option>
                      {availableToAdd.map((t) => (
                        <option key={t.id} value={t.id}>{t.name}</option>
                      ))}
                    </select>
                  </div>
                )}
              </div>
              {rowErrors[m.user_id] && (
                <div className="mt-2 text-xs text-danger">{rowErrors[m.user_id]}</div>
              )}
            </div>
          )
        })}
      </div>
    </div>
  )
}
