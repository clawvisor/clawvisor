package inspector

import "strings"

// BoundServiceHosts returns the canonical host allowlist for a runtime
// placeholder's bound service. The placeholder's `ServiceID` is the
// authoritative source of truth for "what hosts is this credential
// authorized to forward to" — NOT the validator's claimed host (which
// may be hallucinated or attacker-influenced) and NOT the harness-
// supplied `X-Clawvisor-Target-Host` (which the model can pick freely).
//
// v0 is a hardcoded map for the most common services. Extensible later
// either by reading the existing service catalog (preferred) or by
// allowing per-deployment config overrides.
//
// An unknown service returns an empty slice; callers must fail-closed.
func BoundServiceHosts(serviceID string) []string {
	switch strings.ToLower(strings.TrimSpace(normalizeBoundServiceID(serviceID))) {
	case "github":
		return []string{
			"api.github.com",
			"uploads.github.com",
		}
	case "gitlab":
		return []string{"gitlab.com", "*.gitlab.com"}
	case "slack":
		return []string{"slack.com", "*.slack.com"}
	case "gmail", "google.gmail":
		return []string{
			"gmail.googleapis.com",
			"www.googleapis.com",
			"oauth2.googleapis.com",
		}
	case "gcalendar", "google.calendar":
		return []string{
			"www.googleapis.com",
			"calendar.googleapis.com",
			"oauth2.googleapis.com",
		}
	case "gdrive", "google.drive":
		return []string{
			"www.googleapis.com",
			"oauth2.googleapis.com",
		}
	case "google.contacts":
		return []string{
			"people.googleapis.com",
			"www.googleapis.com",
			"oauth2.googleapis.com",
		}
	case "google":
		return []string{
			"www.googleapis.com",
			"gmail.googleapis.com",
			"calendar.googleapis.com",
			"drive.googleapis.com",
			"oauth2.googleapis.com",
		}
	case "stripe":
		return []string{"api.stripe.com"}
	case "twilio":
		return []string{"api.twilio.com"}
	case "notion":
		return []string{"api.notion.com"}
	case "linear":
		return []string{"api.linear.app"}
	case "perplexity":
		return []string{"api.perplexity.ai"}
	case "resend":
		return []string{"api.resend.com"}
	case "openai":
		return []string{"api.openai.com"}
	case "anthropic":
		return []string{"api.anthropic.com"}
	}
	return nil
}

// normalizeBoundServiceID strips synthetic prefixes and account-scoped
// suffixes that wrap a real service token. Without normalization the
// boundary check returns an empty allowlist and every credentialed
// call fails closed even when the underlying service is well-known.
//
// Wrappers handled:
//   - `runtime.captured.<service>.<placeholder>` — produced by the
//     inbound-secret capture path when the proxy auto-vaults a secret
//     it observed in an outbound request.
//   - `<service>:<account>` — produced by the Shadow Tokens UI when
//     the user has multiple accounts for the same service (e.g.
//     `github:ericlevine`, `github:work`). The account suffix scopes
//     ownership; the bound-service host allowlist is shared across
//     accounts.
func normalizeBoundServiceID(serviceID string) string {
	id := strings.TrimSpace(serviceID)
	const capturedPrefix = "runtime.captured."
	if strings.HasPrefix(id, capturedPrefix) {
		remainder := id[len(capturedPrefix):]
		// Shape is `<service>.<placeholder>`; the service token is up
		// to the first '.'. Placeholder tokens may themselves contain
		// dots, so we only split on the first separator.
		if i := strings.IndexByte(remainder, '.'); i > 0 {
			id = remainder[:i]
		} else {
			id = remainder
		}
	}
	if strings.HasPrefix(id, "agent:") {
		parts := strings.Split(id, ":")
		if len(parts) == 3 && parts[2] != "" {
			id = parts[2]
		}
	}
	if strings.HasPrefix(id, "llm:") {
		parts := strings.Split(id, ":")
		if len(parts) >= 3 && parts[1] != "" {
			id = parts[1]
		}
	}
	// Account-scoped: `<service>:<account>`. The account portion
	// scopes credential ownership in the UI; the bound-service host
	// allowlist applies per-service, not per-account.
	if i := strings.IndexByte(id, ':'); i > 0 {
		id = id[:i]
	}
	return id
}
