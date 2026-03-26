// Typed API client. All requests go through these helpers.
// Access token is stored in memory (React state); refresh token in localStorage.
// On 401, the caller should trigger a token refresh.

let accessToken: string | null = null

export function setAccessToken(token: string | null) {
  accessToken = token
}

export function getAccessToken(): string | null {
  return accessToken
}

// ── 401 refresh callback ───────────────────────────────────────────────────────
// AuthProvider registers this so the API client can silently refresh the access
// token when a data endpoint returns 401, without every caller needing to know.

type RefreshFn = () => Promise<string> // resolves to new access token

let _refreshFn: RefreshFn | null = null
let _refreshPromise: Promise<string> | null = null // deduplicates concurrent 401s

export function setRefreshCallback(fn: RefreshFn | null) {
  _refreshFn = fn
}

// All concurrent 401s share one in-flight refresh so the single-use token
// is only consumed once.
function doRefreshOnce(): Promise<string> {
  if (_refreshPromise) return _refreshPromise
  if (!_refreshFn) return Promise.reject(new Error('no refresh callback registered'))
  _refreshPromise = _refreshFn().finally(() => { _refreshPromise = null })
  return _refreshPromise
}

async function request<T>(
  method: string,
  path: string,
  body?: unknown,
  params?: Record<string, string | number | undefined>,
): Promise<T> {
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
  }
  if (accessToken) {
    headers['Authorization'] = `Bearer ${accessToken}`
  }

  let url = path
  if (params) {
    const qs = new URLSearchParams()
    for (const [k, v] of Object.entries(params)) {
      if (v !== undefined && v !== '') qs.set(k, String(v))
    }
    const s = qs.toString()
    if (s) url += '?' + s
  }

  const res = await fetch(url, {
    method,
    headers,
    body: body !== undefined ? JSON.stringify(body) : undefined,
    credentials: 'include',
  })

  // On 401 from a non-auth endpoint, attempt a single silent token refresh and
  // retry the original request. All concurrent 401s share one refresh call.
  if (res.status === 401 && _refreshFn && !path.startsWith('/api/auth/')) {
    const newToken = await doRefreshOnce() // throws if refresh fails → clears auth
    const retryRes = await fetch(url, {
      method,
      headers: { ...headers, Authorization: `Bearer ${newToken}` },
      body: body !== undefined ? JSON.stringify(body) : undefined,
      credentials: 'include',
    })
    if (!retryRes.ok) {
      const err = await retryRes.json().catch(() => ({ error: retryRes.statusText }))
      throw new APIError(retryRes.status, err.error ?? retryRes.statusText, err.code)
    }
    if (retryRes.status === 204) return undefined as T
    return retryRes.json()
  }

  if (!res.ok) {
    const err = await res.json().catch(() => ({ error: res.statusText }))
    throw new APIError(res.status, err.error ?? res.statusText, err.code)
  }

  if (res.status === 204) return undefined as T
  return res.json()
}

const get = <T>(path: string, params?: Record<string, string | number | undefined>) =>
  request<T>('GET', path, undefined, params)
const post = <T>(path: string, body: unknown) => request<T>('POST', path, body)
const put = <T>(path: string, body: unknown) => request<T>('PUT', path, body)
const patch = <T>(path: string, body: unknown) => request<T>('PATCH', path, body)
const del = <T>(path: string, body?: unknown) => request<T>('DELETE', path, body)

// Request with an explicit bearer token (for setup/pending tokens, not the session token)
async function requestWithToken<T>(
  method: string,
  path: string,
  token: string,
  body?: unknown,
): Promise<T> {
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
    'Authorization': `Bearer ${token}`,
  }
  const res = await fetch(path, {
    method,
    headers,
    body: body ? JSON.stringify(body) : undefined,
  })
  if (!res.ok) {
    const data = await res.json().catch(() => ({}))
    throw new APIError(res.status, data.error ?? res.statusText, data.code)
  }
  if (res.status === 204) return undefined as T
  return res.json()
}

// ── Error ─────────────────────────────────────────────────────────────────────

export class APIError extends Error {
  constructor(
    public readonly status: number,
    message: string,
    public readonly code?: string,
  ) {
    super(message)
    this.name = 'APIError'
  }
}

// ── Types ─────────────────────────────────────────────────────────────────────

export interface User {
  id: string
  email: string
  created_at: string
  updated_at: string
}

export interface AuthResponse {
  user: User
  access_token: string
  refresh_token: string
}

// Login may return one of these instead of a full AuthResponse
export interface LoginResult {
  // Normal login
  user?: User
  access_token?: string
  refresh_token?: string
  // TOTP required
  status?: 'requires_totp' | 'setup_required'
  pending_token?: string
  setup_token?: string
}

