package llmproxy

import (
	"net/http"
	"strings"
)

// CredentialClass classifies the upstream provider credential a client
// presented on a lite-proxy request. It drives the govern/contain
// (upstream_auth: vault) carve-out (spec 02 §4c): a recognized org API key is
// safe to silently strip-and-inject (§4b) because injecting the vault key
// changes nothing observable, but a subscription/OAuth seat — or any
// credential not positively recognized as an org API key — must be refused
// rather than silently rebilled from the user's subscription to org-metered
// API billing.
type CredentialClass int

const (
	// CredentialNone means no client-presented provider credential is
	// present (the agent authenticated with only its cvis_ token). The
	// vault path proceeds normally.
	CredentialNone CredentialClass = iota
	// CredentialOrgAPIKey is an x-api-key, or an Authorization bearer with a
	// recognized org API-key prefix (Anthropic sk-ant-api…, OpenAI sk-…).
	// The ONLY class eligible for silent strip-and-inject.
	CredentialOrgAPIKey
	// CredentialSubscription is an Authorization bearer with a Claude
	// subscription/OAuth prefix (sk-ant-oat01-… access, sk-ant-ort01-…
	// refresh). Refuse-or-consent.
	CredentialSubscription
	// CredentialUnrecognized is any Authorization bearer not positively
	// recognized as an org API key (opaque/unknown/JWT/future formats).
	// Fail closed — refuse, because silent inject is only safe for a
	// recognized org key.
	CredentialUnrecognized
)

// ClassifyUpstreamCredential inspects the inbound request's credential
// headers and returns the class of the client-presented provider credential.
// It classifies by the credential itself (prefix), never by token shape or a
// droppable header — the anthropic-beta: oauth-* header is at most a
// corroborating signal and is deliberately NOT consulted here, so it can
// neither be dropped to evade detection nor sent alone to force a refusal of
// a real API-key seat (spec 02 §4c, F2/F3).
//
// Agent tokens (cvis_…) are never provider credentials and are ignored.
func ClassifyUpstreamCredential(r *http.Request) CredentialClass {
	if r == nil {
		return CredentialNone
	}
	// An Authorization bearer is where a subscription/OAuth token would
	// live, so classify it first and most strictly.
	if bearer := providerBearerToken(r); bearer != "" {
		switch {
		case isSubscriptionToken(bearer):
			return CredentialSubscription
		case isOrgAPIKeyToken(bearer):
			return CredentialOrgAPIKey
		default:
			// Unknown Authorization bearer: fail closed.
			return CredentialUnrecognized
		}
	}
	// x-api-key (Anthropic SDK convention) carries an API key by definition;
	// exclude the cvis_ agent token which also rides here.
	if x := strings.TrimSpace(r.Header.Get("x-api-key")); x != "" && !strings.HasPrefix(x, "cvis_") {
		return CredentialOrgAPIKey
	}
	return CredentialNone
}

// providerBearerToken returns the bare Authorization bearer token when it is a
// non-cvis provider credential, else "".
func providerBearerToken(r *http.Request) string {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	// Parse the scheme case-insensitively: a lowercase `authorization: bearer
	// sk-ant-oat01-…` is still a bearer credential and must not slip past the
	// §4c subscription refusal into a silent vault strip-and-inject.
	scheme, rest, found := strings.Cut(auth, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	token := strings.TrimSpace(rest)
	if token == "" || strings.HasPrefix(token, "cvis_") {
		return ""
	}
	return token
}

// isSubscriptionToken reports whether a bearer is a Claude subscription /
// OAuth token. Access tokens are sk-ant-oat01-…, refresh tokens
// sk-ant-ort01-… — sk-ant--prefixed and opaque, NOT JWT-shaped (spec 02 §4c).
func isSubscriptionToken(token string) bool {
	return strings.HasPrefix(token, "sk-ant-oat01-") ||
		strings.HasPrefix(token, "sk-ant-ort01-")
}

// isOrgAPIKeyToken reports whether a bearer is a recognized org provider API
// key: Anthropic sk-ant-api… or OpenAI sk-… (any sk- that is not an
// sk-ant- subscription/other variant). Unknown sk-ant- variants fall through
// to unrecognized (fail closed).
func isOrgAPIKeyToken(token string) bool {
	if strings.HasPrefix(token, "sk-ant-api") {
		return true
	}
	if strings.HasPrefix(token, "sk-ant-") {
		// sk-ant-oat01/ort01 handled by isSubscriptionToken; any other
		// sk-ant- shape is not a recognized API key — fail closed.
		return false
	}
	// OpenAI keys: sk-…, sk-proj-…
	return strings.HasPrefix(token, "sk-")
}

// HasClientProviderCredential reports whether the request carries any usable
// client-presented provider credential (an Authorization bearer that isn't a
// cvis_ agent token, or a non-cvis x-api-key). Used by passthrough posture to
// distinguish "passthrough requested but nothing to forward" from a request
// that legitimately carries the user's own key (spec 02 §4
// PASSTHROUGH_NO_CREDENTIAL).
func HasClientProviderCredential(r *http.Request) bool {
	if r == nil {
		return false
	}
	if providerBearerToken(r) != "" {
		return true
	}
	if x := strings.TrimSpace(r.Header.Get("x-api-key")); x != "" && !strings.HasPrefix(x, "cvis_") {
		return true
	}
	return false
}
