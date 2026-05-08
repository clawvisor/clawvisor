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
	switch strings.ToLower(strings.TrimSpace(serviceID)) {
	case "github":
		return []string{
			"api.github.com",
			"uploads.github.com",
		}
	case "gitlab":
		return []string{"gitlab.com", "*.gitlab.com"}
	case "slack":
		return []string{"slack.com", "*.slack.com"}
	case "gmail", "google", "gdrive", "gcalendar":
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
	case "openai":
		return []string{"api.openai.com"}
	case "anthropic":
		return []string{"api.anthropic.com"}
	}
	return nil
}
