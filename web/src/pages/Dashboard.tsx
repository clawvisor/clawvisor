import { useState, useEffect } from 'react'
import { NavLink, Routes, Route, Navigate, useLocation, useNavigate } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { useAuth } from '../hooks/useAuth'
import { useEventStream } from '../hooks/useEventStream'
import { useTheme } from '../hooks/useTheme'
import { api } from '../api/client'
import Services from './Services'
import Restrictions from './Restrictions'
import Audit from './Audit'
import Agents from './Agents'
import Proxies from './Proxies'
import Vault from './Vault'
import Settings from './Settings'
import Overview from './Overview'
import GetStarted from './GetStarted'
import Tasks from './Tasks'
import AdapterGen from './AdapterGen'
import OrgSettings from './OrgSettings'
import OrgMembers from './OrgMembers'
import OrgAdapters from './OrgAdapters'
import OrgMCPServers from './OrgMCPServers'
import Billing from './Billing'
import OrgSelector from '../components/OrgSelector'
import OnboardingBanner from '../components/OnboardingBanner'

const navItems = [
  { to: '/dashboard', label: 'Dashboard', end: true, icon: <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><rect x="3" y="3" width="7" height="7"/><rect x="14" y="3" width="7" height="7"/><rect x="3" y="14" width="7" height="7"/><rect x="14" y="14" width="7" height="7"/></svg> },
  { to: '/dashboard/get-started', label: 'Get Started', icon: <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><circle cx="12" cy="12" r="10"/><path d="M9.09 9a3 3 0 015.83 1c0 2-3 3-3 3"/><path d="M12 17h.01"/></svg> },
  { to: '/dashboard/tasks', label: 'Tasks', icon: <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><circle cx="12" cy="12" r="10"/><path d="M12 6v6l4 2"/></svg> },
  { to: '/dashboard/services', label: 'Services', icon: <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M4 6h16M4 12h16M4 18h16"/></svg> },
  { to: '/dashboard/restrictions', label: 'Restrictions', icon: <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z"/></svg> },
  { to: '/dashboard/agents', label: 'Agents', icon: <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M16 21v-2a4 4 0 00-4-4H5a4 4 0 00-4 4v2"/><circle cx="8.5" cy="7" r="4"/><path d="M20 8v6M23 11h-6"/></svg> },
  { to: '/dashboard/proxies', label: 'Proxies', icon: <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z"/><path d="M9 12l2 2 4-4"/></svg> },
  { to: '/dashboard/vault', label: 'Vault', icon: <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><rect x="3" y="11" width="18" height="11" rx="2" ry="2"/><path d="M7 11V7a5 5 0 0110 0v4"/></svg> },
{ to: '/dashboard/audit', label: 'Gateway Log', icon: <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M12 20h9M16.5 3.5a2.121 2.121 0 013 3L7 19l-4 1 1-4L16.5 3.5z"/></svg> },
  { to: '/dashboard/settings', label: 'Settings', icon: <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M10.325 4.317c.426-1.756 2.924-1.756 3.35 0a1.724 1.724 0 002.573 1.066c1.543-.94 3.31.826 2.37 2.37a1.724 1.724 0 001.066 2.573c1.756.426 1.756 2.924 0 3.35a1.724 1.724 0 00-1.066 2.573c.94 1.543-.826 3.31-2.37 2.37a1.724 1.724 0 00-2.573 1.066c-.426 1.756-2.924 1.756-3.35 0a1.724 1.724 0 00-2.573-1.066c-1.543.94-3.31-.826-2.37-2.37a1.724 1.724 0 00-1.066-2.573c-1.756-.426-1.756-2.924 0-3.35a1.724 1.724 0 001.066-2.573c-.94-1.543.826-3.31 2.37-2.37.996.608 2.296.07 2.572-1.065z"/><circle cx="12" cy="12" r="3"/></svg> },
]

const billingNavItem = { to: '/dashboard/billing', label: 'Billing', end: undefined as boolean | undefined, icon: <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><rect x="1" y="4" width="22" height="16" rx="2" ry="2"/><path d="M1 10h22"/></svg> }

const orgNavItems = [
  { to: '/dashboard/org', label: 'Organization', end: true, icon: <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M17 21v-2a4 4 0 00-4-4H5a4 4 0 00-4 4v2"/><circle cx="9" cy="7" r="4"/><path d="M23 21v-2a4 4 0 00-3-3.87"/><path d="M16 3.13a4 4 0 010 7.75"/></svg> },
  { to: '/dashboard/org/members', label: 'Members', icon: <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M16 21v-2a4 4 0 00-4-4H5a4 4 0 00-4 4v2"/><circle cx="8.5" cy="7" r="4"/><path d="M20 8v6M23 11h-6"/></svg> },
  { to: '/dashboard/org/adapters', label: 'Custom Adapters', icon: <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><path d="M14 2H6a2 2 0 00-2 2v16a2 2 0 002 2h12a2 2 0 002-2V8z"/><path d="M14 2v6h6"/></svg> },
  { to: '/dashboard/org/mcp-servers', label: 'MCP Servers', icon: <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24"><rect x="2" y="2" width="20" height="8" rx="2" ry="2"/><rect x="2" y="14" width="20" height="8" rx="2" ry="2"/><path d="M6 6h.01M6 18h.01"/></svg> },
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

  // Queue count for sidebar badge (SSE pushes invalidations)
  const { data: queueData } = useQuery({
    queryKey: ['queue'],
    queryFn: () => api.queue.list(),
  })
  const queueCount = queueData?.total ?? 0

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
    <div className="min-h-screen bg-surface-0 flex">
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
          {[...navItems, ...(features?.billing ? [billingNavItem] : [])].map(({ to, label, end, icon }) => (
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
                {label === 'Dashboard' && queueCount > 0 && (
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
      <main className="flex-1 min-w-0 overflow-auto pt-14 md:pt-0">
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
        <OnboardingBanner />
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
          <Route path="get-started" element={<GetStarted />} />
          <Route path="tasks" element={<Tasks />} />
          <Route path="services" element={<Services />} />
          <Route path="restrictions" element={<Restrictions />} />
          {features?.adapter_gen && <Route path="adapter-gen" element={<AdapterGen />} />}
          <Route path="audit" element={<Audit />} />
          <Route path="agents" element={<Agents />} />
          <Route path="proxies/*" element={<Proxies />} />
          <Route path="vault" element={<Vault />} />
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

    </div>
  )
}
