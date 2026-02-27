import { useState, useEffect, useCallback, createContext, useContext, useRef, type ReactNode } from 'react'
import { api, setAccessToken, setRefreshCallback, type User } from '../api/client'

const REFRESH_TOKEN_KEY = 'clawvisor_refresh_token'

interface AuthContextValue {
  user: User | null
  isLoading: boolean
  isAuthenticated: boolean
  authMode: 'magic_link' | 'password' | null
  login: (email: string, password: string) => Promise<void>
  register: (email: string, password: string) => Promise<void>
  logout: () => Promise<void>
}

const AuthContext = createContext<AuthContextValue | null>(null)

export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<User | null>(null)
  const [isLoading, setIsLoading] = useState(true)
  const [authMode, setAuthMode] = useState<'magic_link' | 'password' | null>(null)
  // Prevents React StrictMode's intentional double-invoke from burning the
  // single-use refresh token twice on the initial session restore.
  const didInit = useRef(false)

  // Restore session once on mount.
  useEffect(() => {
    if (didInit.current) return
    didInit.current = true

    // Check for a magic-link token in the URL fragment (e.g. /dashboard#token=...).
    // The backend redirects here after validating the magic link. Fragments are
    // never sent to the server, so the token stays client-side only.
    const hash = window.location.hash
    if (hash.startsWith('#token=')) {
      const fragmentToken = hash.slice('#token='.length)
      // Clear the fragment immediately so it doesn't linger in the URL.
      window.history.replaceState(null, '', window.location.pathname)
      if (fragmentToken) {
        localStorage.setItem(REFRESH_TOKEN_KEY, fragmentToken)
      }
    }

    // Fetch auth mode and refresh token in parallel.
    const configPromise = api.config.public()
      .then((cfg) => setAuthMode(cfg.auth_mode))
      .catch(() => {}) // default stays null → treated like password mode

    const storedRefresh = localStorage.getItem(REFRESH_TOKEN_KEY)
    const authPromise = storedRefresh
      ? api.auth
          .refresh(storedRefresh)
          .then((resp) => {
            setAccessToken(resp.access_token)
            localStorage.setItem(REFRESH_TOKEN_KEY, resp.refresh_token)
            setUser(resp.user)
          })
          .catch(() => {
            localStorage.removeItem(REFRESH_TOKEN_KEY)
            setAccessToken(null)
          })
      : Promise.resolve()

    Promise.all([configPromise, authPromise]).finally(() => setIsLoading(false))
  }, [])

  // Register a refresh callback so the API client can silently handle 401s on
  // data endpoints (expired access token) without logging the user out.
  useEffect(() => {
    setRefreshCallback(async () => {
      const storedRefresh = localStorage.getItem(REFRESH_TOKEN_KEY)
      if (!storedRefresh) throw new Error('no refresh token stored')
      try {
        const resp = await api.auth.refresh(storedRefresh)
        setAccessToken(resp.access_token)
        localStorage.setItem(REFRESH_TOKEN_KEY, resp.refresh_token)
        setUser(resp.user)
        return resp.access_token
      } catch (e) {
        // Refresh failed — clear auth so RequireAuth redirects to /login.
        setAccessToken(null)
        localStorage.removeItem(REFRESH_TOKEN_KEY)
        setUser(null)
        throw e
      }
    })
    return () => setRefreshCallback(null)
  }, [])

  const login = useCallback(async (email: string, password: string) => {
    const resp = await api.auth.login(email, password)
    setAccessToken(resp.access_token)
    localStorage.setItem(REFRESH_TOKEN_KEY, resp.refresh_token)
    setUser(resp.user)
  }, [])

  const register = useCallback(async (email: string, password: string) => {
    const resp = await api.auth.register(email, password)
    setAccessToken(resp.access_token)
    localStorage.setItem(REFRESH_TOKEN_KEY, resp.refresh_token)
    setUser(resp.user)
  }, [])

  const logout = useCallback(async () => {
    const refreshToken = localStorage.getItem(REFRESH_TOKEN_KEY) ?? undefined
    await api.auth.logout(refreshToken).catch(() => {})
    setAccessToken(null)
    localStorage.removeItem(REFRESH_TOKEN_KEY)
    setUser(null)
  }, [])

  return (
    <AuthContext.Provider value={{ user, isLoading, isAuthenticated: user !== null, authMode, login, register, logout }}>
      {children}
    </AuthContext.Provider>
  )
}

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext)
  if (!ctx) throw new Error('useAuth must be used within AuthProvider')
  return ctx
}
