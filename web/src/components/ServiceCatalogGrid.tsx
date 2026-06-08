import type { ReactNode } from 'react'
import type { ServiceInfo } from '../api/client'
import { serviceName, serviceDescription } from '../lib/services'
import { ServiceIcon } from './ServiceIcon'

export type ServiceCatalogEntry = {
  id: string
  label: string
  description: string
  iconSvg?: string
  iconUrl?: string
  isConnected?: boolean
}

export function buildServiceCatalog(services: ServiceInfo[]): ServiceCatalogEntry[] {
  const typeMap = new Map<string, ServiceCatalogEntry>()

  for (const svc of services) {
    if (!(svc.requires_activation ?? true)) continue

    const existing = typeMap.get(svc.id)
    if (existing) {
      if (svc.status === 'activated') existing.isConnected = true
      continue
    }

    typeMap.set(svc.id, {
      id: svc.id,
      label: serviceName(svc.id),
      description: svc.description || serviceDescription(svc.id),
      iconSvg: svc.icon_svg,
      iconUrl: svc.icon_url,
      isConnected: svc.status === 'activated',
    })
  }

  return Array.from(typeMap.values()).sort((a, b) => a.label.localeCompare(b.label))
}

export function ServicePickRow({
  label,
  description,
  icon,
  onClick,
  connected,
}: {
  label: string
  description: string
  icon: ReactNode
  onClick: () => void
  connected?: boolean
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className="dev-pick-row group text-left w-full"
    >
      <div className="dev-pick-icon group-hover:border-brand/30">
        {icon}
      </div>
      <div className="flex-1 min-w-0">
        <p className="dev-pick-title">{label}</p>
        <p className="dev-pick-desc">{description}</p>
      </div>
      {connected && <span className="dev-badge--success shrink-0">connected</span>}
    </button>
  )
}

export function ServiceCatalogGrid({
  entries,
  onSelect,
  emptyMessage = 'No services available.',
}: {
  entries: ServiceCatalogEntry[]
  onSelect: (serviceId: string) => void
  emptyMessage?: string
}) {
  if (entries.length === 0) {
    return <p className="text-sm text-text-tertiary">{emptyMessage}</p>
  }

  return (
    <div className="grid grid-cols-1 md:grid-cols-2 gap-3">
      {entries.map(entry => (
        <ServicePickRow
          key={entry.id}
          label={entry.label}
          description={entry.description}
          connected={entry.isConnected}
          icon={
            <ServiceIcon
              iconSvg={entry.iconSvg}
              iconUrl={entry.iconUrl}
              serviceId={entry.id}
              size={20}
            />
          }
          onClick={() => onSelect(entry.id)}
        />
      ))}
    </div>
  )
}
