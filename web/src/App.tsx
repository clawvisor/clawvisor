import { Component, type ErrorInfo, type ReactNode } from 'react'
import { Navigate, Route, Routes, useLocation } from 'react-router-dom'
import { useAuth } from './hooks/useAuth'
import Login from './pages/Login'
import SSOLogin from './pages/SSOLogin'
import Register from './pages/Register'
import MagicLink from './pages/MagicLink'
import CheckEmail from './pages/CheckEmail'
import VerifyEmail from './pages/VerifyEmail'
import SetupAuth from './pages/SetupAuth'
import TOTPVerify from './pages/TOTPVerify'
import Dashboard from './pages/Dashboard'
import Pricing from './pages/Pricing'
import Welcome from './pages/Welcome'
import OAuthAuthorize from './pages/OAuthAuthorize'
import OAuthCallback from './pages/OAuthCallback'
import ForgotPassword from './pages/ForgotPassword'
import ResetPassword from './pages/ResetPassword'
import MFAVerify from './pages/MFAVerify'
import SecuritySetup from './pages/SecuritySetup'
import Waitlist from './pages/Waitlist'
import AcceptInvite from './pages/AcceptInvite'
import SSOComplete from './pages/SSOComplete'

class ErrorBoundary extends Component<{ children: ReactNode }, { hasError: boolean }> {
  constructor(props: { children: ReactNode }) {
    super(props)
    this.state = { hasError: false }
  }

  static getDerivedStateFromError() {
    return { hasError: true }
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error('Uncaught render error:', error, info)
  }

  render() {
    if (this.state.hasError) {
      return (
        <div className="min-h-screen flex flex-col items-center justify-center gap-4">
          <h1 className="text-xl font-semibold">Something went wrong</h1>
          <button
            className="px-4 py-2 bg-blue-600 text-white rounded hover:bg-blue-700"
            onClick={() => { this.setState({ hasError: false }); window.location.href = '/' }}
          >
            Reload
          </button>
        </div>
      )
    }
    return this.props.children
  }
}

function RequireAuth({ children }: { children: React.ReactNode }) {
  const { isAuthenticated, isLoading, authMode, onboardingComplete, featuresReady } = useAuth()
  const location = useLocation()
  // Wait for session restore first, then resolve auth. An unauthenticated
  // visitor is redirected to login immediately — before the features gate —
  // so logout and deep-links don't stall on an /api/features request that
  // logged-out users don't need.
  if (isLoading) return <div className="min-h-screen flex items-center justify-center">Loading...</div>
  if (!isAuthenticated) {
    return <Navigate to={authMode === 'magic_link' ? '/magic-link' : '/login'} replace />
  }
  // Authenticated: wait for THIS user's features before rendering the app.
  // Feature-gated route trees (e.g. the org/Teams routes in Dashboard) are
  // absent until features arrive; rendering early lets the catch-all redirect
  // fire on a deep-link/refresh before the flags load.
  if (!featuresReady) return <div className="min-h-screen flex items-center justify-center">Loading...</div>
  if (onboardingComplete === false && location.pathname !== '/onboarding') {
    return <Navigate to="/onboarding" replace />
  }
  return <>{children}</>
}

export default function App() {
  const { isAuthenticated, isLoading, authMode, features } = useAuth()

  const unauthRedirect = authMode === 'magic_link' ? '/magic-link' : '/login'
  const passwordAuth = features?.password_auth ?? false
  const ssoEnabled = features?.sso ?? false

  return (
    <ErrorBoundary>
    <Routes>
      <Route
        path="/"
        element={
          isLoading ? null : isAuthenticated ? (
            <Navigate to="/dashboard" replace />
          ) : (
            <Navigate to={unauthRedirect} replace />
          )
        }
      />
      {passwordAuth && <Route path="/login" element={<Login />} />}
      {passwordAuth && <Route path="/register" element={<Register />} />}
      {passwordAuth && <Route path="/forgot-password" element={<ForgotPassword />} />}
      {passwordAuth && <Route path="/reset-password" element={<ResetPassword />} />}
      <Route path="/magic-link" element={<MagicLink />} />
      <Route path="/check-email" element={<CheckEmail />} />
      <Route path="/verify-email" element={<VerifyEmail />} />
      <Route path="/waitlist" element={<Waitlist />} />
      <Route path="/accept-invite" element={<AcceptInvite />} />
      {ssoEnabled && <Route path="/login/sso" element={<SSOLogin />} />}
      <Route path="/sso/complete" element={<SSOComplete />} />
      <Route path="/pricing" element={<Pricing />} />
      <Route
        path="/welcome"
        element={
          <RequireAuth>
            <Welcome />
          </RequireAuth>
        }
      />
      <Route path="/setup-auth" element={<SetupAuth />} />
      <Route path="/totp-verify" element={<TOTPVerify />} />
      <Route path="/login/oauth/callback" element={<OAuthCallback />} />
      <Route path="/mfa-verify" element={<MFAVerify />} />
      <Route
        path="/onboarding"
        element={
          <RequireAuth>
            <SecuritySetup />
          </RequireAuth>
        }
      />
      <Route
        path="/oauth/authorize"
        element={
          <RequireAuth>
            <OAuthAuthorize />
          </RequireAuth>
        }
      />
      <Route
        path="/dashboard/*"
        element={
          <RequireAuth>
            <Dashboard />
          </RequireAuth>
        }
      />
    </Routes>
    </ErrorBoundary>
  )
}
