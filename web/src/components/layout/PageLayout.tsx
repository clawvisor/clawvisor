import type { ReactNode } from 'react'

export default function PageLayout({
  title,
  description,
  actions,
  children,
  className = '',
}: {
  title: string
  description?: string
  actions?: ReactNode
  children: ReactNode
  className?: string
}) {
  return (
    <div className={`page-shell ${className}`.trim()}>
      <header className="page-header">
        <div className="min-w-0">
          <h1 className="page-title">{title}</h1>
          {description && <p className="page-desc">{description}</p>}
        </div>
        {actions && <div className="flex items-center gap-2 shrink-0">{actions}</div>}
      </header>
      {children}
    </div>
  )
}
