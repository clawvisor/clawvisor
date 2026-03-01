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

export interface Agent {
  id: string
  user_id: string
  name: string
  created_at: string
  token?: string // only present on creation
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
  verification?: {
    allow: boolean
    param_scope: string
    reason_coherence: string
    explanation: string
    model: string
    latency_ms: number
    cached: boolean
  }
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
}

export interface QueueItem {
  type: 'approval' | 'task'
  id: string
  created_at: string
  expires_at: string | null
  approval?: QueueApproval
  task?: Task
}

// ── API surface ───────────────────────────────────────────────────────────────

export const api = {
  auth: {
    register: (email: string, password: string) =>
      post<AuthResponse>('/api/auth/register', { email, password }),
    login: (email: string, password: string) =>
      post<AuthResponse>('/api/auth/login', { email, password }),
    refresh: (refreshToken: string) =>
      post<AuthResponse>('/api/auth/refresh', { refresh_token: refreshToken }),
    magic: (token: string) =>
      post<AuthResponse>('/api/auth/magic', { token }),
    logout: (refreshToken?: string) =>
      post<void>('/api/auth/logout', { refresh_token: refreshToken }),
    me: () => get<User>('/api/me'),
    updateMe: (currentPassword: string, newPassword: string) =>
      put<User>('/api/me', { current_password: currentPassword, new_password: newPassword }),
    deleteMe: (password: string) =>
      del<void>('/api/me', { password }),
  },
  agents: {
    list: () => get<Agent[]>('/api/agents'),
    create: (name: string) =>
      post<Agent>('/api/agents', { name }),
    delete: (id: string) => del<void>(`/api/agents/${id}`),
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
    public: () => get<{ auth_mode: 'magic_link' | 'password' }>('/api/config/public'),
  },
  queue: {
    list: () => get<{ items: QueueItem[]; total: number }>('/api/queue'),
  },
  overview: {
    get: () => get<OverviewData>('/api/overview'),
  },
  tasks: {
    list: () => get<{ tasks: Task[]; total: number }>('/api/tasks'),
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
