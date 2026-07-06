package client

import "context"

// Agent is the subset of the server's agent record the provider manages.
// There is no single-item GET and no update endpoint (finding S4): Read
// filters the list, and name/description changes force replacement.
type Agent struct {
	ID          string `json:"id"`
	UserID      string `json:"user_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// Token is only populated by CreateAgent / RotateAgentToken responses;
	// the list endpoint never returns it.
	Token string `json:"token"`
}

// CreateAgentRequest is the POST /api/agents body. WithCallbackSecret is
// omitted (server defaults to minting one); the provider does not manage the
// callback secret in v1.
type CreateAgentRequest struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// CreateAgent registers an agent and returns its one-time bearer token.
func (c *Client) CreateAgent(ctx context.Context, req CreateAgentRequest) (*Agent, error) {
	var a Agent
	if err := c.do(ctx, "POST", "/api/agents", req, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// GetAgent fetches the agent by id from the list endpoint. Returns a 404
// *APIError when the id is absent (the resource is gone).
func (c *Client) GetAgent(ctx context.Context, id string) (*Agent, error) {
	agents, err := c.ListAgents(ctx)
	if err != nil {
		return nil, err
	}
	for i := range agents {
		if agents[i].ID == id {
			return &agents[i], nil
		}
	}
	return nil, &APIError{StatusCode: 404, Code: "NOT_FOUND", Message: "agent not found", Method: "GET", Path: "/api/agents"}
}

// ListAgents returns all agents visible to the token's identity.
func (c *Client) ListAgents(ctx context.Context) ([]Agent, error) {
	var agents []Agent
	if err := c.do(ctx, "GET", "/api/agents", nil, &agents); err != nil {
		return nil, err
	}
	return agents, nil
}

// RotateAgentToken mints a fresh bearer token for an existing agent,
// returning the new token.
func (c *Client) RotateAgentToken(ctx context.Context, id string) (string, error) {
	var out struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := c.do(ctx, "POST", "/api/agents/"+id+"/rotate", nil, &out); err != nil {
		return "", err
	}
	return out.Token, nil
}

// DeleteAgent removes an agent (revoking its token and tasks).
func (c *Client) DeleteAgent(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", "/api/agents/"+id, nil, nil)
}
