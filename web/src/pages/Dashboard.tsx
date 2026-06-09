import { useState, useEffect, useMemo } from 'react'
import { NavLink, Routes, Route, Navigate, useLocation, useNavigate } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { useAuth } from '../hooks/useAuth'
import { useEventStream } from '../hooks/useEventStream'
import { useTheme } from '../hooks/useTheme'
import { api } from '../api/client'
import Services from './Services'
import Policy from './Restrictions'
import PolicyAccountRules from './PolicyAccountRules'
import Activity from './Audit'
import Agents from './Agents'
import Settings from './Settings'
import DashboardIndex from './DashboardIndex'
import Inbox from './Inbox'
import Home from './Home'
import Tasks from './Tasks'
import { useAttentionItems } from '../hooks/useAttentionItems'
import { useSetupProgress } from '../hooks/useSetupProgress'
import AdapterGen from './AdapterGen'
import OrgSettings from './OrgSettings'
import OrgMembers from './OrgMembers'
import OrgAdapters from './OrgAdapters'
import OrgMCPServers from './OrgMCPServers'
import Billing from './Billing'
import OrgSelector from '../components/OrgSelector'
import { DashboardNavItems, DashboardNavSection, type DashboardNavItem } from '../components/DashboardNav'
import Library from './Library'
import HowItWorks from './HowItWorks'

const iconClass = 'w-3.5 h-3.5 opacity-70'

const quickstartIcon = (
  <svg className={iconClass} fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24">
    <rect x="3" y="3" width="7" height="7" />
    <rect x="14" y="3" width="7" height="7" />
    <rect x="3" y="14" width="7" height="7" />
    <rect x="14" y="14" width="7" height="7" />
  </svg>
)

const agentsNavIcon = <svg className={iconClass} fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M16 21v-2a4 4 0 00-4-4H5a4 4 0 00-4 4v2"/><circle cx="8.5" cy="7" r="4"/><path d="M20 8v6M23 11h-6"/></svg>
const accountsNavIcon = <svg className={iconClass} fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M12 22v-5"/><path d="M9 8V2"/><path d="M15 8V2"/><path d="M18 8v5a4 4 0 0 1-4 4h-4a4 4 0 0 1-4-4V8Z"/></svg>
const policyNavIcon = <svg className={iconClass} fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z"/></svg>

const taskHistoryNavItem: DashboardNavItem = {
  to: '/dashboard/tasks',
  label: 'Task history',
  icon: <svg className={iconClass} fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><circle cx="12" cy="12" r="10"/><path d="M12 6v6l4 2"/></svg>,
}

