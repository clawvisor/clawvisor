import { Link } from 'react-router-dom'

export default function ConnectedAgentsStrip({
  agents,
  showConnectAnother = true,
}: {
  agents: { id: string; name: string }[]
  showConnectAnother?: boolean
}) {
  if (agents.length === 0) return null

  return (
    <div className="flex flex-wrap items-center gap-2">
      {agents.map(a => (
        <div key={a.id} className="dev-chip">
          <BotIcon />
          <span className="text-text-primary">{a.name}</span>
        </div>
      ))}
      {showConnectAnother && (
        <Link to="/dashboard/agents" className="dev-text-link px-1 py-1.5">
          connect another →
        </Link>
      )}
    </div>
  )
}

function BotIcon() {
  return (
    <svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth="2" viewBox="0 0 24 24">
      <rect x="3" y="11" width="18" height="10" rx="2" />
      <circle cx="12" cy="5" r="2" />
      <path d="M12 7v4M8 16h.01M16 16h.01" />
    </svg>
  )
}
