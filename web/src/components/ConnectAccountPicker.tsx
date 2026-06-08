import { useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { api } from '../api/client'
import { useAuth } from '../hooks/useAuth'
import { AddServiceModal } from '../pages/Services'
import { ServicePickRow } from './ServiceCatalogGrid'

const FEATURED_SERVICES = [
  {
    id: 'google.gmail',
    label: 'Gmail',
    description: 'Read, send, and label emails on your behalf',
    icon: <img src="/logos/google-gmail.svg" alt="Gmail" className="w-5 h-5 object-contain" />,
  },
  {
    id: 'github',
    label: 'GitHub',
    description: 'Open issues, review PRs, push commits',
    icon: <img src="/logos/github.svg" alt="GitHub" className="w-5 h-5 object-contain dark:invert" />,
  },
  {
    id: 'slack',
    label: 'Slack',
    description: 'Post messages and read channel history',
    icon: <img src="/logos/slack.svg" alt="Slack" className="w-5 h-5 object-contain" />,
  },
  {
    id: 'google.calendar',
    label: 'Google Calendar',
    description: 'Create events, check availability, RSVP',
    icon: <img src="/logos/google-calendar.svg" alt="Google Calendar" className="w-5 h-5 object-contain" />,
  },
] as const

export default function ConnectAccountPicker() {
  const [selectedServiceId, setSelectedServiceId] = useState<string | null>(null)
  const qc = useQueryClient()
  const { features } = useAuth()

  const { data } = useQuery({
    queryKey: ['services'],
    queryFn: () => api.services.list(),
  })

  const { data: googleOAuth } = useQuery({
    queryKey: ['google-oauth-status'],
    queryFn: () => api.system.getGoogleOAuth(),
    enabled: !features?.multi_tenant,
  })

  const { data: microsoftOAuth } = useQuery({
    queryKey: ['microsoft-oauth-status'],
    queryFn: () => api.system.getMicrosoftOAuth(),
    enabled: !features?.multi_tenant,
  })

  const allServices = data?.services ?? []
  const activeConnectionCount = allServices.filter(s => s.status === 'activated').length
  const hasGoogleServices = allServices.some(s => s.id.startsWith('google.'))
  const googleOAuthMissing = !features?.multi_tenant && hasGoogleServices && googleOAuth != null && !googleOAuth.configured
  const hasMicrosoftServices = allServices.some(s => s.id.startsWith('microsoft.'))
  const microsoftOAuthMissing = !features?.multi_tenant && hasMicrosoftServices && microsoftOAuth != null && !microsoftOAuth.configured

  return (
    <div className="space-y-4">
      <p className="text-sm text-text-tertiary leading-relaxed">
        Link an API like Gmail, GitHub, Slack, or Linear so your agents have something to act
        on. Credentials stay in Clawvisor&apos;s vault — agents never see them.
      </p>
      <div className="grid grid-cols-1 md:grid-cols-2 gap-3">
        {FEATURED_SERVICES.map(service => (
          <ServicePickRow
            key={service.id}
            label={service.label}
            description={service.description}
            icon={service.icon}
            onClick={() => setSelectedServiceId(service.id)}
          />
        ))}
      </div>
      <Link
        to="/dashboard/accounts"
        className="dev-btn-ghost gap-1.5"
      >
        <GridAppIcon className="w-3.5 h-3.5" />
        {allServices.length > 0
          ? `Browse all ${allServices.length} services`
          : 'Browse all services'}
      </Link>

      {selectedServiceId && (
        <AddServiceModal
          services={allServices}
          focusedServiceId={selectedServiceId}
          onClose={() => setSelectedServiceId(null)}
          onSuccess={() => {
            qc.invalidateQueries({ queryKey: ['services'] })
            setSelectedServiceId(null)
          }}
          googleOAuthMissing={googleOAuthMissing}
          microsoftOAuthMissing={microsoftOAuthMissing}
          activeConnectionCount={activeConnectionCount}
        />
      )}
    </div>
  )
}

function GridAppIcon({ className }: { className?: string }) {
  return (
    <svg className={className} viewBox="0 0 24 24" fill="none" xmlns="http://www.w3.org/2000/svg">
      <path d="M17 14V20M14 17H20M15.6 10H18.4C18.9601 10 19.2401 10 19.454 9.89101C19.6422 9.79513 19.7951 9.64215 19.891 9.45399C20 9.24008 20 8.96005 20 8.4V5.6C20 5.03995 20 4.75992 19.891 4.54601C19.7951 4.35785 19.6422 4.20487 19.454 4.10899C19.2401 4 18.9601 4 18.4 4H15.6C15.0399 4 14.7599 4 14.546 4.10899C14.3578 4.20487 14.2049 4.35785 14.109 4.54601C14 4.75992 14 5.03995 14 5.6V8.4C14 8.96005 14 9.24008 14.109 9.45399C14.2049 9.64215 14.3578 9.79513 14.546 9.89101C14.7599 10 15.0399 10 15.6 10ZM5.6 10H8.4C8.96005 10 9.24008 10 9.45399 9.89101C9.64215 9.79513 9.79513 9.64215 9.89101 9.45399C10 9.24008 10 8.96005 10 8.4V5.6C10 5.03995 10 4.75992 9.89101 4.54601C9.79513 4.35785 9.64215 4.20487 9.45399 4.10899C9.24008 4 8.96005 4 8.4 4H5.6C5.03995 4 4.75992 4 4.54601 4.10899C4.35785 4.20487 4.20487 4.35785 4.10899 4.54601C4 4.75992 4 5.03995 4 5.6V8.4C4 8.96005 4 9.24008 4.10899 9.45399C4.20487 9.64215 4.35785 9.79513 4.54601 9.89101C4.75992 10 5.03995 10 5.6 10ZM5.6 20H8.4C8.96005 20 9.24008 20 9.45399 19.891C9.64215 19.7951 9.79513 19.6422 9.89101 19.454C10 19.2401 10 18.9601 10 18.4V15.6C10 15.0399 10 14.7599 9.89101 14.546C9.79513 14.3578 9.64215 14.2049 9.45399 14.109C9.24008 14 8.96005 14 8.4 14H5.6C5.03995 14 4.75992 14 4.54601 14.109C4.35785 14.2049 4.20487 14.3578 4.10899 14.546C4 14.7599 4 15.0399 4 15.6V18.4C4 18.9601 4 19.2401 4.10899 19.454C4.20487 19.6422 4.35785 19.7951 4.54601 19.891C4.75992 20 5.03995 20 5.6 20Z" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  )
}