const settingsNavItem: DashboardNavItem = { to: '/dashboard/settings', label: 'Settings', icon: <svg className={iconClass} fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M10.325 4.317c.426-1.756 2.924-1.756 3.35 0a1.724 1.724 0 002.573 1.066c1.543-.94 3.31.826 2.37 2.37a1.724 1.724 0 001.066 2.573c1.756.426 1.756 2.924 0 3.35a1.724 1.724 0 00-1.066 2.573c.94 1.543-.826 3.31-2.37 2.37a1.724 1.724 0 00-2.573 1.066c-.426 1.756-2.924 1.756-3.35 0a1.724 1.724 0 00-2.573-1.066c-1.543.94-3.31-.826-2.37-2.37a1.724 1.724 0 00-1.066-2.573c-1.756-.426-1.756-2.924 0-3.35a1.724 1.724 0 001.066-2.573c-.94-1.543.826-3.31 2.37-2.37.996.608 2.296.07 2.572-1.065z"/><circle cx="12" cy="12" r="3"/></svg> }

const billingNavItem: DashboardNavItem = { to: '/dashboard/billing', label: 'Billing', icon: <svg className={iconClass} fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><rect x="1" y="4" width="22" height="16" rx="2" ry="2"/><path d="M1 10h22"/></svg> }

const howItWorksNavItem: DashboardNavItem = { to: '/dashboard/library/how-it-works', label: 'How it works', icon: <svg className={iconClass} fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><circle cx="12" cy="12" r="10"/><path d="M12 16v-4M12 8h.01"/></svg> }

const libraryNavItem: DashboardNavItem = { to: '/dashboard/library', label: 'Achievements', end: true, icon: <svg className={iconClass} fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M6 9H4.5a2.5 2.5 0 0 1 0-5H6"/><path d="M18 9h1.5a2.5 2.5 0 0 0 0-5H18"/><path d="M4 22h16"/><path d="M10 14.66V17c0 .55-.47.98-.97 1.21C7.85 18.75 7 20.24 7 22"/><path d="M14 14.66V17c0 .55.47.98.97 1.21C16.15 18.75 17 20.24 17 22"/><path d="M18 2H6v7a6 6 0 0 0 12 0V2Z"/></svg> }

const orgNavItems = [
  { to: '/dashboard/org', label: 'Organization', end: true, icon: <svg className="w-3.5 h-3.5 opacity-70" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M17 21v-2a4 4 0 00-4-4H5a4 4 0 00-4 4v2"/><circle cx="9" cy="7" r="4"/><path d="M23 21v-2a4 4 0 00-3-3.87"/><path d="M16 3.13a4 4 0 010 7.75"/></svg> },
  { to: '/dashboard/org/members', label: 'Members', icon: <svg className="w-3.5 h-3.5 opacity-70" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M16 21v-2a4 4 0 00-4-4H5a4 4 0 00-4 4v2"/><circle cx="8.5" cy="7" r="4"/><path d="M20 8v6M23 11h-6"/></svg> },
  { to: '/dashboard/org/adapters', label: 'Custom Adapters', icon: <svg className="w-3.5 h-3.5 opacity-70" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M14 2H6a2 2 0 00-2 2v16a2 2 0 002 2h12a2 2 0 002-2V8z"/><path d="M14 2v6h6"/></svg> },
  { to: '/dashboard/org/mcp-servers', label: 'MCP Servers', icon: <svg className="w-3.5 h-3.5 opacity-70" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><rect x="2" y="2" width="20" height="8" rx="2" ry="2"/><rect x="2" y="14" width="20" height="8" rx="2" ry="2"/><path d="M6 6h.01M6 18h.01"/></svg> },
]

export default function Dashboard() {
  const { user, logout, features, currentOrg } = useAuth()
  const { resolvedTheme, setTheme } = useTheme()
  const [sidebarOpen, setSidebarOpen] = useState(false)
  const location = useLocation()
  const navigate = useNavigate()
  // Close sidebar on route change (mobile)
  useEffect(() => { setSidebarOpen(false) }, [location.pathname])

  // SSE event stream for instant dashboard updates
  useEventStream()

  const { attentionCount } = useAttentionItems()
  const inboxCount = attentionCount
  const { isComplete: setupComplete, isLoading: setupLoading, steps: setupSteps, agents } = useSetupProgress()

  const configureNav = useMemo<DashboardNavItem[]>(() => {
    const liveCount = agents.length
    return [
      {
        to: '/dashboard/agents',
        label: 'Agents',
        icon: agentsNavIcon,
        statusLabel: liveCount > 0 ? String(liveCount) : undefined,
      },
      { to: '/dashboard/accounts', label: 'Accounts', icon: accountsNavIcon },
      { to: '/dashboard/policy', label: 'Policy', icon: policyNavIcon },
    ]
  }, [agents.length])
  const learnNav: DashboardNavItem[] = [howItWorksNavItem, libraryNavItem]

  const quickstartNav = useMemo<DashboardNavItem[]>(() => {
    const requiredSteps = setupSteps.filter(s => !s.optional)
    const completed = requiredSteps.filter(s => s.complete).length
    const total = requiredSteps.length
    const showProgress = !setupLoading && !setupComplete && total > 0

    return [{
      to: '/dashboard/home',
      label: 'Quickstart',
      end: true,
      icon: quickstartIcon,
      progressLabel: showProgress ? `${completed} / ${total}` : undefined,
    }]
  }, [setupComplete, setupLoading, setupSteps])

  const operateNav: DashboardNavItem[] = [
    {
      to: '/dashboard/activity',
      label: 'Activity',
      inboxBadge: true,
      icon: <svg className={iconClass} fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M12 20h9M16.5 3.5a2.121 2.121 0 013 3L7 19l-4 1 1-4L16.5 3.5z" /></svg>,
    },
    taskHistoryNavItem,
  ]

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

  // Redirect to welcome page if user has no billing setup yet.
  if (billingEnabled && !billingLoading && billingStatus?.status === 'none' && billingStatus?.plan === 'none') {
    return <Navigate to="/welcome" replace />
  }

  return (
    <div className="h-screen w-full overflow-hidden bg-surface-0 flex">
      {/* Mobile header */}
      <div className="fixed top-0 left-0 right-0 z-40 flex items-center gap-3 px-4 h-16 bg-surface-2 border-b border-border-default md:hidden">
        <button
          onClick={() => setSidebarOpen(true)}
          className="text-text-primary p-1 -ml-1"
          aria-label="Open menu"
        >
          <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M4 6h16M4 12h16M4 18h16"/></svg>
        </button>
        <span className="font-sans text-sm font-semibold text-text-primary flex items-center gap-2">
          <img src="/penrose-logo.png" alt="" className="dev-brand-logo" />
          clawvisor
        </span>
        {inboxCount > 0 && (
          <button
            onClick={() => navigate('/dashboard/activity')}
            className="dev-badge--count ml-auto"
          >
            {inboxCount > 9 ? '9+' : inboxCount}
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
      <nav className={`dev-sidebar fixed inset-y-0 left-0 z-50 w-[260px] transform transition-transform duration-200 ease-in-out md:sticky md:top-0 md:h-screen md:translate-x-0 ${sidebarOpen ? 'translate-x-0' : '-translate-x-full'}`}>
        <div className="dev-sidebar-brand">
          <div className="dev-sidebar-logo">
            <img src="/penrose-logo.png" alt="" className="dev-brand-logo" />
            clawvisor
          </div>
        </div>
        <ul className="flex-1 py-2 overflow-y-auto">
          <DashboardNavItems items={quickstartNav} inboxCount={inboxCount} />
          <DashboardNavSection title="Operate" items={operateNav} inboxCount={inboxCount} />
          <DashboardNavSection title="Learn" items={learnNav} inboxCount={inboxCount} />
          <DashboardNavSection title="Configure" items={configureNav} inboxCount={inboxCount} />
          <DashboardNavSection
            title="Admin"
            items={[
              settingsNavItem,
              ...(features?.billing ? [billingNavItem] : []),
            ]}
            inboxCount={inboxCount}
          />
          {features?.teams && (
            <>
              <li className="dev-nav-section">Organization</li>
              {currentOrg && orgNavItems.map(({ to, label, end, icon }) => (
                <li key={to} className="dev-nav-item">
                  <NavLink
                    to={to}
                    end={end}
                    className={({ isActive }) =>
                      isActive ? 'dev-nav-link dev-nav-link--active' : 'dev-nav-link dev-nav-link--idle'
                    }
                  >
                    <span className="flex items-center gap-2.5 min-w-0">
                      {icon}
                      <span className="truncate">{label}</span>
                    </span>
                  </NavLink>
                </li>
              ))}
              {!currentOrg && (
                <li className="dev-nav-item">
                  <NavLink
                    to="/dashboard/org"
                    className={({ isActive }) =>
                      isActive ? 'dev-nav-link dev-nav-link--active' : 'dev-nav-link dev-nav-link--idle'
                    }
                  >
                    <span className="flex items-center gap-2.5">
                      <svg className={iconClass} fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M12 5v14m-7-7h14"/></svg>
                      Create org
                    </span>
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
        <div className="dev-status-bar">
          {versionData?.current && (
            <div className="flex items-center gap-1.5">
              <span className="text-text-tertiary">build</span>
              <span className="text-text-secondary">v{versionData.current}</span>
              {versionData.update_available && (
                <span className="inline-block w-1.5 h-1.5 rounded-full bg-brand animate-pulse" title={`v${versionData.latest} available`} />
              )}
            </div>
          )}
          <div className="truncate text-text-secondary">{user?.email}</div>
          <div className="flex items-center gap-2 pt-0.5">
            <button
              onClick={logout}
              className="text-text-tertiary hover:text-text-primary transition-colors"
            >
              sign out
            </button>
            <button
              onClick={() => setTheme(resolvedTheme === 'dark' ? 'light' : 'dark')}
              className="ml-auto text-text-tertiary hover:text-text-primary transition-colors p-1 rounded-sm hover:bg-surface-2"
              title={resolvedTheme === 'dark' ? 'Switch to light mode' : 'Switch to dark mode'}
            >
              {resolvedTheme === 'dark' ? (
                <svg className="w-3.5 h-3.5" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><circle cx="12" cy="12" r="5"/><path d="M12 1v2M12 21v2M4.22 4.22l1.42 1.42M18.36 18.36l1.42 1.42M1 12h2M21 12h2M4.22 19.78l1.42-1.42M18.36 5.64l1.42-1.42"/></svg>
              ) : (
                <svg className="w-3.5 h-3.5" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M21 12.79A9 9 0 1111.21 3 7 7 0 0021 12.79z"/></svg>
              )}
            </button>
          </div>
        </div>
      </nav>

      {/* Main content */}
      <main className="dev-workspace flex-1 min-w-0 h-full overflow-y-auto overflow-x-hidden pt-16 md:pt-0">
        {versionData?.update_available && (
          <div className="dev-banner--info">
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
                  Run <code className="text-xs dev-inset px-2 py-0.5">clawvisor update</code> to get the latest version
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
        {llmStatus?.spend_cap_exhausted && (
          <div className="dev-banner--warning">
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
          <div className="dev-banner--danger">
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
          <div className="dev-banner--warning">
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
          <Route index element={<DashboardIndex />} />
          <Route path="home" element={<Home />} />
          <Route path="inbox" element={<Inbox />} />
          <Route path="library" element={<Library />} />
          <Route path="library/how-it-works" element={<HowItWorks />} />
          <Route path="quickstart" element={<Navigate to="/dashboard/home" replace />} />
          <Route path="setup" element={<Navigate to="/dashboard/home" replace />} />
          <Route path="how-it-works" element={<Navigate to="/dashboard/library/how-it-works" replace />} />
          <Route path="what-is-clawvisor" element={<Navigate to="/dashboard/library/how-it-works" replace />} />
          <Route path="get-started" element={<Navigate to="/dashboard/home" replace />} />
          <Route path="tasks" element={<Tasks />} />
          <Route path="accounts" element={<Services />} />
          <Route path="services" element={<Navigate to="/dashboard/accounts" replace />} />
          <Route path="policy" element={<Policy />} />
          <Route path="policy/accounts/:serviceKey" element={<PolicyAccountRules />} />
          <Route path="restrictions" element={<Navigate to="/dashboard/policy" replace />} />
          {features?.adapter_gen && <Route path="adapter-gen" element={<AdapterGen />} />}
          <Route path="activity" element={<Activity />} />
          <Route path="audit" element={<Navigate to="/dashboard/activity" replace />} />
          <Route path="agents" element={<Agents />} />
          <Route path="agents/setup/:harness" element={<Agents />} />
          <Route path="agents/:agentId" element={<Agents />} />
          <Route path="runtime" element={<Navigate to="/dashboard/policy" replace />} />
          <Route path="settings" element={<Settings />} />
          {features?.billing && <Route path="billing" element={<Billing />} />}
          {features?.teams && (
            <>
              <Route path="org" element={<OrgSettings />} />
              <Route path="org/members" element={<OrgMembers />} />
              <Route path="org/adapters" element={<OrgAdapters />} />
              <Route path="org/mcp-servers" element={<OrgMCPServers />} />
            </>
          )}
          <Route path="*" element={<Navigate to="/dashboard" replace />} />
        </Routes>
      </main>

      {inboxCount > 0 && (
        <button
          type="button"
          onClick={() => navigate('/dashboard/activity')}
          className="fixed bottom-0 left-0 right-0 z-40 md:hidden flex items-center justify-center gap-2 px-4 py-2.5 bg-surface-1 text-warning text-sm font-mono border-t border-warning/40 safe-area-pb"
        >
          <span>{inboxCount === 1 ? '1 needs attention' : `${inboxCount} need attention`}</span>
          <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M9 5l7 7-7 7"/></svg>
        </button>
      )}

    </div>
  )
}
