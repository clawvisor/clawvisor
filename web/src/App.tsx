import { Component, type ErrorInfo, type ReactNode } from 'react'
import { Navigate, Route, Routes } from 'react-router-dom'
import { useAuth } from './hooks/useAuth'
import Login from './pages/Login'
import Register from './pages/Register'
import MagicLink from './pages/MagicLink'
import CheckEmail from './pages/CheckEmail'
import VerifyEmail from './pages/VerifyEmail'
import SetupAuth from './pages/SetupAuth'
import TOTPVerify from './pages/TOTPVerify'
import Dashboard from './pages/Dashboard'
import OAuthAuthorize from './pages/OAuthAuthorize'

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
  const { isAuthenticated, isLoading, authMode } = useAuth()
  if (isLoading) return <div className="min-h-screen flex items-center justify-center">Loading...</div>
  if (!isAuthenticated) {
    return <Navigate to={authMode === 'magic_link' ? '/magic-link' : '/login'} replace />
  }
  return <>{children}</>
}

export default function App() {
  const { isAuthenticated, isLoading, authMode, features } = useAuth()

  const unauthRedirect = authMode === 'magic_link' ? '/magic-link' : '/login'
  const passwordAuth = features?.password_auth ?? false

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
      <Route path="/magic-link" element={<MagicLink />} />
      <Route path="/check-email" element={<CheckEmail />} />
      <Route path="/verify-email" element={<VerifyEmail />} />
      <Route path="/setup-auth" element={<SetupAuth />} />
      <Route path="/totp-verify" element={<TOTPVerify />} />
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
