// Typed API client. All requests go through these helpers.
// Access token is stored in memory (React state); refresh token in localStorage.
// On 401, the caller should trigger a token refresh.

import { populateFromServices } from '../lib/services'

let accessToken: string | null = null
let currentOrgId: string | null = null

export function setAccessToken(token: string | null) {
  accessToken = token
}

export function getAccessToken(): string | null {
  return accessToken
}

export function setCurrentOrgId(orgId: string | null) {
  currentOrgId = orgId
}

export function getCurrentOrgId(): string | null {
  return currentOrgId
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
  if (currentOrgId) {
    headers['X-Org-Id'] = currentOrgId
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
      throw new APIError(retryRes.status, err.error ?? retryRes.statusText, err.code, err)
    }
    if (retryRes.status === 204) return undefined as T
    return retryRes.json()
  }

  if (!res.ok) {
    const err = await res.json().catch(() => ({ error: res.statusText }))
    throw new APIError(res.status, err.error ?? res.statusText, err.code, err)
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
    public readonly extra?: Record<string, unknown>,
  ) {
    super(message)
    this.name = 'APIError'
  }

  get waitlistAvailable(): boolean {
    return this.extra?.waitlist_available === true
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

export interface GoogleAuthResult {
  // Full login (no MFA)
  user?: User
  access_token?: string
  refresh_token?: string
  // MFA required
  status?: 'requires_mfa'
  pending_token?: string
  mfa_methods?: {
    has_totp: boolean
    passkey_count: number
    has_backup_codes: boolean
  }
}

// Login may return one of these instead of a full AuthResponse
export interface LoginResult {
  // Normal login
  user?: User
  access_token?: string
  refresh_token?: string
  // MFA required
  status?: 'requires_mfa'
  pending_token?: string
  mfa_methods?: {
    has_totp: boolean
    passkey_count: number
    has_backup_codes: boolean
  }
}

export interface RegisterResult {
  status: 'verify_email'
}

export type VerifyEmailResult = AuthResponse

export interface WebAuthnCredential {
  id: string
  user_id: string
  name: string
  sign_count: number
  transports: string[]
  created_at: string
}

export interface ResetMethods {
  has_totp: boolean
  has_backup_codes: boolean
  passkey_count: number
}

export interface UserAuthMethods {
  has_password: boolean
  has_totp: boolean
  has_google: boolean
  has_backup_codes: boolean
  passkey_count: number
}

export interface OnboardingStatus {
  tos_accepted: boolean
  has_security_method: boolean
  has_backup_codes: boolean
  onboarding_completed: boolean
}

export interface BackupCodesResponse {
  codes: string[]
}

export interface Agent {
  id: string
  user_id: string
  name: string
  created_at: string
  token?: string // only present on creation
  active_task_count: number
  last_task_at?: string
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

export interface ServiceActionInfo {
  id: string
  display_name: string
  category?: string
  sensitivity?: string
}

export interface ServiceInfo {
  id: string
  name: string
  description: string
  icon_svg?: string
  icon_url?: string
  alias?: string
  oauth: boolean
  device_flow?: boolean
  pkce_flow?: boolean
  pkce_client_id_required?: boolean
  auto_identity?: boolean
  requires_activation?: boolean
  credential_free?: boolean
  actions: ServiceActionInfo[]
  variables?: VariableMeta[]
  status: 'activated' | 'not_activated'
  activated_at?: string
  setup_url?: string
  key_hint?: string
}

export interface VariableMeta {
  name: string
  display_name: string
  description?: string
  required: boolean
  default?: string
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
  config: Record<string, any>
  created_at: string
  updated_at: string
}

export interface PendingGroup {
  chat_id: string
  title: string
  type: string
  detected_at: string
}

export interface TelegramGroup {
  id: string
  user_id: string
  group_chat_id: string
  title: string
  auto_approval_enabled: boolean
  auto_approval_notify: boolean
  created_at: string
  updated_at: string
}

export interface TaskAction {
  service: string
  action: string
  auto_execute: boolean
  expected_use?: string
}

export interface PlannedCall {
  service: string
  action: string
  params?: Record<string, unknown>
  reason: string
}

export interface Task {
  id: string
  user_id: string
  agent_id: string
  purpose: string
  lifetime: 'session' | 'standing'
  status: 'pending_approval' | 'pending_scope_expansion' | 'active' | 'completed' | 'expired' | 'denied' | 'revoked'
  authorized_actions: TaskAction[]
  planned_calls?: PlannedCall[]
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
  approval_source?: string
  approval_rationale?: ApprovalRationale
}

export interface ApprovalRationale {
  explanation: string
  confidence: string
  model: string
  latency_ms: number
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
  adapter_gen: boolean
  billing: boolean
  local_daemon: boolean
  mobile_pairing: boolean
}

export interface VersionInfo {
  current: string
  latest?: string
  update_available: boolean
  release_url?: string
  upgrade_command?: string
  auto_update: boolean
}

export interface LLMUsage {
  spend_cap: number
  total_spent: number
  remaining: number
  pct_used: number
}

export interface LLMStatus {
  status: 'ok' | 'spend_cap_exhausted'
  is_haiku_proxy: boolean
  spend_cap_exhausted: boolean
  provider: string
  model: string
  usage?: LLMUsage
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

export interface WelcomeAction {
  id: string
  display_name: string
  category?: string
  sensitivity?: string
}

export interface WelcomeService {
  id: string
  name: string
  alias?: string
  description?: string
  icon_url?: string
  icon_svg?: string
  actions?: WelcomeAction[]
}

export interface WelcomeAgent {
  id: string
  name: string
}

export interface TaskSuggestion {
  title: string
  prompt: string
  agent?: string
  services: string[]
  risk?: 'low' | 'medium' | 'high'
}

export interface WelcomeData {
  ready: boolean
  services: WelcomeService[]
  agents: WelcomeAgent[]
  suggestions: TaskSuggestion[]
  llm_used: boolean
  llm_status: 'ok' | 'unconfigured' | 'exhausted' | 'error'
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

export interface LocalDaemon {
  id: string
  user_id: string
  daemon_id: string
  name: string
  paired_at: string
  last_connected_at: string | null
  connected: boolean
}

export interface LocalDaemonPairResult {
  daemon_id: string
  name: string
  connection_token: string
}

export interface LocalDaemonServices {
  version: string
  name: string
  services: LocalService[]
}

export interface LocalService {
  id: string
  name: string
  description?: string
  icon?: string
  actions: LocalServiceAction[]
}

export interface LocalServiceAction {
  id: string
  name: string
  description?: string
  params?: LocalServiceParam[]
}

export interface LocalServiceParam {
  name: string
  type: string
  required: boolean
  description?: string
}

export interface EnabledLocalService {
  id: string
  user_id: string
  daemon_id: string
  service_id: string
  enabled_at: string
}

export interface AdapterGenParamPreview {
  name: string
  type: string
  required: boolean
}

export interface AdapterGenActionPreview {
  name: string
  display_name: string
  method?: string
  path?: string
  category: string
  sensitivity: string
  params?: AdapterGenParamPreview[]
}

export interface AdapterGenResult {
  service_id: string
  display_name: string
  description?: string
  base_url: string
  auth_type: string
  yaml: string
  actions: AdapterGenActionPreview[]
  warnings?: string[]
  installed: boolean
}

// ── Billing types ─────────────────────────────────────────────────────────────

export interface BillingPlan {
  name: string
  display_name: string
  monthly_price?: number
  max_connections: number
  included_requests: number
  overage_per_request?: number
  soft_cap_note?: string
  features?: string[]
  contact_us?: boolean
}

export interface BillingStatus {
  plan: string
  plan_display_name?: string
  status: string
  current_period_start?: string
  current_period_end?: string
  cancel_at_period_end?: boolean
  stripe_publishable_key?: string
  usage?: {
    requests: { used: number; limit: number }
    connections: { limit: number }
  }
  discount?: {
    name?: string
    percent_off?: number
    amount_off?: number
    ends_at?: string
  }
}

export interface PromoValidation {
  valid: boolean
  name: string
  percent_off?: number
  amount_off?: number
  duration_months?: number
  duration?: string
}

export interface BillingPlansResponse {
  plans: BillingPlan[]
}

// ── Org types ─────────────────────────────────────────────────────────────────

export interface Org {
  id: string
  name: string
  slug: string
  created_by: string
  created_at: string
  updated_at: string
}

export interface OrgMember {
  id: string
  org_id: string
  user_id: string
  email?: string
  role: 'owner' | 'admin' | 'member'
  joined_at: string
}

export interface OrgInvite {
  id: string
  org_id: string
  email: string
  role: 'admin' | 'member'
  invited_by: string
  expires_at: string
  created_at: string
}

export interface OrgMembership {
  org: Org
  role: 'owner' | 'admin' | 'member'
}

export interface OrgRestriction {
  id: string
  org_id: string
  service: string
  action: string
  reason?: string
  created_by: string
  created_at: string
}

export interface OrgService {
  service_id: string
  name: string
  status: 'active' | 'inactive'
  credential_type: 'shared' | 'per_user' | 'none'
}

export interface CustomAdapter {
  id: string
  service_id: string
  name: string
  auth_type: string
  created_at: string
}

export interface CustomMCPServer {
  id: string
  name: string
  url: string
  auth_type: string
  description?: string
  created_at: string
}

// ── API surface ───────────────────────────────────────────────────────────────

export const api = {
  auth: {
    register: (email: string, password: string) =>
      post<RegisterResult>('/api/auth/register', { email, password }),
    joinWaitlist: (email: string) =>
      post<{ status: 'waitlisted' }>('/api/auth/waitlist', { email }),
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
    devSkipOnboarding: (email: string) =>
      post<AuthResponse>('/api/auth/dev/skip-onboarding', { email }),
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
      verifyBegin: (pendingToken: string) =>
        requestWithToken<{ challenge_id: string; options: any }>('POST', '/api/auth/passkey/verify/begin', pendingToken, {}),
      verifyFinish: (pendingToken: string, challengeId: string, credential: any) =>
        requestWithToken<AuthResponse>('POST', '/api/auth/passkey/verify/finish', pendingToken, { challenge_id: challengeId, credential }),
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
    google: {
      exchange: (code: string, redirectUri: string) =>
        post<GoogleAuthResult>('/api/auth/google', { code, redirect_uri: redirectUri }),
      clientId: () => get<{ client_id: string }>('/api/auth/google/client-id'),
    },
    backupCode: {
      verify: (pendingToken: string, code: string) =>
        requestWithToken<AuthResponse>('POST', '/api/auth/backup-codes/mfa-verify', pendingToken, { code }),
    },
    resetPassword: {
      forgot: (email: string) =>
        post<{ status: string }>('/api/auth/forgot-password', { email }),
      methods: (resetToken: string) =>
        requestWithToken<ResetMethods>('POST', '/api/auth/reset-password/methods', resetToken, {}),
      verifyBackupCode: (resetToken: string, code: string) =>
        requestWithToken<{ reset_token: string }>('POST', '/api/auth/reset-password/verify-backup-code', resetToken, { code }),
      verifyTotp: (resetToken: string, code: string) =>
        requestWithToken<{ reset_token: string }>('POST', '/api/auth/reset-password/verify-totp', resetToken, { code }),
      reset: (verifiedToken: string, password: string) =>
        requestWithToken<AuthResponse>('POST', '/api/auth/reset-password', verifiedToken, { password }),
    },
    onboarding: {
      status: () => get<OnboardingStatus>('/api/auth/onboarding/status'),
      acceptTos: () => post<{ accepted: boolean }>('/api/auth/onboarding/accept-tos', {}),
      generateBackupCodes: () => post<BackupCodesResponse>('/api/auth/onboarding/backup-codes', {}),
      complete: () => post<{ completed: boolean }>('/api/auth/onboarding/complete', {}),
    },
  },
  agents: {
    list: () => get<Agent[]>('/api/agents'),
    create: (name: string) =>
      post<Agent>('/api/agents', { name }),
    delete: (id: string) => del<{ revoked_tasks: number }>(`/api/agents/${id}`),
  },
  connections: {
    list: () => get<ConnectionRequest[]>('/api/agents/connections'),
    approve: (id: string) =>
      post<{ status: string; agent_id: string }>(`/api/agents/connect/${id}/approve`, {}),
    deny: (id: string) =>
      post<{ status: string }>(`/api/agents/connect/${id}/deny`, {}),
  },
  services: {
    list: async () => {
      const result = await get<{ services: ServiceInfo[] }>('/api/services')
      // Populate the display metadata cache from the API response.
      populateFromServices(result.services)
      return result
    },
    // Returns the OAuth consent URL via authenticated fetch (fixes missing-auth-header issue).
    // If the user already has all required scopes, returns {already_authorized: true} instead.
    oauthGetUrl: (serviceID: string, pendingReqId?: string, alias?: string, newAccount?: boolean) =>
      get<{ url?: string; already_authorized?: boolean; service?: string }>('/api/oauth/url', {
        service: serviceID,
        ...(pendingReqId ? { pending_request_id: pendingReqId } : {}),
        ...(alias ? { alias } : {}),
        ...(newAccount ? { new_account: 'true' } : {}),
      }),
    activate: (serviceID: string) =>
      post<{ status: string; service: string }>(`/api/services/${serviceID}/activate`, {}),
    activateWithKey: (serviceID: string, token: string, alias?: string, config?: Record<string, string>) =>
      post<{ status: string; service: string }>(`/api/services/${serviceID}/activate-key`, {
        token,
        ...(alias ? { alias } : {}),
        ...(config && Object.keys(config).length > 0 ? { config } : {}),
      }),
    deactivatePreflight: (serviceID: string, alias?: string) =>
      post<{ service: string; affected_task_count: number }>(`/api/services/${serviceID}/deactivate?dry_run=true`, {
        ...(alias ? { alias } : {}),
      }),
    deactivate: (serviceID: string, alias?: string) =>
      post<{ status: string; service: string }>(`/api/services/${serviceID}/deactivate`, {
        ...(alias ? { alias } : {}),
      }),
    renameAlias: (serviceID: string, oldAlias: string, newAlias: string) =>
      post<{ status: string; service: string; alias: string }>(`/api/services/${serviceID}/rename-alias`, {
        old_alias: oldAlias,
        new_alias: newAlias,
      }),
    pkceFlowStart: (serviceID: string, alias?: string, clientId?: string) =>
      post<{ authorize_url: string; state: string }>(`/api/services/${serviceID}/pkce-flow/start`, {
        ...(alias ? { alias } : {}),
        ...(clientId ? { client_id: clientId } : {}),
      }),
    deviceFlowStart: (serviceID: string, alias?: string) =>
      post<{ flow_id: string; user_code: string; verification_uri: string; interval: number; expires_in: number }>(
        `/api/services/${serviceID}/device-flow/start`, {
          ...(alias ? { alias } : {}),
        }),
    deviceFlowPoll: (serviceID: string, flowId: string) =>
      post<{ status: string; interval?: number; error?: string }>(
        `/api/services/${serviceID}/device-flow/poll`, { flow_id: flowId }),
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
    // Group observation
    upsertTelegramGroup: (groupChatId: string, title?: string) =>
      post<TelegramGroup>('/api/notifications/telegram/group', { group_chat_id: groupChatId, title: title ?? '' }),
    deleteTelegramGroup: (groupChatId: string) =>
      del<void>(`/api/notifications/telegram/groups/active/${groupChatId}`),
    detectTelegramGroups: () =>
      post<PendingGroup[]>('/api/notifications/telegram/groups/detect', {}),
    listTelegramGroups: () =>
      get<PendingGroup[]>('/api/notifications/telegram/groups'),
    dismissTelegramGroup: (chatId: string) =>
      del<void>(`/api/notifications/telegram/groups/${chatId}`),
    // Multi-group management
    addGroupManually: (groupChatId: string) =>
      post<TelegramGroup>('/api/notifications/telegram/groups/manual', { group_chat_id: groupChatId }),
    listActiveGroups: () =>
      get<TelegramGroup[]>('/api/notifications/telegram/groups/active'),
    createGroupPairing: (groupChatId: string) =>
      post<{ session_id: string; pairing_url: string; instruction: string }>(`/api/notifications/telegram/groups/active/${groupChatId}/pair`, {}),
    listPairedAgents: (groupChatId: string) =>
      get<{ id: string; name: string }[]>(`/api/notifications/telegram/groups/active/${groupChatId}/agents`),
    setAutoApproval: (groupChatId: string, enabled: boolean, notify?: boolean) =>
      put<TelegramGroup>(`/api/notifications/telegram/groups/active/${groupChatId}/auto-approval`, { enabled, ...(notify !== undefined && { notify }) }),
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
  system: {
    getGoogleOAuth: () =>
      get<{ configured: boolean }>('/api/system/google-oauth'),
    setGoogleOAuth: (clientId: string, clientSecret: string) =>
      post<{ ok: boolean }>('/api/system/google-oauth', { client_id: clientId, client_secret: clientSecret }),
    listPKCECredentials: () =>
      get<{ service_id: string; client_id: string }[]>('/api/system/pkce-credentials'),
    setPKCECredential: (serviceId: string, clientId: string) =>
      post<{ ok: boolean }>('/api/system/pkce-credentials', { service_id: serviceId, client_id: clientId }),
    deletePKCECredential: (serviceId: string) =>
      del<{ ok: boolean }>(`/api/system/pkce-credentials/${serviceId}`),
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
  welcome: {
    suggestions: () => get<WelcomeData>('/api/welcome/suggestions'),
  },
  devices: {
    list: () => get<PairedDevice[]>('/api/devices'),
    delete: (id: string) => del<void>(`/api/devices/${id}`),
    pairInfo: () => get<PairInfo>('/api/devices/pair/info'),
    startPairing: () => post<PairSession>('/api/devices/pair', {}),
  },
  localDaemon: {
    list: () => get<LocalDaemon[]>('/api/daemon/list'),
    pair: (daemonId: string, code: string, nonce: string, name?: string) =>
      post<LocalDaemonPairResult>('/api/daemon/pair', { daemon_id: daemonId, code, nonce, ...(name ? { name } : {}) }),
    delete: (id: string) => del<void>(`/api/daemon/${id}`),
    services: (id: string) => get<LocalDaemonServices>(`/api/daemon/${id}/services`),
    request: (id: string, service: string, action: string, params?: Record<string, string>) =>
      post<{ success: boolean; data?: unknown; error?: string }>(`/api/daemon/${id}/request`, { service, action, params }),
    rename: (id: string, name: string) =>
      put<LocalDaemon>(`/api/daemon/${id}`, { name }),
    enableService: (daemonId: string, serviceId: string) =>
      post<EnabledLocalService>(`/api/daemon/${daemonId}/services/${serviceId}/enable`, {}),
    disableService: (daemonId: string, serviceId: string) =>
      post<void>(`/api/daemon/${daemonId}/services/${serviceId}/disable`, {}),
    listEnabledServices: () =>
      get<EnabledLocalService[]>('/api/daemon/services/enabled'),
  },
  orgs: {
    list: () => get<OrgMembership[]>('/api/orgs'),
    create: (name: string, slug: string) =>
      post<Org>('/api/orgs', { name, slug }),
    get: (id: string) => get<Org>(`/api/orgs/${id}`),
    update: (id: string, name: string) =>
      put<Org>(`/api/orgs/${id}`, { name }),
    delete: (id: string) => del<void>(`/api/orgs/${id}`),
    members: {
      list: (orgId: string) => get<OrgMember[]>(`/api/orgs/${orgId}/members`),
      add: (orgId: string, userId: string, role: string) =>
        post<OrgMember>(`/api/orgs/${orgId}/members`, { user_id: userId, role }),
      remove: (orgId: string, userId: string) =>
        del<void>(`/api/orgs/${orgId}/members/${userId}`),
      updateRole: (orgId: string, userId: string, role: string) =>
        patch<OrgMember>(`/api/orgs/${orgId}/members/${userId}`, { role }),
    },
    invites: {
      list: (orgId: string) => get<OrgInvite[]>(`/api/orgs/${orgId}/invites`),
      create: (orgId: string, email: string, role: string) =>
        post<OrgInvite>(`/api/orgs/${orgId}/invites`, { email, role }),
      delete: (orgId: string, inviteId: string) =>
        del<void>(`/api/orgs/${orgId}/invites/${inviteId}`),
      accept: (token: string) =>
        post<{ org_id: string; role: string }>('/api/orgs/invites/accept', { token }),
    },
    restrictions: {
      list: (orgId: string) => get<OrgRestriction[]>(`/api/orgs/${orgId}/restrictions`),
      create: (orgId: string, service: string, action: string, reason?: string) =>
        post<OrgRestriction>(`/api/orgs/${orgId}/restrictions`, { service, action, reason }),
      delete: (orgId: string, restrictionId: string) =>
        del<void>(`/api/orgs/${orgId}/restrictions/${restrictionId}`),
    },
    audit: (orgId: string, filter?: AuditFilter) =>
      get<{ entries: AuditEntry[]; total: number }>(`/api/orgs/${orgId}/audit`, filter as Record<string, string | number | undefined>),
    agents: (orgId: string) => get<Agent[]>(`/api/orgs/${orgId}/agents`),
    createAgent: (orgId: string, name: string) =>
      post<{ agent: Agent; token: string }>(`/api/orgs/${orgId}/agents`, { name }),
    deleteAgent: (orgId: string, agentId: string) =>
      del<{ revoked_tasks: number }>(`/api/orgs/${orgId}/agents/${agentId}`),
    revokeTask: (orgId: string, taskId: string) =>
      post<{ status: string }>(`/api/orgs/${orgId}/tasks/${taskId}/revoke`, {}),
    tasks: (orgId: string, params?: { status?: string; limit?: number; offset?: number }) => {
      const q = new URLSearchParams()
      if (params?.status) q.set('status', params.status)
      if (params?.limit) q.set('limit', String(params.limit))
      if (params?.offset) q.set('offset', String(params.offset))
      const qs = q.toString()
      return get<{ tasks: Task[]; total: number }>(`/api/orgs/${orgId}/tasks${qs ? `?${qs}` : ''}`)
    },
    services: (orgId: string) => get<{ services: OrgService[] }>(`/api/orgs/${orgId}/services`),
    adapters: (orgId: string) => get<CustomAdapter[]>(`/api/orgs/${orgId}/adapters`),
    mcpServers: (orgId: string) => get<CustomMCPServer[]>(`/api/orgs/${orgId}/mcp-servers`),
  },
  billing: {
    status: () => get<BillingStatus>('/api/billing/status'),
    plans: () => get<BillingPlansResponse>('/api/billing/plans'),
    checkout: (plan: string, successUrl: string, cancelUrl: string) =>
      post<{ url: string }>('/api/billing/checkout', { plan, success_url: successUrl, cancel_url: cancelUrl }),
    portal: (returnUrl: string) =>
      post<{ url: string }>('/api/billing/portal', { return_url: returnUrl }),
    applyPromo: (code: string) =>
      post<{ status: string }>('/api/billing/promo', { code }),
    validatePromo: (code: string) =>
      post<PromoValidation>('/api/billing/promo/validate', { code }),
    activateFreeTier: () =>
      post<{ status: string }>('/api/billing/activate', {}),
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
  adapterGen: {
    create: (opts: {
      sourceType: string
      source?: string
      sourceUrl?: string
      sourceHeaders?: Record<string, string>
      serviceId?: string
      authType?: string
    }) =>
      post<AdapterGenResult>('/api/adapters/generate', {
        source_type: opts.sourceType,
        ...(opts.sourceUrl ? { source_url: opts.sourceUrl } : { source: opts.source }),
        ...(opts.sourceHeaders && Object.keys(opts.sourceHeaders).length ? { source_headers: opts.sourceHeaders } : {}),
        ...(opts.serviceId ? { service_id: opts.serviceId } : {}),
        ...(opts.authType ? { auth_type: opts.authType } : {}),
      }),
    update: (serviceId: string, opts: {
      sourceType: string
      source?: string
      sourceUrl?: string
      sourceHeaders?: Record<string, string>
    }) =>
      put<AdapterGenResult>(`/api/adapters/${serviceId}/generate`, {
        source_type: opts.sourceType,
        ...(opts.sourceUrl ? { source_url: opts.sourceUrl } : { source: opts.source }),
        ...(opts.sourceHeaders && Object.keys(opts.sourceHeaders).length ? { source_headers: opts.sourceHeaders } : {}),
      }),
    install: (yaml: string) =>
      post<AdapterGenResult>('/api/adapters/install', { yaml }),
    remove: (serviceId: string) =>
      del<{ status: string; service_id: string }>(`/api/adapters/${serviceId}`),
  },
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
