package client

import (
	"context"
	"fmt"
)

// Org is the subset of an org record the provider manages via the instance-admin
// bootstrap surface (POST/GET/DELETE /api/admin/orgs). These routes are
// instance-scoped (a `cvat_` instance-admin token, no org_id) and only exist on
// a self-hosted deployment (they 404 on the hosted SaaS and on OSS).
type Org struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
	Tier string `json:"tier"`
}

// CreateOrgRequest is the POST /api/admin/orgs body.
type CreateOrgRequest struct {
	Name string `json:"name"`
	Slug string `json:"slug"`
}

// CreateOrg provisions an unowned enterprise-tier org and returns it.
func (c *Client) CreateOrg(ctx context.Context, req CreateOrgRequest) (*Org, error) {
	var o Org
	if err := c.do(ctx, "POST", "/api/admin/orgs", req, &o); err != nil {
		return nil, err
	}
	return &o, nil
}

// GetOrg reads an org by id. Returns a 404 *APIError when absent or deleted.
func (c *Client) GetOrg(ctx context.Context, id string) (*Org, error) {
	var o Org
	if err := c.do(ctx, "GET", "/api/admin/orgs/"+id, nil, &o); err != nil {
		return nil, err
	}
	return &o, nil
}

// DeleteOrg soft-deletes an org.
func (c *Client) DeleteOrg(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", "/api/admin/orgs/"+id, nil, nil)
}

// OrgToken is a cvot_ org-scoped token minted for an org via the instance-admin
// surface. The plaintext Token is only returned at create time.
type OrgToken struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Role        string `json:"role"`
	TokenPrefix string `json:"token_prefix"`
	ExpiresAt   string `json:"expires_at"`
	Token       string `json:"token"`
}

// CreateOrgTokenRequest is the POST /api/admin/orgs/{id}/tokens body.
type CreateOrgTokenRequest struct {
	Name          string `json:"name"`
	ExpiresInDays *int   `json:"expires_in_days,omitempty"`
}

// CreateOrgTokenAdmin mints a cvot_ org-admin token for orgID via the
// instance-admin surface, returning its one-time plaintext.
func (c *Client) CreateOrgTokenAdmin(ctx context.Context, orgID string, req CreateOrgTokenRequest) (*OrgToken, error) {
	var t OrgToken
	if err := c.do(ctx, "POST", fmt.Sprintf("/api/admin/orgs/%s/tokens", orgID), req, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// ListOrgTokens lists an org's active tokens (metadata only — no plaintext).
func (c *Client) ListOrgTokens(ctx context.Context, orgID string) ([]OrgToken, error) {
	var out struct {
		Tokens []OrgToken `json:"tokens"`
	}
	if err := c.do(ctx, "GET", fmt.Sprintf("/api/admin/orgs/%s/tokens", orgID), nil, &out); err != nil {
		return nil, err
	}
	return out.Tokens, nil
}

// RevokeOrgToken revokes a cvot_ token by id under its org.
func (c *Client) RevokeOrgToken(ctx context.Context, orgID, tokenID string) error {
	return c.do(ctx, "DELETE", fmt.Sprintf("/api/admin/orgs/%s/tokens/%s", orgID, tokenID), nil, nil)
}
