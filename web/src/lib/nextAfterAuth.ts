// Shared post-auth routing helper.
//
// If the user arrived via an invite email, route them back to /accept-invite
// after sign-in so the invite is consumed and the plan-selection step is
// skipped. Otherwise land on /dashboard.
//
// Centralized here so every auth-completion page (Login, MFAVerify,
// TOTPVerify, SetupAuth, SecuritySetup, SSOComplete, ...) computes the same
// post-auth destination — keeps invite-redirect behavior consistent.
import { peekPendingInviteToken } from './pendingInvite'

export function nextAfterAuth(): string {
  const token = peekPendingInviteToken()
  return token ? `/accept-invite?token=${encodeURIComponent(token)}` : '/dashboard'
}
