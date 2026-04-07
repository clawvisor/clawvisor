// Renders a service icon from the SVG markup provided by the API (icon_svg field).
// Falls back to a letter initial when no icon is available.

interface ServiceIconProps {
  iconSvg?: string
  serviceId: string
  size?: number
  className?: string
}

export function ServiceIcon({ iconSvg, serviceId, size = 24, className = '' }: ServiceIconProps) {
  if (iconSvg) {
    // Inject width/height into the SVG root element.
    const sized = iconSvg.replace(
      /^<svg/,
      `<svg width="${size}" height="${size}"`,
    )
    return <span className={className} dangerouslySetInnerHTML={{ __html: sized }} />
  }
  // Fallback: first letter of the service name.
  const base = serviceId.includes(':') ? serviceId.slice(0, serviceId.indexOf(':')) : serviceId
  return (
    <div
      className={`rounded-md flex items-center justify-center font-semibold ${className}`}
      style={{ width: size, height: size, fontSize: size * 0.4, backgroundColor: '#e5e7eb', color: '#6b7280' }}
    >
      {(base.split('.').pop() ?? base).charAt(0).toUpperCase()}
    </div>
  )
}

// Icon background wrapper for consistent presentation in lists.
export function ServiceIconBadge({ iconSvg, serviceId, size = 36 }: { iconSvg?: string; serviceId: string; size?: number }) {
  return (
    <div
      className="rounded-lg border border-border-subtle flex items-center justify-center shrink-0"
      style={{ width: size + 12, height: size + 12, backgroundColor: '#ffffff' }}
    >
      <ServiceIcon iconSvg={iconSvg} serviceId={serviceId} size={size} />
    </div>
  )
}
