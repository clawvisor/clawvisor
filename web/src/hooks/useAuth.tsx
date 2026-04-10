import { useState, useEffect, useCallback, createContext, useContext, useRef, type ReactNode } from 'react'
import { api, setAccessToken, setRefreshCallback, setCurrentOrgId, type User, type FeatureSet, type LoginResult, type RegisterResult, type Org } from '../api/client'

const REFRESH_TOKEN_KEY = 'clawvisor_refresh_token'
const CURRENT_ORG_KEY = 'clawvisor_current_org'

function safeSetItem(key: string, value: string) {
  try { localStorage.setItem(key, value) } catch { /* quota exceeded — ignore */ }
}

interface AuthContextValue {
  user: User | null
  isLoading: boolean
  isAuthenticated: boolean
  authMode: 'magic_link' | 'password' | 'passkey' | null
  features: FeatureSet | null
  currentOrg: Org | null
  setCurrentOrg: (org: Org | null) => void
  login: (email: string, password: string) => Promise<LoginResult>
  register: (email: string, password?: string) => Promise<RegisterResult>
  logout: () => Promise<void>
  /** Set session tokens directly (used by pages that handle multi-step auth flows) */
  setSession: (accessToken: string, refreshToken: string, user: User) => void
}

const AuthContext = createContext<AuthContextValue | null>(null)

export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<User | null>(null)
  const [isLoading, setIsLoading] = useState(true)
  const [authMode, setAuthMode] = useState<'magic_link' | 'password' | 'passkey' | null>(null)
  const [features, setFeatures] = useState<FeatureSet | null>(null)
  const [currentOrg, setCurrentOrgState] = useState<Org | null>(() => {
    try {
      const stored = localStorage.getItem(CURRENT_ORG_KEY)
      if (stored) {
        const org = JSON.parse(stored) as Org
        setCurrentOrgId(org.id)
        return org
      }
    } catch (e) {
      console.warn('useAuth: failed to parse stored org from localStorage', e)
    }
    return null
  })
  // Prevents React StrictMode's intentional double-invoke from burning the
  // single-use refresh token twice on the initial session restore.
  const didInit = useRef(false)

  const setCurrentOrg = useCallback((org: Org | null) => {
    setCurrentOrgState(org)
    if (org) {
      safeSetItem(CURRENT_ORG_KEY, JSON.stringify(org))
      setCurrentOrgId(org.id)
    } else {
      localStorage.removeItem(CURRENT_ORG_KEY)
      setCurrentOrgId(null)
    }
  }, [])

  // Restore session once on mount.
  useEffect(() => {
    if (didInit.current) return
    didInit.current = true

    // Fetch auth mode, features, and refresh token in parallel.
    const configPromise = api.config.public()
      .then((cfg) => setAuthMode(cfg.auth_mode))
      .catch((e) => console.warn('useAuth: failed to fetch config', e)) // default stays null → treated like password mode

    const featuresPromise = api.features.get()
      .then((f) => setFeatures(f))
      .catch((e) => console.warn('useAuth: failed to fetch features', e)) // default stays null

    const storedRefresh = localStorage.getItem(REFRESH_TOKEN_KEY)
    const authPromise = storedRefresh
      ? api.auth
          .refresh(storedRefresh)
          .then((resp) => {
            setAccessToken(resp.access_token)
            safeSetItem(REFRESH_TOKEN_KEY, resp.refresh_token)
            setUser(resp.user)
          })
          .catch((e) => {
            console.warn('useAuth: token refresh failed', e)
            localStorage.removeItem(REFRESH_TOKEN_KEY)
            setAccessToken(null)
          })
      : Promise.resolve()

    Promise.all([configPromise, featuresPromise, authPromise]).finally(() => setIsLoading(false))
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
        safeSetItem(REFRESH_TOKEN_KEY, resp.refresh_token)
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

  const setSession = useCallback((at: string, rt: string, u: User) => {
    setAccessToken(at)
    safeSetItem(REFRESH_TOKEN_KEY, rt)
    setUser(u)
  }, [])

  const login = useCallback(async (email: string, password: string): Promise<LoginResult> => {
    const resp = await api.auth.login(email, password)
    // Only set session if we got full tokens back (not TOTP/setup redirect)
    if (resp.access_token && resp.refresh_token && resp.user) {
      setAccessToken(resp.access_token)
      safeSetItem(REFRESH_TOKEN_KEY, resp.refresh_token)
      setUser(resp.user)
    }
    return resp
  }, [])

  const register = useCallback(async (email: string, password?: string): Promise<RegisterResult> => {
    const resp = await api.auth.register(email, password)
    // Only set session if we got full tokens back (local mode)
    if (resp.access_token && resp.refresh_token) {
      setAccessToken(resp.access_token)
      safeSetItem(REFRESH_TOKEN_KEY, resp.refresh_token)
      setUser(resp.user ?? null)
    }
    return resp
  }, [])

  const logout = useCallback(async () => {
    const refreshToken = localStorage.getItem(REFRESH_TOKEN_KEY) ?? undefined
    await api.auth.logout(refreshToken).catch((e) => console.warn('useAuth: logout request failed', e))
    setAccessToken(null)
    localStorage.removeItem(REFRESH_TOKEN_KEY)
    localStorage.removeItem(CURRENT_ORG_KEY)
    setCurrentOrgId(null)
    setCurrentOrgState(null)
    setUser(null)
  }, [])

  return (
    <AuthContext.Provider value={{ user, isLoading, isAuthenticated: user !== null, authMode, features, currentOrg, setCurrentOrg, login, register, logout, setSession }}>
      {children}
    </AuthContext.Provider>
  )
}

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext)
  if (!ctx) throw new Error('useAuth must be used within AuthProvider')
  return ctx
}
