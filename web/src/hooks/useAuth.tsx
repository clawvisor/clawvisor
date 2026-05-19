import { useState, useEffect, useCallback, createContext, useContext, useRef, type ReactNode } from 'react'
import { APIError, api, setAccessToken, setRefreshCallback, setCurrentOrgId, type User, type FeatureSet, type LoginResult, type RegisterResult, type Org } from '../api/client'

const REFRESH_TOKEN_KEY = 'clawvisor_refresh_token'
const CURRENT_ORG_KEY = 'clawvisor_current_org'
const INITIAL_REFRESH_RETRY_MS = 1_000
const MAX_INITIAL_REFRESH_RETRY_MS = 5_000
const MAX_INITIAL_REFRESH_ATTEMPTS = 5

function safeSetItem(key: string, value: string) {
  try { localStorage.setItem(key, value) } catch { /* quota exceeded — ignore */ }
}

function isRefreshTokenRejected(error: unknown): boolean {
  return error instanceof APIError && (error.status === 400 || error.status === 401 || error.status === 403)
}

function delay(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms))
}

interface AuthContextValue {
  user: User | null
  isLoading: boolean
  isAuthenticated: boolean
  authMode: 'magic_link' | 'password' | 'passkey' | null
  features: FeatureSet | null
  currentOrg: Org | null
  setCurrentOrg: (org: Org | null) => void
  onboardingComplete: boolean | null
  refreshOnboarding: () => Promise<void>
  login: (email: string, password: string) => Promise<LoginResult>
  register: (email: string, password: string) => Promise<RegisterResult>
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
  const [onboardingComplete, setOnboardingComplete] = useState<boolean | null>(null)
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

  const checkOnboarding = useCallback(() => {
    api.auth.onboarding.status()
      .then((s) => setOnboardingComplete(s.onboarding_completed))
      .catch(() => setOnboardingComplete(null))
  }, [])

  const refreshOnboarding = useCallback(async () => {
    try {
      const s = await api.auth.onboarding.status()
      setOnboardingComplete(s.onboarding_completed)
    } catch {
      // ignore
    }
  }, [])

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

    // Fetch auth mode and refresh token in parallel. Features are fetched
    // separately by the user-dependent effect below so we can return a
    // per-user FeatureSet (e.g. plan-based gating) once the user is known.
    const configPromise = api.config.public()
      .then((cfg) => setAuthMode(cfg.auth_mode))
      .catch((e) => console.warn('useAuth: failed to fetch config', e)) // default stays null → treated like password mode

    async function restoreSession() {
      let refreshToken = localStorage.getItem(REFRESH_TOKEN_KEY)
      if (!refreshToken) return

      let retryDelay = INITIAL_REFRESH_RETRY_MS
      for (let attempt = 1; attempt <= MAX_INITIAL_REFRESH_ATTEMPTS; attempt++) {
        try {
          const resp = await api.auth.refresh(refreshToken)
          setAccessToken(resp.access_token)
          safeSetItem(REFRESH_TOKEN_KEY, resp.refresh_token)
          setUser(resp.user)
          // Check onboarding status now that we have a valid token.
          checkOnboarding()
          return
        } catch (e) {
          if (isRefreshTokenRejected(e)) {
            console.warn('useAuth: token refresh rejected', e)
            localStorage.removeItem(REFRESH_TOKEN_KEY)
            setAccessToken(null)
            return
          }

          if (attempt === MAX_INITIAL_REFRESH_ATTEMPTS) {
            console.warn('useAuth: token refresh temporarily failed; giving up initial restore', e)
            setAccessToken(null)
            return
          }

          console.warn('useAuth: token refresh temporarily failed; retrying', e)
          await delay(retryDelay)
          retryDelay = Math.min(retryDelay * 2, MAX_INITIAL_REFRESH_RETRY_MS)

          refreshToken = localStorage.getItem(REFRESH_TOKEN_KEY)
          if (!refreshToken) {
            setAccessToken(null)
            return
          }
        }
      }
    }

    const authPromise = restoreSession()

    Promise.all([configPromise, authPromise]).finally(() => setIsLoading(false))
  }, [checkOnboarding])

  // Fetch features on mount and whenever the authenticated user changes, so
  // the server can return per-user feature gates (e.g. plan-based gating).
  // The cancel flag drops stale responses if `user` changes again before the
  // in-flight request resolves — without it a slow anonymous request could
  // clobber a fast per-user response on login.
  useEffect(() => {
    let cancelled = false
    api.features.get()
      .then((f) => { if (!cancelled) setFeatures(f) })
      .catch((e) => console.warn('useAuth: failed to fetch features', e))
    return () => { cancelled = true }
  }, [user])

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
        if (isRefreshTokenRejected(e)) {
          // The server definitively rejected this refresh token. Clear auth so
          // RequireAuth redirects to /login.
          setAccessToken(null)
          localStorage.removeItem(REFRESH_TOKEN_KEY)
          setUser(null)
        }
        throw e
      }
    })
    return () => setRefreshCallback(null)
  }, [])

  const setSession = useCallback((at: string, rt: string, u: User) => {
    setAccessToken(at)
    safeSetItem(REFRESH_TOKEN_KEY, rt)
    setUser(u)
    checkOnboarding()
  }, [checkOnboarding])

  const login = useCallback(async (email: string, password: string): Promise<LoginResult> => {
    const resp = await api.auth.login(email, password)
    // Only set session if we got full tokens back (not TOTP/setup redirect)
    if (resp.access_token && resp.refresh_token && resp.user) {
      setAccessToken(resp.access_token)
      safeSetItem(REFRESH_TOKEN_KEY, resp.refresh_token)
      setUser(resp.user)
      checkOnboarding()
    }
    return resp
  }, [checkOnboarding])

  const register = useCallback(async (email: string, password: string): Promise<RegisterResult> => {
    return api.auth.register(email, password)
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
    setOnboardingComplete(null)
  }, [])

  return (
    <AuthContext.Provider value={{ user, isLoading, isAuthenticated: user !== null, authMode, features, currentOrg, setCurrentOrg, onboardingComplete, refreshOnboarding, login, register, logout, setSession }}>
      {children}
    </AuthContext.Provider>
  )
}

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext)
  if (!ctx) throw new Error('useAuth must be used within AuthProvider')
  return ctx
}