export interface RegisterResult {
  user?: User
  // Local mode: full tokens
  access_token?: string
  refresh_token?: string
  // Non-local mode without email verification: setup token
  setup_token?: string
  // Non-local mode with email verification: status
  status?: 'verify_email'
}

export interface VerifyEmailResult {
  setup_token: string
  email: string
}

export interface WebAuthnCredential {
  id: string
  user_id: string
  name: string
  sign_count: number
  transports: string[]
  created_at: string
}

export interface UserAuthMethods {
  has_password: boolean
  has_totp: boolean
  passkey_count: number
}

export interface Agent {
  id: string
  user_id: string
  name: string
  created_at: string
  token?: string // only present on creation
}

export interface ConnectionRequest {
  id: string
  user_id: string
  name: string
  description: string
  callback_url?: string
  status: string // pending | approved | denied | expired
  agent_id?: string
  ip_address: string
  created_at: string
  expires_at: string
}

export interface ServiceInfo {
  id: string
  alias?: string
  oauth: boolean
  requires_activation?: boolean
  credential_free?: boolean
  actions: string[]
  status: 'activated' | 'not_activated'
  activated_at?: string
}

export interface Restriction {
  id: string
  user_id: string
  service: string
  action: string
  reason: string
  created_at: string
}

export interface VerificationVerdict {
  allow: boolean
  param_scope: string
  reason_coherence: string
  explanation: string
  model: string
  latency_ms: number
  cached: boolean
}

export interface AuditEntry {
  id: string
  user_id: string
  agent_id?: string
  request_id: string
  task_id?: string
  timestamp: string
  service: string
  action: string
  params_safe: Record<string, unknown>
  decision: string
  outcome: string
  policy_id?: string
  rule_id?: string
  safety_flagged: boolean
  safety_reason?: string
  reason?: string
  data_origin?: string
  context_src?: string
  duration_ms: number
  filters_applied?: unknown
  verification?: VerificationVerdict
  error_msg?: string
}

export interface AuditFilter {
  service?: string
  outcome?: string
  task_id?: string
  limit?: number
  offset?: number
}

export interface PendingApproval {
  id: string
  user_id: string
  request_id: string
  audit_id: string
  request_blob: {
    service: string
    action: string
    params: Record<string, unknown>
    reason?: string
    callback_url?: string
  }
  expires_at: string
  created_at: string
}

export interface NotificationConfig {
  id: string
  user_id: string
  channel: string
  config: Record<string, string>
  created_at: string
  updated_at: string
}

export interface TaskAction {
  service: string
  action: string
  auto_execute: boolean
  expected_use?: string
}

export interface Task {
  id: string
  user_id: string
  agent_id: string
  purpose: string
  lifetime: 'session' | 'standing'
  status: 'pending_approval' | 'pending_scope_expansion' | 'active' | 'completed' | 'expired' | 'denied' | 'revoked'
  authorized_actions: TaskAction[]
  callback_url?: string
  created_at: string
  approved_at?: string
  expires_at?: string
  expires_in_seconds: number
  request_count: number
  pending_action?: TaskAction
  pending_reason?: string
  risk_level?: string
  risk_details?: RiskAssessment
}

export interface RiskAssessment {
  risk_level: string
  explanation: string
  factors: string[]
  conflicts: RiskConflict[]
  model: string
  latency_ms: number
}

export interface RiskConflict {
  field: string
  description: string
  severity: string
}

export interface FeatureSet {
  multi_tenant: boolean
  email_verification: boolean
  passkeys: boolean
  sso: boolean
  teams: boolean
  usage_metering: boolean
  password_auth: boolean
}

export interface VersionInfo {
  current: string
  latest?: string
  update_available: boolean
  release_url?: string
  upgrade_command?: string
}

export interface LLMStatus {
  status: 'ok' | 'spend_cap_exhausted'
  is_haiku_proxy: boolean
  spend_cap_exhausted: boolean
  provider: string
  model: string
}

export interface ActivityBucket {
  bucket: string
  outcome: string
  count: number
}

export interface OverviewData {
  queue: QueueItem[]
  queue_total: number
  active_tasks: Task[]
  activity: ActivityBucket[]
}

export interface QueueApproval {
  request_id: string
  audit_id: string
  service: string
  action: string
  params: Record<string, unknown>
  reason?: string
  verification?: VerificationVerdict
}

export interface QueueItem {
  type: 'approval' | 'task' | 'connection'
  id: string
  created_at: string
  expires_at: string | null
  approval?: QueueApproval
  task?: Task
  connection?: ConnectionRequest
}

export interface PairedDevice {
  id: string
  user_id: string
  device_name: string
  paired_at: string
  last_seen_at: string
}

export interface PairInfo {
  daemon_id: string
  relay_host: string
}

export interface PairSession {
  pairing_token: string
  code: string
  expires_at: string
}

