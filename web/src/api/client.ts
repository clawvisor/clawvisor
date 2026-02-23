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

export interface AgentRole {
  id: string
  user_id: string
  name: string
  description: string
  created_at: string
}

export interface Agent {
  id: string
  user_id: string
  name: string
  role_id: string | null
  created_at: string
  token?: string // only present on creation
}

export interface ServiceInfo {
  id: string
  oauth: boolean
  actions: string[]
  status: 'activated' | 'not_activated'
  activated_at?: string
}

export interface PolicyRecord {
  id: string
  user_id: string
  slug: string
  name: string
  description: string
  role_id: string | null
  rules_yaml: string
  created_at: string
  updated_at: string
}

export interface ValidationResult {
  valid: boolean
  errors: string[]
  conflicts: PolicyConflict[]
}

export interface PolicyConflict {
  type: string
  message: string
}

export interface PolicyDecision {
  decision: 'execute' | 'approve' | 'block'
  policy_id: string
  rule_id: string
  reason: string
}

export interface EvalRequest {
  service: string
  action: string
  params?: Record<string, unknown>
  role?: string
}

export interface AuditEntry {
  id: string
  user_id: string
  agent_id?: string
  request_id: string
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
  error_msg?: string
}

export interface AuditFilter {
  service?: string
  outcome?: string
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

// ── API surface ───────────────────────────────────────────────────────────────

export const api = {
  auth: {
    register: (email: string, password: string) =>
      post<AuthResponse>('/api/auth/register', { email, password }),
    login: (email: string, password: string) =>
      post<AuthResponse>('/api/auth/login', { email, password }),
    refresh: (refreshToken: string) =>
      post<AuthResponse>('/api/auth/refresh', { refresh_token: refreshToken }),
    logout: (refreshToken?: string) =>
      post<void>('/api/auth/logout', { refresh_token: refreshToken }),
    me: () => get<User>('/api/me'),
    updateMe: (currentPassword: string, newPassword: string) =>
      put<User>('/api/me', { current_password: currentPassword, new_password: newPassword }),
    deleteMe: (password: string) =>
      del<void>('/api/me', { password }),
  },
  roles: {
    list: () => get<AgentRole[]>('/api/roles'),
    create: (name: string, description?: string) =>
      post<AgentRole>('/api/roles', { name, description: description ?? '' }),
    update: (id: string, name: string, description?: string) =>
      put<AgentRole>(`/api/roles/${id}`, { name, description: description ?? '' }),
    delete: (id: string) => del<void>(`/api/roles/${id}`),
  },
  agents: {
    list: () => get<Agent[]>('/api/agents'),
    create: (name: string, roleId?: string) =>
      post<Agent>('/api/agents', { name, role_id: roleId }),
    updateRole: (id: string, roleId: string | null) =>
      patch<Agent>(`/api/agents/${id}`, { role_id: roleId }),
    delete: (id: string) => del<void>(`/api/agents/${id}`),
  },
  services: {
    list: () => get<{ services: ServiceInfo[] }>('/api/services'),
    oauthStartUrl: (serviceID: string, pendingReqId?: string) => {
      const base = `/api/oauth/start?service=${encodeURIComponent(serviceID)}`
      return pendingReqId ? `${base}&pending_request_id=${pendingReqId}` : base
    },
  },
  policies: {
    list: (roleId?: string) =>
      get<PolicyRecord[]>('/api/policies', roleId ? { role: roleId } : undefined),
    get: (id: string) => get<PolicyRecord>(`/api/policies/${id}`),
    create: (yaml: string) => post<PolicyRecord>('/api/policies', { yaml }),
    update: (id: string, yaml: string) => put<PolicyRecord>(`/api/policies/${id}`, { yaml }),
    delete: (id: string) => del<void>(`/api/policies/${id}`),
    validate: (yaml: string) =>
      post<ValidationResult>('/api/policies/validate', { yaml }),
    evaluate: (req: EvalRequest) =>
      post<PolicyDecision>('/api/policies/evaluate', req),
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
  },
}

export { get, post, put, patch, del }
