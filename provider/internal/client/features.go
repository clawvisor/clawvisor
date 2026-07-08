package client

import "context"

// Features mirrors the server's FeatureSet (pkg/clawvisor/options.go /
// internal/api/server.go). Only the fields the provider gates on are named;
// unknown fields are ignored. A missing capability field decodes as false,
// so the provider treats "server too old to report it" as "unavailable" —
// exactly the fail-fast contract in 06b/finding M1.
type Features struct {
	MultiTenant     bool `json:"multi_tenant"`
	SSO             bool `json:"sso"`
	Teams           bool `json:"teams"`
	ProxyLite       bool `json:"proxy_lite"`
	SecretVault     bool `json:"secret_vault"`
	APITokens       bool `json:"api_tokens"`
	UserManagement  bool `json:"user_management"`  // owned by spec 04
	LocalGovernance bool `json:"local_governance"` // owned by spec 06a
}

// Has reports whether the named capability is present. Names match the
// Capability* constants below (the JSON field names on FeatureSet).
func (f Features) Has(capability string) bool {
	switch capability {
	case CapabilityAPITokens:
		return f.APITokens
	case CapabilityLocalGovernance:
		return f.LocalGovernance
	case CapabilityUserManagement:
		return f.UserManagement
	case CapabilityTeams:
		return f.Teams
	case CapabilitySSO:
		return f.SSO
	case CapabilityMultiTenant:
		return f.MultiTenant
	case CapabilitySecretVault:
		return f.SecretVault
	case CapabilityProxyLite:
		return f.ProxyLite
	default:
		return false
	}
}

// Capability names — the FeatureSet JSON field a resource depends on.
const (
	CapabilityAPITokens       = "api_tokens"
	CapabilityLocalGovernance = "local_governance"
	CapabilityUserManagement  = "user_management"
	CapabilityTeams           = "teams"
	CapabilitySSO             = "sso"
	CapabilityMultiTenant     = "multi_tenant"
	CapabilitySecretVault     = "secret_vault"
	CapabilityProxyLite       = "proxy_lite"
)

// Features fetches GET /api/features. The route always exists (OptionalUser
// middleware), so this doubles as an endpoint/auth reachability probe.
func (c *Client) Features(ctx context.Context) (*Features, error) {
	var f Features
	if err := c.do(ctx, "GET", "/api/features", nil, &f); err != nil {
		return nil, err
	}
	return &f, nil
}