// ── API surface ───────────────────────────────────────────────────────────────

export const api = {
  auth: {
    register: (email: string, password?: string) =>
      post<RegisterResult>('/api/auth/register', { email, password }),
    login: (email: string, password: string) =>
      post<LoginResult>('/api/auth/login', { email, password }),
    refresh: (refreshToken: string) =>
      post<AuthResponse>('/api/auth/refresh', { refresh_token: refreshToken }),
    magic: (token: string) =>
      post<AuthResponse>('/api/auth/magic', { token }),
    verifyEmail: (token: string) =>
      post<VerifyEmailResult>('/api/auth/verify-email', { token }),
    resendVerification: (email: string) =>
      post<{ status: string }>('/api/auth/resend-verification', { email }),
    logout: (refreshToken?: string) =>
      post<void>('/api/auth/logout', { refresh_token: refreshToken }),
    me: () => get<User>('/api/me'),
    updateMe: (currentPassword: string, newPassword: string) =>
      put<User>('/api/me', { current_password: currentPassword, new_password: newPassword }),
    deleteMe: (password: string) =>
      del<void>('/api/me', { password }),
    methods: () => get<UserAuthMethods>('/api/auth/methods'),
    setupPassword: (password: string, setupToken: string) =>
      requestWithToken<AuthResponse>('POST', '/api/auth/setup-password', setupToken, { password }),
    passkey: {
      registerBegin: (setupToken: string) =>
        requestWithToken<{ challenge_id: string; options: any }>('POST', '/api/auth/passkey/register/begin', setupToken, {}),
      registerFinish: (setupToken: string, challengeId: string, credential: any, name?: string) =>
        requestWithToken<AuthResponse>('POST', '/api/auth/passkey/register/finish', setupToken, { challenge_id: challengeId, credential, name }),
      loginBegin: () =>
        post<{ challenge_id: string; options: any }>('/api/auth/passkey/login/begin', {}),
      loginFinish: (challengeId: string, credential: any) =>
        post<AuthResponse>('/api/auth/passkey/login/finish', { challenge_id: challengeId, credential }),
      list: () => get<WebAuthnCredential[]>('/api/auth/passkeys'),
      addBegin: () =>
        post<{ challenge_id: string; options: any }>('/api/auth/passkeys/add/begin', {}),
      addFinish: (challengeId: string, credential: any, name?: string) =>
        post<WebAuthnCredential>('/api/auth/passkeys/add/finish', { challenge_id: challengeId, credential, name }),
      delete: (id: string) => del<void>(`/api/auth/passkeys/${id}`),
      rename: (id: string, name: string) => put<void>(`/api/auth/passkeys/${id}`, { name }),
    },
    totp: {
      setup: () => post<{ secret: string; uri: string; qr_data_url: string }>('/api/auth/totp/setup', {}),
      confirm: (code: string) => post<{ enabled: boolean }>('/api/auth/totp/confirm', { code }),
      verify: (pendingToken: string, code: string) =>
        requestWithToken<AuthResponse>('POST', '/api/auth/totp/verify', pendingToken, { code }),
      status: () => get<{ enabled: boolean }>('/api/auth/totp'),
      disable: (password: string) => del<void>('/api/auth/totp', { password }),
    },
  },
  agents: {
    list: () => get<Agent[]>('/api/agents'),
    create: (name: string) =>
      post<Agent>('/api/agents', { name }),
    delete: (id: string) => del<void>(`/api/agents/${id}`),
  },
  connections: {
    list: () => get<ConnectionRequest[]>('/api/agents/connections'),
    approve: (id: string) =>
      post<{ status: string; agent_id: string }>(`/api/agents/connect/${id}/approve`, {}),
    deny: (id: string) =>
      post<{ status: string }>(`/api/agents/connect/${id}/deny`, {}),
  },
  services: {
    list: () => get<{ services: ServiceInfo[] }>('/api/services'),
    // Returns the OAuth consent URL via authenticated fetch (fixes missing-auth-header issue).
    // If the user already has all required scopes, returns {already_authorized: true} instead.
    oauthGetUrl: (serviceID: string, pendingReqId?: string, alias?: string) =>
      get<{ url?: string; already_authorized?: boolean; service?: string }>('/api/oauth/url', {
        service: serviceID,
        ...(pendingReqId ? { pending_request_id: pendingReqId } : {}),
        ...(alias ? { alias } : {}),
      }),
    activate: (serviceID: string) =>
      post<{ status: string; service: string }>(`/api/services/${serviceID}/activate`, {}),
    activateWithKey: (serviceID: string, token: string, alias?: string) =>
      post<{ status: string; service: string }>(`/api/services/${serviceID}/activate-key`, {
        token,
        ...(alias ? { alias } : {}),
      }),
    deactivate: (serviceID: string, alias?: string) =>
      post<{ status: string; service: string }>(`/api/services/${serviceID}/deactivate`, {
        ...(alias ? { alias } : {}),
      }),
  },
  restrictions: {
    list: () => get<Restriction[]>('/api/restrictions'),
    create: (service: string, action: string, reason?: string) =>
      post<Restriction>('/api/restrictions', { service, action, reason: reason ?? '' }),
    delete: (id: string) => del<void>(`/api/restrictions/${id}`),
  },
  audit: {
    list: (filter?: AuditFilter) =>
      get<{ entries: AuditEntry[]; total: number }>('/api/audit', filter as Record<string, string | number | undefined>),
    get: (id: string) => get<AuditEntry>(`/api/audit/${id}`),
  },
  approvals: {
    list: () => get<{ entries: PendingApproval[]; total: number }>('/api/approvals'),
    approve: (requestId: string) =>
      post<{ status: string; request_id: string; audit_id: string; result?: unknown }>
        (`/api/approvals/${requestId}/approve`, {}),
    deny: (requestId: string) =>
      post<{ status: string; request_id: string; audit_id: string }>
        (`/api/approvals/${requestId}/deny`, {}),
  },
  notifications: {
    list: () => get<NotificationConfig[]>('/api/notifications'),
    upsertTelegram: (botToken: string, chatId: string) =>
      put<NotificationConfig>('/api/notifications/telegram', { bot_token: botToken, chat_id: chatId }),
    deleteTelegram: () => del<void>('/api/notifications/telegram'),
    testTelegram: () => post<{ status: string }>('/api/notifications/telegram/test', {}),
    startPairing: (botToken: string) =>
      post<{ pairing_id: string; bot_username: string; status: string; expires_at: string }>(
        '/api/notifications/telegram/pair', { bot_token: botToken }),
    pairingStatus: (pairingId: string) =>
      get<{ pairing_id: string; bot_username: string; status: string; expires_at: string }>(
        `/api/notifications/telegram/pair/${pairingId}`),
    confirmPairing: (pairingId: string, code: string) =>
      post<NotificationConfig>(
        `/api/notifications/telegram/pair/${pairingId}/confirm`, { code }),
  },
  config: {
    public: () => get<{ auth_mode: 'magic_link' | 'password' | 'passkey' }>('/api/config/public'),
  },
  version: {
    get: () => get<VersionInfo>('/api/version'),
  },
  llm: {
    status: () => get<LLMStatus>('/api/llm/status'),
    update: (provider: string, endpoint: string, apiKey: string, model: string) =>
      put<{ status: string; warning?: string }>('/api/llm', { provider, endpoint, api_key: apiKey, model }),
  },
  features: {
    get: () => get<FeatureSet>('/api/features'),
  },
  queue: {
    list: () => get<{ items: QueueItem[]; total: number }>('/api/queue'),
  },
  overview: {
    get: () => get<OverviewData>('/api/overview'),
  },
  devices: {
    list: () => get<PairedDevice[]>('/api/devices'),
    delete: (id: string) => del<void>(`/api/devices/${id}`),
    pairInfo: () => get<PairInfo>('/api/devices/pair/info'),
    startPairing: () => post<PairSession>('/api/devices/pair', {}),
  },
  oauthApprove: (params: {
    client_id: string
    redirect_uri: string
    state: string
    code_challenge: string
    scope: string
    daemon_id?: string
  }) => post<{ redirect_uri: string }>('/oauth/authorize', params),
  oauthDeny: (params: {
    client_id: string
    redirect_uri: string
    state: string
  }) => post<{ redirect_uri: string }>('/oauth/deny', params),
  tasks: {
    list: (params?: { status?: string; limit?: number; offset?: number }) => {
      const q = new URLSearchParams()
      if (params?.status) q.set('status', params.status)
      if (params?.limit) q.set('limit', String(params.limit))
      if (params?.offset) q.set('offset', String(params.offset))
      const qs = q.toString()
      return get<{ tasks: Task[]; total: number }>(`/api/tasks${qs ? `?${qs}` : ''}`)
    },
    approve: (id: string) =>
      post<{ task_id: string; status: string; expires_at: string }>(`/api/tasks/${id}/approve`, {}),
    deny: (id: string) =>
      post<{ task_id: string; status: string }>(`/api/tasks/${id}/deny`, {}),
    expandApprove: (id: string) =>
      post<{ task_id: string; status: string; expires_at: string }>(`/api/tasks/${id}/expand/approve`, {}),
    expandDeny: (id: string) =>
      post<{ task_id: string; status: string }>(`/api/tasks/${id}/expand/deny`, {}),
    revoke: (id: string) =>
      post<{ task_id: string; status: string }>(`/api/tasks/${id}/revoke`, {}),
  },
}

export { get, post, put, patch, del }
