import { NavLink, Routes, Route, Navigate } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { useAuth } from '../hooks/useAuth'
import { api } from '../api/client'
import Services from './Services'
import Policies from './Policies'
import PolicyEditor from './PolicyEditor'
import Audit from './Audit'
import Agents from './Agents'
import Settings from './Settings'
import Overview from './Overview'
import ApprovalsPanel from '../components/ApprovalsPanel'

const navItems = [
  { to: '/dashboard', label: 'Overview', end: true },
  { to: '/dashboard/services', label: 'Services' },
  { to: '/dashboard/policies', label: 'Policies' },
  { to: '/dashboard/agents', label: 'Agents' },
  { to: '/dashboard/audit', label: 'Audit Log' },
  { to: '/dashboard/settings', label: 'Settings' },
]

export default function Dashboard() {
  const { user, logout } = useAuth()

  // Poll pending approvals for notification badge
  const { data: approvalsData } = useQuery({
    queryKey: ['approvals'],
    queryFn: () => api.approvals.list(),
    refetchInterval: 15_000,
  })
  const pendingCount = approvalsData?.entries?.length ?? 0

  return (
    <div className="min-h-screen bg-gray-50 flex">
      {/* Sidebar */}
      <nav className="w-56 bg-white border-r flex flex-col shrink-0">
        <div className="px-4 py-5 border-b">
          <span className="font-bold text-lg tracking-tight">Clawvisor</span>
        </div>
        <ul className="flex-1 py-3 space-y-0.5 px-2">
          {navItems.map(({ to, label, end }) => (
            <li key={to}>
              <NavLink
                to={to}
                end={end}
                className={({ isActive }) =>
                  `flex items-center gap-2 px-3 py-2 rounded text-sm font-medium transition-colors ${
                    isActive
                      ? 'bg-blue-50 text-blue-700'
                      : 'text-gray-600 hover:bg-gray-100 hover:text-gray-900'
                  }`
                }
              >
                {label}
                {label === 'Overview' && pendingCount > 0 && (
                  <span className="ml-auto bg-orange-500 text-white text-xs font-bold rounded-full w-5 h-5 flex items-center justify-center">
                    {pendingCount > 9 ? '9+' : pendingCount}
                  </span>
                )}
              </NavLink>
            </li>
          ))}
        </ul>
        <div className="px-4 py-3 border-t text-sm text-gray-500 space-y-1">
          <div className="truncate">{user?.email}</div>
          <button
            onClick={logout}
            className="text-gray-400 hover:text-gray-700 transition-colors"
          >
            Sign out
          </button>
        </div>
      </nav>

      {/* Main content */}
      <main className="flex-1 min-w-0 overflow-auto">
        <Routes>
          <Route index element={<Overview pendingCount={pendingCount} />} />
          <Route path="services" element={<Services />} />
          <Route path="policies" element={<Policies />} />
          <Route path="policies/new" element={<PolicyEditor />} />
          <Route path="policies/:id" element={<PolicyEditor />} />
          <Route path="audit" element={<Audit />} />
          <Route path="agents" element={<Agents />} />
          <Route path="settings" element={<Settings />} />
          <Route path="*" element={<Navigate to="/dashboard" replace />} />
        </Routes>
      </main>

      {/* Floating approvals panel (visible from anywhere) */}
      {pendingCount > 0 && <ApprovalsPanel />}
    </div>
  )
}
