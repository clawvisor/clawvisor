import type { ReactNode } from 'react'
import { NavLink } from 'react-router-dom'

export type DashboardNavItem = {
  to: string
  label: string
  end?: boolean
  icon: ReactNode
  inboxBadge?: boolean
  progressLabel?: string
}

function DashboardNavItemRow({
  to,
  label,
  end,
  icon,
  inboxBadge,
  progressLabel,
  inboxCount,
}: DashboardNavItem & { inboxCount: number }) {
  return (
    <li className="dev-nav-item">
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
        {progressLabel ? (
          <span className="font-mono text-xs text-text-tertiary shrink-0 tabular-nums">
            {progressLabel}
          </span>
        ) : inboxBadge && inboxCount > 0 ? (
          <span className="dev-badge--count shrink-0">
            {inboxCount > 9 ? '9+' : inboxCount}
          </span>
        ) : null}
      </NavLink>
    </li>
  )
}

export function DashboardNavItems({
  items,
  inboxCount,
}: {
  items: DashboardNavItem[]
  inboxCount: number
}) {
  if (items.length === 0) return null
  return (
    <>
      {items.map(item => (
        <DashboardNavItemRow key={item.to} {...item} inboxCount={inboxCount} />
      ))}
    </>
  )
}

export function DashboardNavSection({
  title,
  items,
  inboxCount,
}: {
  title: string
  items: DashboardNavItem[]
  inboxCount: number
}) {
  if (items.length === 0) return null

  return (
    <>
      <li className="dev-nav-section">{title}</li>
      <DashboardNavItems items={items} inboxCount={inboxCount} />
    </>
  )
}
