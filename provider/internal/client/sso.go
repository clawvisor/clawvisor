package client

import "context"

// SSOConnection is a per-org IdP configuration (SAML or OIDC). It mirrors the
// cloud server's org SSO config API (PUT/GET/DELETE /api/orgs/{id}/sso). The
// OIDC client secret is write-only: the server stores it encrypted and never
// returns it, so it is absent from reads.
type SSOConnection struct {
	Kind               string `json:"kind"`
	SAMLEntityID       string `json:"saml_entity_id,omitempty"`
	SAMLSSOURL         string `json:"saml_sso_url,omitempty"`
	SAMLCertificatePEM string `json:"saml_certificate_pem,omitempty"`
	OIDCIssuer         string `json:"oidc_issuer,omitempty"`
	OIDCClientID       string `json:"oidc_client_id,omitempty"`
	// OIDCClientSecret is sent on PUT only; the server encrypts it at rest and
	// never returns it, so reads leave this empty.
	OIDCClientSecret string `json:"oidc_client_secret,omitempty"`
	JITProvision     bool   `json:"jit_provision"`
	DefaultRole      string `json:"default_role,omitempty"`
	EmailDomain      string `json:"email_domain,omitempty"`
	SSOTeamAttribute string `json:"sso_team_attribute,omitempty"`
	Enabled          bool   `json:"enabled"`
}

// GetSSOConnection fetches the org's SSO config. Returns (nil, nil) when none is
// configured — the server responds 200 with a null body in that case.
func (c *Client) GetSSOConnection(ctx context.Context) (*SSOConnection, error) {
	var out *SSOConnection
	if err := c.do(ctx, "GET", c.Scope.Org("sso"), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// PutSSOConnection upserts the org's SSO config.
func (c *Client) PutSSOConnection(ctx context.Context, conn SSOConnection) error {
	return c.do(ctx, "PUT", c.Scope.Org("sso"), conn, nil)
}

// DeleteSSOConnection removes the org's SSO config.
func (c *Client) DeleteSSOConnection(ctx context.Context) error {
	return c.do(ctx, "DELETE", c.Scope.Org("sso"), nil, nil)
}
