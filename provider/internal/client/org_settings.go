package client

import "context"

// OrgSettings is the org-admin-configurable settings surface
// (GET/PUT /api/orgs/{id}/settings). Cloud/enterprise only — these routes do
// not exist on an OSS-only server, so the calls 404 there.
type OrgSettings struct {
	// MemberSelfService controls whether non-admin org members may run the
	// personal connect flows (connect agents, activate services). Default true.
	MemberSelfService bool `json:"member_self_service"`
}

// GetOrgSettings reads the org's settings. Requires an org-scoped client
// (provider org_id set).
func (c *Client) GetOrgSettings(ctx context.Context) (*OrgSettings, error) {
	var out OrgSettings
	if err := c.do(ctx, "GET", c.Scope.Org("settings"), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PutOrgSettings updates the org's settings and returns the stored value.
func (c *Client) PutOrgSettings(ctx context.Context, s OrgSettings) (*OrgSettings, error) {
	var out OrgSettings
	if err := c.do(ctx, "PUT", c.Scope.Org("settings"), s, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
