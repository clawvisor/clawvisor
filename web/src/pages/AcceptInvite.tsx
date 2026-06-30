import { useEffect, useMemo, useRef, useState } from 'react'
import { Link, useNavigate, useSearchParams } from 'react-router-dom'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { api, APIError } from '../api/client'
import { useAuth } from '../hooks/useAuth'
import { setPendingInviteToken, clearPendingInviteToken } from '../lib/pendingInvite'

// Build the destination URL for the register/login redirect.
// We carry the email and org name in the URL so the auth pages can
// prefill / contextualize the form ("Join Acme Corp" instead of
// "Create your account").
function authUrlFor(path: '/register' | '/login', invite: { email: string; org_name: string }): string {
  const q = new URLSearchParams({
    invite: '1',
    email: invite.email,
    org: invite.org_name,
  })
  return `${path}?${q.toString()}`
}

export default function AcceptInvite() {
  const [params] = useSearchParams()
  const token = params.get('token') ?? ''
  const navigate = useNavigate()
  const qc = useQueryClient()
  const { isAuthenticated, isLoading: authLoading, user } = useAuth()
  const [acceptError, setAcceptError] = useState<string | null>(null)
  // Use a ref for the in-flight guard instead of state. Putting `accepting`
  // in the effect's dep array caused a race where the state flip's
  // re-render triggered the effect's cleanup, which set `cancelled=true`
  // on the in-flight promise's closure — blocking the post-success
  // navigate. A ref doesn't trigger re-renders, so the effect runs once
  // and the promise resolution path is uncontested.
  const acceptingRef = useRef(false)
  const [accepting, setAccepting] = useState(false)

  // We're committed to this token now (it came in via the URL search
  // param). Drop the localStorage bridge copy so a later auth-chain
  // redirect can't re-trigger a stale /accept-invite navigation.
  useEffect(() => {
    clearPendingInviteToken()
  }, [])

  const {
    data: invite,
    error: inspectError,
    isLoading: inspecting,
  } = useQuery({
    queryKey: ['invite-inspect', token],
    queryFn: () => api.orgs.invites.inspect(token),
    enabled: token.length > 0,
    retry: false,
  })

  const emailMismatch = useMemo(() => {
    if (!invite || !user) return false
    return invite.email.toLowerCase() !== user.email.toLowerCase()
  }, [invite, user])

  // attempt counter so retries re-fire the effect even when no other
  // deps changed (e.g. server-side state was fixed between attempts).
  const [attempt, setAttempt] = useState(0)

  useEffect(() => {
    if (!invite || !isAuthenticated || authLoading || emailMismatch) return
    if (acceptingRef.current) return
    acceptingRef.current = true
    setAccepting(true)
    setAcceptError(null)
    api.orgs.invites
      .accept(token)
      .then(async () => {
        // Drop the stale empty-orgs cache so Dashboard's /welcome
        // redirect doesn't fire on a fresh invitee whose membership
        // landed after the orgs query already resolved as [].
        await qc.invalidateQueries({ queryKey: ['orgs'] })
        navigate('/dashboard', { replace: true })
      })
      .catch((err) => {
        // A 404 here means the invite is no longer usable — it could
        // have been revoked, it expired between inspect and accept, or
        // it was already consumed by another tab/device. We can't
        // distinguish those cases from the response, so we have to
        // surface the failure rather than silently landing the user on
        // /dashboard claiming success: an "already_member" win is a
        // 200, not a 404. (StrictMode's double-effect-invoke is already
        // blocked by acceptingRef.current; we don't need the 404
        // fallback to absorb it.)
        setAcceptError(err instanceof APIError ? err.message : 'Failed to accept invite. Please try again.')
        acceptingRef.current = false
        setAccepting(false)
      })
  }, [invite, isAuthenticated, authLoading, emailMismatch, token, navigate, attempt])

  function goToAuth(path: '/register' | '/login') {
    if (!invite) return
    if (token) setPendingInviteToken(token)
    navigate(authUrlFor(path, invite), { replace: true })
  }

  if (!token) {
    return (
      <Centered>
        <Card
          title="Invalid invite link"
          body="This invite link is missing its token. Ask the person who invited you to resend it."
        />
      </Centered>
    )
  }

  if (inspecting || authLoading) {
    return (
      <Centered>
        <Card title="Loading invite…" body="One moment." spinner />
      </Centered>
    )
  }

  if (inspectError) {
    const msg =
      inspectError instanceof APIError && inspectError.status === 404
        ? 'This invite has expired or already been used. Ask an admin to send a new one.'
        : 'Couldn’t load this invite. Please try again or ask the inviter for a new link.'
    return (
      <Centered>
        <Card title="Invite unavailable" body={msg} />
      </Centered>
    )
  }

  if (!invite) {
    return (
      <Centered>
        <Card title="Invite unavailable" body="No invite was found for that link." />
      </Centered>
    )
  }

  if (isAuthenticated && emailMismatch) {
    return (
      <Centered>
        <div className="max-w-md w-full bg-surface-1 border border-border-default rounded-lg p-8 space-y-4">
          <h1 className="text-xl font-semibold text-text-primary">Wrong account</h1>
          <p className="text-sm text-text-secondary">
            This invite was sent to <span className="font-medium text-text-primary">{invite.email}</span>, but you're
            currently signed in as <span className="font-medium text-text-primary">{user?.email}</span>.
          </p>
          <p className="text-sm text-text-secondary">
            Sign out and sign back in with the invited address to accept.
          </p>
          <Link
            to="/login"
            className="block w-full text-center py-2.5 px-4 rounded-md bg-brand text-surface-0 text-sm font-medium hover:bg-brand-strong transition-colors"
          >
            Switch accounts
          </Link>
        </div>
      </Centered>
    )
  }

  if (isAuthenticated) {
    // The auto-accept effect is running.
    if (acceptError) {
      return (
        <Centered>
          <div className="max-w-md w-full bg-surface-1 border border-border-default rounded-lg p-8 space-y-4">
            <h1 className="text-xl font-semibold text-text-primary">Couldn’t join {invite.org_name}</h1>
            <p className="text-sm text-danger">{acceptError}</p>
            <button
              onClick={() => {
                acceptingRef.current = false
                setAccepting(false)
                setAcceptError(null)
                setAttempt((n) => n + 1)
              }}
              className="w-full py-2.5 px-4 rounded-md bg-brand text-surface-0 text-sm font-medium hover:bg-brand-strong transition-colors"
            >
              Try again
            </button>
          </div>
        </Centered>
      )
    }
    return (
      <Centered>
        <div className="max-w-md w-full bg-surface-1 border border-border-default rounded-lg p-8 text-center space-y-4">
          <Spinner />
          <h1 className="text-lg font-semibold text-text-primary">
            Joining {invite.org_name}…
          </h1>
          <p className="text-sm text-text-secondary">
            One moment — we’re finishing your setup.
          </p>
        </div>
      </Centered>
    )
  }

  return (
    <div className="min-h-screen bg-surface-0 flex items-center justify-center px-4">
      <div className="max-w-md w-full bg-surface-1 border border-border-default rounded-lg p-8 space-y-6">
        <div>
          <p className="text-sm font-medium text-text-tertiary uppercase tracking-wide">You're invited</p>
          <h1 className="mt-2 text-2xl font-semibold text-text-primary">Join {invite.org_name} on Clawvisor</h1>
          <p className="mt-2 text-sm text-text-secondary">
            Invitation sent to <span className="font-medium text-text-primary">{invite.email}</span>
            {invite.role && invite.role !== 'member' ? ` as ${invite.role}` : ''}.
          </p>
        </div>

        <div className="space-y-3">
          <button
            onClick={() => goToAuth('/register')}
            className="w-full py-2.5 px-4 rounded-md bg-brand text-surface-0 text-sm font-medium hover:bg-brand-strong transition-colors"
          >
            Create account and join
          </button>
          <button
            onClick={() => goToAuth('/login')}
            className="w-full py-2.5 px-4 rounded-md bg-surface-2 text-text-primary text-sm font-medium hover:bg-surface-3 transition-colors"
          >
            I already have an account
          </button>
        </div>

        <p className="text-center text-xs text-text-tertiary">
          Joining an organization is free. You won't be asked to choose a plan.
        </p>

        <p className="text-center text-xs text-text-tertiary">
          <Link to="/" className="hover:underline">
            Cancel
          </Link>
        </p>
      </div>
      {/* Suppress unused warning on the local `accepting` state — we keep it
          mirrored to the ref because future telemetry / debug overlays
          may want to read it from React DevTools. */}
      <span className="hidden" data-accepting={accepting} />
    </div>
  )
}

function Centered({ children }: { children: React.ReactNode }) {
  return <div className="min-h-screen bg-surface-0 flex items-center justify-center px-4">{children}</div>
}

function Card({ title, body, spinner }: { title: string; body: string; spinner?: boolean }) {
  return (
    <div className="max-w-md w-full bg-surface-1 border border-border-default rounded-lg p-8 space-y-3">
      {spinner && (
        <div className="flex justify-center">
          <Spinner />
        </div>
      )}
      <h1 className="text-xl font-semibold text-text-primary">{title}</h1>
      <p className="text-sm text-text-secondary">{body}</p>
    </div>
  )
}

function Spinner() {
  return (
    <svg className="w-6 h-6 animate-spin text-brand" fill="none" viewBox="0 0 24 24" aria-hidden="true">
      <circle className="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" strokeWidth="4" />
      <path
        className="opacity-75"
        fill="currentColor"
        d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z"
      />
    </svg>
  )
}
