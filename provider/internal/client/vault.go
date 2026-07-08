package client

import "context"

// VaultItem is the metadata the server returns for a vault entry. The secret
// value is never included (write-only server-side); the provider tracks drift
// via a private-state hash of the last written value, never via Read.
type VaultItem struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Status string `json:"status"`
}

// CreateVaultEntry writes a new vault entry. id is the vault item id (the
// server's storage key); value is the secret plaintext.
func (c *Client) CreateVaultEntry(ctx context.Context, id, value string) error {
	body := map[string]string{"id": id, "value": value}
	return c.do(ctx, "POST", "/api/vault/items", body, nil)
}

// GetVaultEntry fetches metadata for a vault entry. Returns a 404 *APIError
// when absent. The response never carries the secret value.
func (c *Client) GetVaultEntry(ctx context.Context, id string) (*VaultItem, error) {
	var item VaultItem
	if err := c.do(ctx, "GET", "/api/vault/items/"+id, nil, &item); err != nil {
		return nil, err
	}
	return &item, nil
}

// UpdateVaultEntry overwrites the secret value of an existing entry.
func (c *Client) UpdateVaultEntry(ctx context.Context, id, value string) error {
	body := map[string]string{"value": value}
	return c.do(ctx, "PUT", "/api/vault/items/"+id, body, nil)
}

// DeleteVaultEntry removes a vault entry.
func (c *Client) DeleteVaultEntry(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", "/api/vault/items/"+id, nil, nil)
}

// VaultReferenceInput is the reference payload: a pointer to a secret in the
// customer's own store (never a value). backend is aws-sm | gcp-sm |
// hashicorp; id is the ARN / resource name / path; json_key is optional.
type VaultReferenceInput struct {
	Backend string `json:"backend"`
	ID      string `json:"id"`
	JSONKey string `json:"json_key,omitempty"`
}

// CreateVaultReference writes a new reference entry under serviceID. When
// verify is true the server dry-run resolves the reference on create and fails
// fast on a bad target (no value is ever returned or stored in state).
func (c *Client) CreateVaultReference(ctx context.Context, serviceID string, ref VaultReferenceInput, verify bool) error {
	body := map[string]any{"id": serviceID, "reference": ref}
	return c.do(ctx, "POST", withVerify("/api/vault/items", verify), body, nil)
}

// UpdateVaultReference overwrites the reference target of an existing entry.
func (c *Client) UpdateVaultReference(ctx context.Context, serviceID string, ref VaultReferenceInput, verify bool) error {
	body := map[string]any{"reference": ref}
	return c.do(ctx, "PUT", withVerify("/api/vault/items/"+serviceID, verify), body, nil)
}

func withVerify(path string, verify bool) string {
	if verify {
		return path + "?verify=1"
	}
	return path
}
