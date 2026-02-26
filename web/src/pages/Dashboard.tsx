import { NavLink, Routes, Route, Navigate } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { useAuth } from '../hooks/useAuth'
import { api } from '../api/client'
import Services from './Services'
import Restrictions from './Restrictions'
import Audit from './Audit'
import Agents from './Agents'
import Settings from './Settings'
import Overview from './Overview'
import Tasks from './Tasks'
import ApprovalsPanel from '../components/ApprovalsPanel'

const navItems = [
  { to: '/dashboard', label: 'Overview', end: true },
  { to: '/dashboard/tasks', label: 'Tasks' },
  { to: '/dashboard/services', label: 'Services' },
  { to: '/dashboard/restrictions', label: 'Restrictions' },
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

  // Poll tasks for badge count
  const { data: tasksData } = useQuery({
    queryKey: ['tasks'],
    queryFn: () => api.tasks.list(),
    refetchInterval: 15_000,
  })
  const actionableTaskCount = (tasksData?.tasks ?? []).filter(
    t => t.status === 'pending_approval' || t.status === 'pending_scope_expansion'
  ).length

  return (
    <div className="min-h-screen bg-gray-50 flex">
      {/* Sidebar */}
      <nav className="w-56 bg-[#0f1117] border-r border-gray-800 flex flex-col shrink-0">
        <div className="px-4 py-5 border-b border-gray-800">
          <span className="font-bold text-lg tracking-tight text-white flex items-center gap-2">
            <img src="/favicon.svg" alt="" className="w-5 h-5" />
            Clawvisor
          </span>
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
                      ? 'bg-white/10 text-white'
                      : 'text-gray-400 hover:bg-white/5 hover:text-gray-200'
                  }`
                }
              >
                {label}
                {label === 'Overview' && pendingCount > 0 && (
                  <span className="ml-auto bg-orange-500 text-white text-xs font-bold rounded-full w-5 h-5 flex items-center justify-center">
                    {pendingCount > 9 ? '9+' : pendingCount}
                  </span>
                )}
                {label === 'Tasks' && actionableTaskCount > 0 && (
                  <span className="ml-auto bg-orange-500 text-white text-xs font-bold rounded-full w-5 h-5 flex items-center justify-center">
                    {actionableTaskCount > 9 ? '9+' : actionableTaskCount}
                  </span>
                )}
              </NavLink>
            </li>
          ))}
        </ul>
        <div className="px-4 py-3 border-t border-gray-800 text-sm space-y-1">
          <div className="truncate text-gray-400">{user?.email}</div>
          <button
            onClick={logout}
            className="text-gray-500 hover:text-gray-300 transition-colors"
          >
            Sign out
          </button>
        </div>
      </nav>

      {/* Main content */}
      <main className="flex-1 min-w-0 overflow-auto">
        <Routes>
          <Route index element={<Overview pendingCount={pendingCount} actionableTaskCount={actionableTaskCount} />} />
          <Route path="tasks" element={<Tasks />} />
          <Route path="services" element={<Services />} />
          <Route path="restrictions" element={<Restrictions />} />
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
