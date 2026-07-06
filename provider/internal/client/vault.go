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
