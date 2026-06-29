// Pending org-invite token bridge.
//
// When an invited user clicks the email link they land on /accept-invite. If
// they aren't signed in yet, the landing page stashes the token here and
// sends them through /login or /register. Each step in the auth chain (login,
// register-verify-email, onboarding) checks for a pending token and, when
// found, returns the user to /accept-invite to consume it.
//
// localStorage (not sessionStorage) because the email verification link
// typically opens a fresh tab, which would drop sessionStorage. A stale
// pending token is safe: accept enforces email match and burns single-use
// invites, so an unintended attempt is at worst a 4xx with no side effect.
// We TTL after 30 minutes to avoid accumulating dead tokens forever.
const KEY = 'clawvisor_pending_invite'
const TTL_MS = 30 * 60 * 1000

type Stored = { token: string; ts: number }

export function setPendingInviteToken(token: string) {
  try {
    const payload: Stored = { token, ts: Date.now() }
    localStorage.setItem(KEY, JSON.stringify(payload))
  } catch {
    // Quota / private mode — the AcceptInvite page also reads the token
    // from its own ?token= URL param so this just degrades the redirect.
  }
}

export function takePendingInviteToken(): string | null {
  try {
    const raw = localStorage.getItem(KEY)
    if (!raw) return null
    localStorage.removeItem(KEY)
    const parsed = JSON.parse(raw) as Stored
    if (!parsed || typeof parsed.token !== 'string') return null
    if (Date.now() - parsed.ts > TTL_MS) return null
    return parsed.token
  } catch {
    return null
  }
}

// Read the pending invite token without consuming it. Used by auth-chain
// redirect logic (Login, SecuritySetup, MFAVerify, SetupAuth) to compute
// where to send the user without burning the token: StrictMode fires effects
// twice in dev, and a consume-on-redirect would pop the token on the first
// run and return /dashboard on the second, racing the navigate calls.
// AcceptInvite reads its token from the URL search param, so the stored
// copy is cleared by clearPendingInviteToken() once we're committed to
// the /accept-invite redirect.
export function peekPendingInviteToken(): string | null {
  try {
    const raw = localStorage.getItem(KEY)
    if (!raw) return null
    const parsed = JSON.parse(raw) as Stored
    if (!parsed || typeof parsed.token !== 'string') return null
    if (Date.now() - parsed.ts > TTL_MS) {
      localStorage.removeItem(KEY)
      return null
    }
    return parsed.token
  } catch {
    return null
  }
}

export function clearPendingInviteToken() {
  try {
    localStorage.removeItem(KEY)
  } catch {
    // ignore
  }
}
