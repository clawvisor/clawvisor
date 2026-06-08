import { Link } from 'react-router-dom'
import { ServiceIcon } from './ServiceIcon'

export default function ConnectedServicesStrip({
  services,
}: {
  services: {
    id: string
    name: string
    alias?: string
    icon_url?: string
    icon_svg?: string
  }[]
}) {
  if (services.length === 0) return null

  return (
    <div className="flex flex-wrap items-center gap-2">
      {services.map(s => (
        <div key={`${s.id}:${s.alias ?? ''}`} className="dev-chip">
          <div className={s.id === 'github' ? 'dark:invert' : ''}>
            <ServiceIcon iconUrl={s.icon_url} iconSvg={s.icon_svg} serviceId={s.id} size={16} />
          </div>
          <span className="text-text-primary">{s.name}</span>
          {s.alias && <span className="text-text-tertiary">({s.alias})</span>}
        </div>
      ))}
      <Link to="/dashboard/accounts" className="dev-text-link px-1 py-1.5">
        connect another →
      </Link>
    </div>
  )
}
