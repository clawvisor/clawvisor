import { useState, useEffect } from 'react'
import { NavLink, Routes, Route, Navigate, useLocation, useNavigate } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { useAuth } from '../hooks/useAuth'
import { useEventStream } from '../hooks/useEventStream'
import { useTheme } from '../hooks/useTheme'
import { api } from '../api/client'
import Services from './Services'
import Policy from './Restrictions'
import Activity from './Audit'
import Agents from './Agents'
import Settings from './Settings'
import Overview from './Overview'
import GetStarted from './GetStarted'
import HowItWorks from './HowItWorks'
import Tasks from './Tasks'
import AdapterGen from './AdapterGen'
import OrgSettings from './OrgSettings'
import OrgMembers from './OrgMembers'
import OrgAdapters from './OrgAdapters'
import OrgMCPServers from './OrgMCPServers'
import Governance from './Governance'
import OrgNotifications from './OrgNotifications'
import OrgTeams from './OrgTeams'
import Billing from './Billing'
import KeyVault from './KeyVault'
import OrgSelector from '../components/OrgSelector'
import OnboardingBanner from '../components/OnboardingBanner'

const navItems = [
  { to: '/dashboard/how-it-works', label: 'How it works', icon: <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><circle cx="12" cy="12" r="10"/><path d="M12 16v-4M12 8h.01"/></svg> },
  { to: '/dashboard', label: 'Overview', end: true, icon: <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><rect x="3" y="3" width="7" height="7"/><rect x="14" y="3" width="7" height="7"/><rect x="3" y="14" width="7" height="7"/><rect x="14" y="14" width="7" height="7"/></svg> },
  { to: '/dashboard/get-started', label: 'Get Started', icon: <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" viewBox="0 0 24 24"><path d="M4.5 16.5c-1.5 1.26-2 5-2 5s3.74-.5 5-2c.71-.84.7-2.13-.09-2.91a2.18 2.18 0 0 0-2.91-.09z"/><path d="m12 15-3-3a22 22 0 0 1 2-3.95A12.88 12.88 0 0 1 22 2c0 2.72-.78 7.5-6 11a22.35 22.35 0 0 1-4 2z"/><path d="M9 12H4s.55-3.03 2-4c1.62-1.08 5 0 5 0"/><path d="M12 15v5s3.03-.55 4-2c1.08-1.62 0-5 0-5"/></svg> },
  { to: '/dashboard/tasks', label: 'Tasks', icon: <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><circle cx="12" cy="12" r="10"/><path d="M12 6v6l4 2"/></svg> },
  { to: '/dashboard/accounts', label: 'Accounts', icon: <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M4 6h16M4 12h16M4 18h16"/></svg> },
  { to: '/dashboard/policy', label: 'Policy', icon: <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z"/></svg> },
  { to: '/dashboard/agents', label: 'Agents', icon: <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M16 21v-2a4 4 0 00-4-4H5a4 4 0 00-4 4v2"/><circle cx="8.5" cy="7" r="4"/><path d="M20 8v6M23 11h-6"/></svg> },
  { to: '/dashboard/activity', label: 'Activity', icon: <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M12 20h9M16.5 3.5a2.121 2.121 0 013 3L7 19l-4 1 1-4L16.5 3.5z"/></svg> },
  { to: '/dashboard/settings', label: 'Settings', icon: <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M10.325 4.317c.426-1.756 2.924-1.756 3.35 0a1.724 1.724 0 002.573 1.066c1.543-.94 3.31.826 2.37 2.37a1.724 1.724 0 001.066 2.573c1.756.426 1.756 2.924 0 3.35a1.724 1.724 0 00-1.066 2.573c.94 1.543-.826 3.31-2.37 2.37a1.724 1.724 0 00-2.573 1.066c-.426 1.756-2.924 1.756-3.35 0a1.724 1.724 0 00-2.573-1.066c-1.543.94-3.31-.826-2.37-2.37a1.724 1.724 0 00-1.066-2.573c-1.756-.426-1.756-2.924 0-3.35a1.724 1.724 0 001.066-2.573c-.94-1.543.826-3.31 2.37-2.37.996.608 2.296.07 2.572-1.065z"/><circle cx="12" cy="12" r="3"/></svg> },
]

const billingNavItem = { to: '/dashboard/billing', label: 'Billing', end: undefined as boolean | undefined, icon: <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><rect x="1" y="4" width="22" height="16" rx="2" ry="2"/><path d="M1 10h22"/></svg> }

// Org sidebar — pared down while we figure out which surfaces customers
// will actually self-serve. Governance, notifications, SSO, custom
// adapters, and MCP servers are temporarily admin-managed via the
// cmd/admin tool, so they're hidden here. Routes remain wired in case
// of bookmarked links, but the nav doesn't surface them.
const orgNavItems = [
  { to: '/dashboard/org', label: 'Organization', end: true, icon: <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M17 21v-2a4 4 0 00-4-4H5a4 4 0 00-4 4v2"/><circle cx="9" cy="7" r="4"/><path d="M23 21v-2a4 4 0 00-3-3.87"/><path d="M16 3.13a4 4 0 010 7.75"/></svg> },
  { to: '/dashboard/org/members', label: 'Members', icon: <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M16 21v-2a4 4 0 00-4-4H5a4 4 0 00-4 4v2"/><circle cx="8.5" cy="7" r="4"/><path d="M20 8v6M23 11h-6"/></svg> },
  { to: '/dashboard/org/teams', label: 'Teams', icon: <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M17 21v-2a4 4 0 00-4-4H5a4 4 0 00-4 4v2"/><circle cx="9" cy="7" r="4"/><circle cx="17" cy="11" r="3"/></svg> },
]

export default function Dashboard() {
  const { user, logout, features, currentOrg } = useAuth()
  const { resolvedTheme, setTheme } = useTheme()
  const [sidebarOpen, setSidebarOpen] = useState(false)
  const location = useLocation()
  const navigate = useNavigate()
  const runtimeActivityUI = !!features?.runtime_activity

  // Close sidebar on route change (mobile)
  useEffect(() => { setSidebarOpen(false) }, [location.pathname])

  // SSE event stream for instant dashboard updates
  useEventStream()

  // Queue count for sidebar badge (SSE pushes invalidations)
  const { data: queueData } = useQuery({
    queryKey: ['queue'],
    queryFn: () => api.queue.list(),
    refetchInterval: 30_000,
  })
  const { data: runtimeStatus } = useQuery({
    queryKey: ['runtime-status'],
    queryFn: async () => {
      try {
        return await api.runtime.status()
      } catch {
        return null
      }
    },
    refetchInterval: 30_000,
    enabled: runtimeActivityUI,
  })
  const { data: runtimeApprovalData } = useQuery({
    queryKey: ['runtime-approvals'],
    queryFn: async () => {
      try {
        return await api.runtime.listApprovals()
      } catch {
        return { entries: [], total: 0 }
      }
    },
    refetchInterval: 30_000,
    enabled: !!runtimeStatus?.enabled,
  })
  const queueCount = (queueData?.total ?? 0) + (runtimeStatus?.enabled ? (runtimeApprovalData?.total ?? 0) : 0)

  // Check for version updates (infrequently)
  const { data: versionData } = useQuery({
    queryKey: ['version'],
    queryFn: () => api.version.get(),
    refetchInterval: 3600_000, // 1 hour
    staleTime: 3600_000,
  })

  // Check LLM health (for haiku proxy spend cap exhaustion)
  const { data: llmStatus } = useQuery({
    queryKey: ['llm-status'],
    queryFn: () => api.llm.status(),
  })

  // Billing status (for expired state banner) — only when billing is enabled.
  const billingEnabled = !!features?.billing
  const { data: billingStatus, isLoading: billingLoading } = useQuery({
    queryKey: ['billing-status'],
    queryFn: () => api.billing.status(),
    enabled: billingEnabled,
    refetchInterval: 300_000, // 5 minutes
    staleTime: 60_000,
  })

  // Org membership — if the user is a member of any org, skip the
  // /welcome plan-picker. Invitees joining via the org-invite flow
  // shouldn't have to pick a personal plan; they're already part of
  // an org and the accept handler seeds a free-tier sub anyway. This
  // also avoids a dead-end if the free-tier seed transiently fails.
  // Note: enabled even when the teams feature flag is off — the
  // /api/orgs endpoint works regardless of the gate (it just returns
  // an empty list for users with no memberships), and we always want
  // to know "is this user in any org?" before bouncing them.
  const {
    data: memberships,
    isLoading: membershipsLoading,
    isError: membershipsErrored,
  } = useQuery({
    queryKey: ['orgs'],
    queryFn: () => api.orgs.list(),
    staleTime: 60_000,
  })
  const hasOrg = (memberships?.length ?? 0) > 0

  // Redirect to welcome page only if the user has no billing setup AND
  // isn't already an org member. Wait for both queries before deciding
  // — without the loading guard, the redirect can fire on first paint
  // before memberships have loaded, sending an invited user to the
  // plan picker by mistake. Also bail if the /api/orgs lookup errored:
  // treating a failure as "zero memberships" would route org-only users
  // (who have no personal billing) to the plan picker by mistake.
  if (
    billingEnabled &&
    !billingLoading &&
    !membershipsLoading &&
    !membershipsErrored &&
    billingStatus?.status === 'none' &&
    billingStatus?.plan === 'none' &&
    !hasOrg
  ) {
    return <Navigate to="/welcome" replace />
  }

  return (
    <div className="h-screen w-full overflow-hidden bg-surface-0 flex">
      {/* Mobile header */}
      <div className="fixed top-0 left-0 right-0 z-40 flex items-center gap-3 px-4 py-3 bg-surface-1 border-b border-border-default md:hidden">
        <button
          onClick={() => setSidebarOpen(true)}
          className="text-text-primary p-1 -ml-1"
          aria-label="Open menu"
        >
          <svg className="w-5 h-5" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M4 6h16M4 12h16M4 18h16"/></svg>
        </button>
        <span className="font-bold text-lg tracking-tight text-text-primary flex items-center gap-2">
          <img src="/favicon.svg" alt="" className="w-5 h-5" />
          Clawvisor
        </span>
        {queueCount > 0 && (
          <button
            onClick={() => navigate('/dashboard')}
            className="text-xs font-mono font-medium px-1.5 py-0.5 rounded bg-warning text-surface-0 ml-auto"
          >
            {queueCount > 9 ? '9+' : queueCount}
          </button>
        )}
      </div>

      {/* Sidebar overlay (mobile) */}
      {sidebarOpen && (
        <div
          className="fixed inset-0 z-40 bg-black/40 md:hidden"
          onClick={() => setSidebarOpen(false)}
        />
      )}

      {/* Sidebar */}
      <nav className={`fixed inset-y-0 left-0 z-50 w-56 bg-surface-1 border-r border-border-default flex flex-col shrink-0 transform transition-transform duration-200 ease-in-out md:sticky md:top-0 md:h-screen md:translate-x-0 ${sidebarOpen ? 'translate-x-0' : '-translate-x-full'}`}>
        <div className="px-4 py-5 border-b border-border-default">
          <span className="font-bold text-lg tracking-tight text-text-primary flex items-center gap-2">
            <img src="/favicon.svg" alt="" className="w-5 h-5" />
            Clawvisor
          </span>
        </div>
        <ul className="flex-1 py-2 overflow-y-auto">
          {[
            ...navItems.filter((i) => {
              if (i.to === '/dashboard/how-it-works') return !!features?.proxy_lite
              if (i.to === '/dashboard/get-started') return !features?.proxy_lite
              return true
            }),
            ...(features?.billing ? [billingNavItem] : []),
          ].map(({ to, label, end, icon }) => (
            <li key={to}>
              <NavLink
                to={to}
                end={end}
                className={({ isActive }) =>
                  `flex items-center justify-between px-4 py-2 text-sm font-medium transition-colors border-l-2 ${
                    isActive
                      ? 'bg-brand-muted text-brand border-l-brand'
                      : 'text-text-secondary hover:bg-surface-2 hover:text-text-primary border-l-transparent'
                  }`
                }
              >
                <span className="flex items-center gap-3">
                  {icon}
                  {label}
                </span>
                {label === 'Overview' && queueCount > 0 && (
                  <span className="text-xs font-mono font-medium px-1.5 py-0.5 rounded bg-warning text-surface-0">
                    {queueCount > 9 ? '9+' : queueCount}
                  </span>
                )}
              </NavLink>
            </li>
          ))}
          {features?.teams && (
            <>
              <li className="px-4 pt-4 pb-1">
                <span className="text-xs font-semibold text-text-tertiary uppercase tracking-wider">Organization</span>
              </li>
              {currentOrg && orgNavItems.map(({ to, label, end, icon }) => (
                <li key={to}>
                  <NavLink
                    to={to}
                    end={end}
                    className={({ isActive }) =>
                      `flex items-center gap-3 px-4 py-2 text-sm font-medium transition-colors border-l-2 ${
                        isActive
                          ? 'bg-brand-muted text-brand border-l-brand'
                          : 'text-text-secondary hover:bg-surface-2 hover:text-text-primary border-l-transparent'
                      }`
                    }
                  >
                    {icon}
                    {label}
                  </NavLink>
                </li>
              ))}
              {!currentOrg && (
                <li>
                  <NavLink
                    to="/dashboard/org"
                    className={({ isActive }) =>
                      `flex items-center gap-3 px-4 py-2 text-sm font-medium transition-colors border-l-2 ${
                        isActive
                          ? 'bg-brand-muted text-brand border-l-brand'
                          : 'text-text-secondary hover:bg-surface-2 hover:text-text-primary border-l-transparent'
                      }`
                    }
                  >
                    <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M12 5v14m-7-7h14"/></svg>
                    Create Organization
                  </NavLink>
                </li>
              )}
            </>
          )}
        </ul>
        {features?.teams && (
          <div className="px-3 py-2 border-t border-border-default">
            <OrgSelector />
          </div>
        )}
        <div className="px-4 py-3 border-t border-border-default text-sm space-y-1">
          {versionData?.current && (
            <div className="text-xs text-text-tertiary flex items-center gap-1.5">
              v{versionData.current}
              {versionData.update_available && (
                <span className="inline-block w-2 h-2 rounded-full bg-brand animate-pulse" title={`v${versionData.latest} available`} />
              )}
            </div>
          )}
          <div className="truncate text-text-secondary">{user?.email}</div>
          <div className="flex items-center gap-2">
            <button
              onClick={logout}
              className="text-text-tertiary hover:text-text-primary transition-colors"
            >
              Sign out
            </button>
            <button
              onClick={() => setTheme(resolvedTheme === 'dark' ? 'light' : 'dark')}
              className="ml-auto text-text-tertiary hover:text-text-primary transition-colors p-1 rounded hover:bg-surface-2"
              title={resolvedTheme === 'dark' ? 'Switch to light mode' : 'Switch to dark mode'}
            >
              {resolvedTheme === 'dark' ? (
                <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><circle cx="12" cy="12" r="5"/><path d="M12 1v2M12 21v2M4.22 4.22l1.42 1.42M18.36 18.36l1.42 1.42M1 12h2M21 12h2M4.22 19.78l1.42-1.42M18.36 5.64l1.42-1.42"/></svg>
              ) : (
                <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M21 12.79A9 9 0 1111.21 3 7 7 0 0021 12.79z"/></svg>
              )}
            </button>
          </div>
        </div>
      </nav>

      {/* Main content */}
      <main className="flex-1 min-w-0 h-full overflow-y-auto overflow-x-hidden pt-14 md:pt-0">
        {versionData?.update_available && (
          <div className="mx-4 mt-3 px-4 py-2.5 rounded-md bg-brand-muted border border-brand/30 flex flex-col sm:flex-row sm:items-center sm:justify-between gap-2 text-sm">
            <span className="text-text-primary">
              <span className="font-medium">Clawvisor v{versionData.latest}</span> is available
              {versionData.current && <span className="text-text-secondary"> (current: v{versionData.current})</span>}
            </span>
            <span className="flex items-center gap-3">
              {versionData.auto_update ? (
                <span className="text-text-secondary">
                  Auto-update is enabled — this update will be applied automatically
                </span>
              ) : (
                <span className="text-text-secondary">
                  Run <code className="text-xs bg-surface-2 px-2 py-1 rounded font-mono">clawvisor update</code> to get the latest version
                </span>
              )}
              {versionData.release_url && (
                <a
                  href={versionData.release_url}
                  target="_blank"
                  rel="noopener noreferrer"
                  className="text-brand hover:text-brand/80 font-medium transition-colors"
                >
                  View release
                </a>
              )}
            </span>
          </div>
        )}
        {/* The connect-accounts pitch is noise on org settings pages —
            people there are managing members/teams/SSO, not onboarding. */}
        {!location.pathname.startsWith('/dashboard/org') && <OnboardingBanner />}
        {llmStatus?.spend_cap_exhausted && (
          <div className="mx-4 mt-3 px-4 py-2.5 rounded-md bg-warning/10 border border-warning/30 flex flex-col sm:flex-row sm:items-center sm:justify-between gap-2 text-sm">
            <span className="text-text-primary">
              <span className="font-medium">Free LLM credit exhausted</span>
              <span className="text-text-secondary"> — verification and risk assessment are paused. Add your own API key to restore them.</span>
            </span>
            <NavLink
              to="/dashboard/settings"
              className="text-brand hover:text-brand/80 font-medium transition-colors whitespace-nowrap"
            >
              Configure API key
            </NavLink>
          </div>
        )}
        {billingStatus && !['active', 'past_due', 'none'].includes(billingStatus.status) && billingStatus.plan !== 'none' && (
          <div className="mx-4 mt-3 px-4 py-2.5 rounded-md bg-danger/10 border border-danger/30 flex flex-col sm:flex-row sm:items-center sm:justify-between gap-2 text-sm">
            <span className="text-text-primary">
              <span className="font-medium">Your subscription has expired.</span>
              <span className="text-text-secondary"> Choose a plan to continue using Clawvisor.</span>
            </span>
            <NavLink
              to="/pricing"
              className="text-danger hover:text-danger/80 font-medium transition-colors whitespace-nowrap"
            >
              Choose a plan
            </NavLink>
          </div>
        )}
        {billingStatus?.status === 'past_due' && (
          <div className="mx-4 mt-3 px-4 py-2.5 rounded-md bg-warning/10 border border-warning/30 flex flex-col sm:flex-row sm:items-center sm:justify-between gap-2 text-sm">
            <span className="text-text-primary">
              <span className="font-medium">Payment past due.</span>
              <span className="text-text-secondary"> Please update your payment method to avoid service interruption.</span>
            </span>
            <NavLink
              to="/dashboard/billing"
              className="text-warning hover:text-warning/80 font-medium transition-colors whitespace-nowrap"
            >
              Manage billing
            </NavLink>
          </div>
        )}
        <Routes>
          <Route index element={<Overview />} />
          <Route path="how-it-works" element={<HowItWorks />} />
          <Route path="what-is-clawvisor" element={<Navigate to="/dashboard/how-it-works" replace />} />
          <Route path="get-started" element={<GetStarted />} />
          <Route path="tasks" element={<Tasks />} />
          <Route path="accounts" element={<Services />} />
          <Route path="services" element={<Navigate to="/dashboard/accounts" replace />} />
          <Route path="policy" element={<Policy />} />
          <Route path="restrictions" element={<Navigate to="/dashboard/policy" replace />} />
          {features?.adapter_gen && <Route path="adapter-gen" element={<AdapterGen />} />}
          <Route path="activity" element={<Activity />} />
          <Route path="audit" element={<Navigate to="/dashboard/activity" replace />} />
          <Route path="agents" element={<Agents />} />
          <Route path="agents/:agentId" element={<Agents />} />
          {/* Focused deep-link page for vaulting one upstream LLM API key.
              Linked to by the one-paste install skill when the user's shell
              env doesn't carry $ANTHROPIC_API_KEY / $OPENAI_API_KEY. Not in
              the nav — strictly a deep-link target. */}
          <Route path="keys/:provider" element={<KeyVault />} />
          <Route path="runtime" element={<Navigate to="/dashboard/policy" replace />} />
          <Route path="settings" element={<Settings />} />
          {features?.billing && <Route path="billing" element={<Billing />} />}
          {features?.teams && (
            <>
              <Route path="org" element={<OrgSettings />} />
              <Route path="org/members" element={<OrgMembers />} />
              <Route path="org/teams" element={<OrgTeams />} />
              <Route path="org/governance" element={<Governance />} />
              <Route path="org/notifications" element={<OrgNotifications />} />
              {/* org/sso temporarily admin-managed — see cmd/admin/org_sso.go
                  in the cloud repo. Customers configure SSO through Clawvisor
                  staff during onboarding. The OrgSSO.tsx file is retained so
                  we can re-enable self-serve in a future release without
                  re-implementing the form. */}
              <Route path="org/adapters" element={<OrgAdapters />} />
              <Route path="org/mcp-servers" element={<OrgMCPServers />} />
            </>
          )}
          <Route path="*" element={<Navigate to="/dashboard" replace />} />
        </Routes>
      </main>

    </div>
  )
}
