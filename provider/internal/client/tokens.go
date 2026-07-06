package client

import "context"

// APIToken is the subset of a server API-token record the provider manages.
// The plaintext token is only returned at create time.
type APIToken struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	TokenPrefix string `json:"token_prefix"`
	Scope       string `json:"scope"`
	Token       string `json:"token"`
	ExpiresAt   string `json:"expires_at"`
}

// CreateTokenRequest is the POST /api/tokens body. In 05-lite the only
// accepted scope is "instance-admin"; ExpiresAt is optional RFC3339.
type CreateTokenRequest struct {
	Name      string `json:"name"`
	Scope     string `json:"scope,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

// CreateToken mints an API token, returning its one-time plaintext.
func (c *Client) CreateToken(ctx context.Context, req CreateTokenRequest) (*APIToken, error) {
	var t APIToken
	if err := c.do(ctx, "POST", "/api/tokens", req, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// GetToken fetches a token record by id from the list endpoint (there is no
// single-item GET). A revoked token is treated as gone. Returns a 404
// *APIError when the id is absent or revoked.
func (c *Client) GetToken(ctx context.Context, id string) (*APIToken, error) {
	var out struct {
		Tokens []struct {
			APIToken
			RevokedAt *string `json:"revoked_at"`
		} `json:"tokens"`
	}
	if err := c.do(ctx, "GET", "/api/tokens", nil, &out); err != nil {
		return nil, err
	}
	for _, t := range out.Tokens {
		if t.ID == id {
			if t.RevokedAt != nil {
				break
			}
			tok := t.APIToken
			return &tok, nil
		}
	}
	return nil, &APIError{StatusCode: 404, Code: "NOT_FOUND", Message: "api token not found", Method: "GET", Path: "/api/tokens"}
}

// DeleteToken revokes an API token.
func (c *Client) DeleteToken(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", "/api/tokens/"+id, nil, nil)
}
