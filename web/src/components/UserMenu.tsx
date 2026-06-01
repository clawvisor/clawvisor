import { useRef, useEffect, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { useNavigate } from 'react-router-dom'
import { useAuth } from '../hooks/useAuth'
import { useTheme } from '../hooks/useTheme'
import { api } from '../api/client'

export default function UserMenu() {
  const { user, logout, features } = useAuth()
  const { resolvedTheme, setTheme } = useTheme()
  const navigate = useNavigate()
  const [open, setOpen] = useState(false)
  const menuRef = useRef<HTMLDivElement>(null)
  const triggerRef = useRef<HTMLButtonElement>(null)

  const billingEnabled = !!features?.billing
  const { data: billingStatus } = useQuery({
    queryKey: ['billing-status'],
    queryFn: () => api.billing.status(),
    enabled: billingEnabled,
    staleTime: 60_000,
  })

  const planLabel = billingStatus?.plan_display_name ?? 'Free plan'
  const subStatus = billingStatus?.status ?? 'none'
  const email = user?.email ?? ''
  const initials = email.slice(0, 2).toUpperCase()

  useEffect(() => {
    if (!open) return
    const handler = (e: MouseEvent) => {
      if (
        menuRef.current && !menuRef.current.contains(e.target as Node) &&
        triggerRef.current && !triggerRef.current.contains(e.target as Node)
      ) setOpen(false)
    }
    document.addEventListener('mousedown', handler)
    return () => document.removeEventListener('mousedown', handler)
  }, [open])

  useEffect(() => {
    if (!open) return
    const handler = (e: KeyboardEvent) => {
      if (e.key === 'Escape') { setOpen(false); triggerRef.current?.focus() }
    }
    document.addEventListener('keydown', handler)
    return () => document.removeEventListener('keydown', handler)
  }, [open])

  const planBadgeClass = {
    active: 'bg-success/10 text-success',
    past_due: 'bg-warning/10 text-warning',
    canceled: 'bg-danger/10 text-danger',
    unpaid: 'bg-danger/10 text-danger',
  }[subStatus] ?? 'bg-surface-2 text-text-tertiary'

  return (
    <div className="relative">
      {open && (
        <div
          ref={menuRef}
          role="menu"
          className="absolute bottom-full left-0 right-0 mb-1 z-50 rounded-lg bg-surface-1 border border-border-default shadow-lg overflow-hidden"
        >
          <div className="px-3 py-2.5 border-b border-border-default">
            <p className="text-xs text-text-tertiary truncate">{email}</p>
          </div>

          <ul className="py-1" role="none">
            <SidebarMenuItem
              icon={<SettingsIcon />}
              label="Settings"
              onClick={() => { navigate('/dashboard/settings'); setOpen(false) }}
              external
            />
            <SidebarMenuItem
              icon={<BlogIcon />}
              label="Blog"
              onClick={() => { navigate('/blog'); setOpen(false) }}
              external
            />
            <SidebarMenuItem
              icon={<ServerIcon />}
              label="Self hosted"
              onClick={() => { navigate('/self-hosted'); setOpen(false) }}
              external
            />
          </ul>

          <div className="h-px bg-border-default" />

          <ul className="py-1" role="none">
            <SidebarMenuItem
              icon={<FileTextIcon />}
              label="Terms of service"
              onClick={() => { navigate('terms'); setOpen(false) }}
              external
            />
            <SidebarMenuItem
              icon={<ShieldIcon />}
              label="Privacy policy"
              onClick={() => { navigate('/privacy'); setOpen(false) }}
              external
            />
          </ul>

          <div className="h-px bg-border-default" />

          <ul className="py-1" role="none">
            <SidebarMenuItem
              icon={<LogoutIcon />}
              label="Log out"
              onClick={() => { logout(); setOpen(false) }}
              danger
            />
          </ul>
        </div>
      )}

      <button
        ref={triggerRef}
        onClick={() => setOpen(!open)}
        aria-haspopup="true"
        aria-expanded={open}
        className="w-full flex items-center gap-2.5 px-2 py-2 rounded-lg hover:bg-surface-2 transition-all duration-150 focus:outline-none focus:ring-2 focus:ring-brand/30 group"
      >

        <div className="w-7 h-7 rounded-full bg-brand/20 border border-brand/30 flex items-center justify-center shrink-0 text-[11px] font-semibold text-brand select-none">
          {initials || (
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5" className="w-4 h-4 text-brand" aria-hidden="true">
              <circle cx="12" cy="8" r="4" />
              <path d="M4 20c0-4 3.6-7 8-7s8 3 8 7" strokeLinecap="round" />
            </svg>
          )}
        </div>


        <div className="flex-1 min-w-0 text-left">
          <p className="text-xs font-medium text-text-primary truncate leading-tight">{email}</p>
          <span className={`inline-flex items-center gap-1 text-[10px] font-medium px-1.5 py-px rounded-full mt-0.5 ${planBadgeClass}`}>
            <span className="w-1.5 h-1.5 rounded-full bg-current opacity-70" />
            {planLabel}
          </span>
        </div>

        <span
          role="button"
          tabIndex={0}
          onClick={(e) => {
            e.stopPropagation()
            setTheme(resolvedTheme === 'dark' ? 'light' : 'dark')
          }}
          onKeyDown={(e) => {
            if (e.key === 'Enter' || e.key === ' ') {
              e.stopPropagation()
              setTheme(resolvedTheme === 'dark' ? 'light' : 'dark')
            }
          }}
          className="shrink-0 w-6 h-6 rounded-md flex items-center justify-center text-text-tertiary hover:text-text-primary hover:bg-surface-3 transition-all duration-150"
          title={resolvedTheme === 'dark' ? 'Switch to light' : 'Switch to dark'}
          aria-label={resolvedTheme === 'dark' ? 'Switch to light mode' : 'Switch to dark mode'}
        >
          {resolvedTheme === 'dark' ? <SunIcon /> : <MoonIcon />}
        </span>
      </button>
    </div>
  )
}

// ─── Dropdown menu item ───────────────────────────────────────────────────────

interface SidebarMenuItemProps {
  icon: React.ReactNode
  label: string
  kbd?: string
  onClick: () => void
  danger?: boolean
  external?: boolean
}

function SidebarMenuItem({ icon, label, kbd, onClick, danger, external }: SidebarMenuItemProps) {
  return (
    <li role="none" className="px-1">
      <button
        role="menuitem"
        onClick={onClick}
        className={`sidebar-menu-item group/item w-full flex items-center gap-2.5 px-2.5 py-1.5 text-xs font-medium transition-all duration-150 rounded-md text-left
          ${danger
            ? 'text-danger hover:bg-danger/10'
            : 'text-text-secondary hover:bg-surface-2 hover:text-text-primary'
          }`}
      >
        <span className={`shrink-0 transition-all duration-200 group-hover/item:scale-110 group-hover/item:rotate-6
          ${danger ? 'text-danger' : 'text-text-tertiary group-hover/item:text-text-primary'}`}>
          {icon}
        </span>
        <span className="flex-1">{label}</span>
        {kbd && (
          <kbd className="text-[10px] text-text-tertiary bg-surface-2 border border-border-default rounded px-1.5 py-px font-mono">
            {kbd}
          </kbd>
        )}
        {external && !kbd && (
          <svg className="w-3 h-3 shrink-0 opacity-40 transition-opacity group-hover/item:opacity-80" viewBox="0 0 12 12" fill="none" stroke="currentColor" strokeWidth="1.5" aria-hidden="true">
            <path d="M2.5 9.5l7-7M9.5 2.5H4M9.5 2.5v5.5" strokeLinecap="round" strokeLinejoin="round" />
          </svg>
        )}
      </button>
    </li>
  )
}

// ─── Icons ────────────────────────────────────────────────────────────────────

const ic = {
  width: 14, height: 14, viewBox: '0 0 24 24',
  fill: 'none', stroke: 'currentColor',
  strokeWidth: '1.75', strokeLinecap: 'round' as const, strokeLinejoin: 'round' as const,
  'aria-hidden': true as const,
}
const icSm = { ...ic, width: 13, height: 13 }

const SettingsIcon = () => <svg {...ic}><path d="M10.325 4.317c.426-1.756 2.924-1.756 3.35 0a1.724 1.724 0 002.573 1.066c1.543-.94 3.31.826 2.37 2.37a1.724 1.724 0 001.066 2.573c1.756.426 1.756 2.924 0 3.35a1.724 1.724 0 00-1.066 2.573c.94 1.543-.826 3.31-2.37 2.37a1.724 1.724 0 00-2.573 1.066c-.426 1.756-2.924 1.756-3.35 0a1.724 1.724 0 00-2.573-1.066c-1.543.94-3.31-.826-2.37-2.37a1.724 1.724 0 00-1.066-2.573c-1.756-.426-1.756-2.924 0-3.35a1.724 1.724 0 001.066-2.573c-.94-1.543.826-3.31 2.37-2.37.996.608 2.296.07 2.572-1.065z"/><circle cx="12" cy="12" r="3"/></svg>

const BlogIcon = () => <svg {...ic}><path d="M4 22h16a2 2 0 002-2V4a2 2 0 00-2-2H8a2 2 0 00-2 2v16a2 2 0 01-2 2zm0 0a2 2 0 01-2-2v-9c0-1.1.9-2 2-2h2"/><path d="M18 14h-8M15 18h-5M10 6h8v4h-8z"/></svg>

const ServerIcon = () => <svg {...ic}><rect x="2" y="2" width="20" height="8" rx="2" ry="2"/><rect x="2" y="14" width="20" height="8" rx="2" ry="2"/><path d="M6 6h.01M6 18h.01"/></svg>

const FileTextIcon = () => <svg {...ic}><path d="M14 2H6a2 2 0 00-2 2v16a2 2 0 002 2h12a2 2 0 002-2V8z"/><path d="M14 2v6h6M16 13H8M16 17H8M10 9H8"/></svg>

const ShieldIcon = () => <svg {...ic}><path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z"/><path d="M9 12l2 2 4-4"/></svg>

const LogoutIcon = () => <svg {...ic}><path d="M9 21H5a2 2 0 01-2-2V5a2 2 0 012-2h4M16 17l5-5-5-5M21 12H9"/></svg>

const SunIcon = () => <svg {...icSm}><circle cx="12" cy="12" r="5"/><path d="M12 1v2M12 21v2M4.22 4.22l1.42 1.42M18.36 18.36l1.42 1.42M1 12h2M21 12h2M4.22 19.78l1.42-1.42M18.36 5.64l1.42-1.42"/></svg>

const MoonIcon = () => <svg {...icSm}><path d="M21 12.79A9 9 0 1111.21 3 7 7 0 0021 12.79z"/></svg>