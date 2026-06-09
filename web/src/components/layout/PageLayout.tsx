import type { ReactNode } from 'react'

/** Standard dashboard page gutter + vertical rhythm (matches Accounts). */
export function DashboardPageShell({
  children,
  className = '',
}: {
  children: ReactNode
  className?: string
}) {
  return <div className={`page-shell ${className}`.trim()}>{children}</div>
}

export function PageHeader({
  title,
  meta,
  description,
  actions,
  className = '',
}: {
  title: string
  meta?: ReactNode
  description?: ReactNode
  actions?: ReactNode
  className?: string
}) {
  return (
    <div className={`flex flex-col sm:flex-row sm:items-center justify-between gap-4 ${className}`.trim()}>
      <div className="min-w-0">
        <h1 className="page-title">{title}</h1>
        {meta && (
          <p className="text-sm text-text-tertiary mt-1 max-w-1/2">{meta}</p>
        )}
        {description && (
          <p className="text-xs text-text-tertiary mt-2 max-w-xl">{description}</p>
        )}
      </div>
      {actions && (
        <div className="flex items-center gap-3 shrink-0">{actions}</div>
      )}
    </div>
  )
}

export default function PageLayout({
  title,
  meta,
  description,
  actions,
  children,
  className = '',
}: {
  title: string
  meta?: ReactNode
  description?: ReactNode
  actions?: ReactNode
  children: ReactNode
  className?: string
}) {
  return (
    <div className={`page-shell ${className}`.trim()}>
      <header className="page-header">
        <div className="min-w-0">
          <h1 className="page-title">{title}</h1>
          {meta && <p className="text-sm text-text-tertiary mt-1 max-w-1/2">{meta}</p>}
          {description && <p className="page-desc">{description}</p>}
        </div>
        {actions && <div className="flex items-center gap-2 shrink-0">{actions}</div>}
      </header>
      {children}
    </div>
  )
}
