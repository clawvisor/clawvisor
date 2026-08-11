import { Link } from 'react-router'
import PlanGrid from '../components/PlanGrid'
import { useAuth } from '../hooks/useAuth'

export default function Pricing() {
  const { isAuthenticated } = useAuth()

  return (
    <div className="min-h-screen bg-surface-0">
      <div className="max-w-4xl mx-auto px-6 py-16">
        <div className="text-center mb-12">
          <h1 className="text-3xl font-bold text-text-primary mb-3">Choose your plan</h1>
          <p className="text-text-secondary max-w-lg mx-auto">
            Get started for free. Upgrade when you need more.
          </p>
        </div>

        <PlanGrid />

        {/* Only for signed-in users: /dashboard is protected, so showing this
            publicly just bounces a visitor through the login redirect. */}
        {isAuthenticated && (
          <div className="text-center mt-12">
            <Link to="/dashboard" className="text-sm text-text-tertiary hover:text-text-primary transition-colors">
              Back to dashboard
            </Link>
          </div>
        )}
      </div>
    </div>
  )
}
