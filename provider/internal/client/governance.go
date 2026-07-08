package client

import "context"

// The governance client uses ONE set of structs for both OSS (instance-scoped
// `/api/governance/*`) and cloud (`/api/orgs/{id}/governance/*`) paths — the
// only difference is Scope.Governance(...). Body shapes are byte-identical to
// spec 06a's local governance handler (and cloud's), so the Terraform
// resources are schema-identical across tiers (PRD §8).
//
// NOTE: these endpoints do not exist on OSS until spec 06a lands. The provider
// gates every governance resource on the `local_governance` capability and
// fails fast before reaching them; these methods are exercised by acceptance
// tests only once 06a is merged (and the capability flips true).

// ModelPolicy is the singleton model allow/deny policy. Models are
// provider-qualified canonical ids (e.g. "anthropic/claude-3-5-sonnet").
type ModelPolicy struct {
	Mode   string   `json:"mode"` // "allow" | "deny"
	Models []string `json:"models"`
}

// GetModelPolicy fetches the active model policy. 404 → none set.
func (c *Client) GetModelPolicy(ctx context.Context) (*ModelPolicy, error) {
	var mp ModelPolicy
	if err := c.do(ctx, "GET", c.Scope.Governance("model_policy"), nil, &mp); err != nil {
		return nil, err
	}
	return &mp, nil
}

// PutModelPolicy upserts the model policy (append-only server-side).
func (c *Client) PutModelPolicy(ctx context.Context, mp ModelPolicy) (*ModelPolicy, error) {
	var out ModelPolicy
	if err := c.do(ctx, "PUT", c.Scope.Governance("model_policy"), mp, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteModelPolicy clears the model policy.
func (c *Client) DeleteModelPolicy(ctx context.Context) error {
	return c.do(ctx, "DELETE", c.Scope.Governance("model_policy"), nil, nil)
}

// SpendCap is a per-window spend cap. Window is the daily/monthly
// discriminator (also the path key). Response list items from
// GET /spend_caps MUST carry the "window" field (06a contract).
type SpendCap struct {
	Window      string `json:"window"`
	CapMicros   int64  `json:"cap_micros"`
	Enforcement string `json:"enforcement"` // "soft" | "hard"
}

// spendCapBody is the PUT body (window travels in the path, not the body).
type spendCapBody struct {
	CapMicros   int64  `json:"cap_micros"`
	Enforcement string `json:"enforcement"`
}

// GetSpendCap fetches the cap for a window by filtering GET /spend_caps.
// 404 → no cap for that window.
func (c *Client) GetSpendCap(ctx context.Context, window string) (*SpendCap, error) {
	var out struct {
		SpendCaps []SpendCap `json:"spend_caps"`
	}
	if err := c.do(ctx, "GET", c.Scope.Governance("spend_caps"), nil, &out); err != nil {
		return nil, err
	}
	for _, sc := range out.SpendCaps {
		if sc.Window == window {
			cp := sc
			return &cp, nil
		}
	}
	return nil, &APIError{StatusCode: 404, Code: "NOT_FOUND", Message: "spend cap not set for window", Method: "GET", Path: c.Scope.Governance("spend_caps")}
}

// PutSpendCap upserts the cap for a window.
func (c *Client) PutSpendCap(ctx context.Context, window string, capMicros int64, enforcement string) (*SpendCap, error) {
	var out SpendCap
	body := spendCapBody{CapMicros: capMicros, Enforcement: enforcement}
	if err := c.do(ctx, "PUT", c.Scope.Governance("spend_caps/"+window), body, &out); err != nil {
		return nil, err
	}
	out.Window = window
	return &out, nil
}

// DeleteSpendCap clears the cap for a window.
func (c *Client) DeleteSpendCap(ctx context.Context, window string) error {
	return c.do(ctx, "DELETE", c.Scope.Governance("spend_caps/"+window), nil, nil)
}

// ContentPolicy is one content-scanning rule. id is server-assigned.
type ContentPolicy struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Pattern      string `json:"pattern"`
	PatternKind  string `json:"pattern_kind"` // "regex" | "keyword"
	Action       string `json:"action"`       // "block" | "flag"
	BlockMessage string `json:"block_message"`
	Enabled      bool   `json:"enabled"`
}

// CreateContentPolicy creates a content policy (POST /content_policies).
func (c *Client) CreateContentPolicy(ctx context.Context, cp ContentPolicy) (*ContentPolicy, error) {
	var out ContentPolicy
	if err := c.do(ctx, "POST", c.Scope.Governance("content_policies"), cp, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetContentPolicy fetches one content policy by id from the list. 404 → gone.
func (c *Client) GetContentPolicy(ctx context.Context, id string) (*ContentPolicy, error) {
	var out struct {
		ContentPolicies []ContentPolicy `json:"content_policies"`
	}
	if err := c.do(ctx, "GET", c.Scope.Governance("content_policies"), nil, &out); err != nil {
		return nil, err
	}
	for _, cp := range out.ContentPolicies {
		if cp.ID == id {
			p := cp
			return &p, nil
		}
	}
	return nil, &APIError{StatusCode: 404, Code: "NOT_FOUND", Message: "content policy not found", Method: "GET", Path: c.Scope.Governance("content_policies")}
}

// UpdateContentPolicy updates a content policy by id.
func (c *Client) UpdateContentPolicy(ctx context.Context, id string, cp ContentPolicy) (*ContentPolicy, error) {
	var out ContentPolicy
	if err := c.do(ctx, "PUT", c.Scope.Governance("content_policies/"+id), cp, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteContentPolicy removes a content policy by id.
func (c *Client) DeleteContentPolicy(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", c.Scope.Governance("content_policies/"+id), nil, nil)
}

// TaskPolicy is the singleton task-guidance policy.
type TaskPolicy struct {
	Guidance string `json:"guidance"`
}

// GetTaskPolicy fetches the task policy. 404 → none set.
func (c *Client) GetTaskPolicy(ctx context.Context) (*TaskPolicy, error) {
	var tp TaskPolicy
	if err := c.do(ctx, "GET", c.Scope.Governance("task_policy"), nil, &tp); err != nil {
		return nil, err
	}
	return &tp, nil
}

// PutTaskPolicy upserts the task policy.
func (c *Client) PutTaskPolicy(ctx context.Context, tp TaskPolicy) (*TaskPolicy, error) {
	var out TaskPolicy
	if err := c.do(ctx, "PUT", c.Scope.Governance("task_policy"), tp, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteTaskPolicy clears the task policy.
func (c *Client) DeleteTaskPolicy(ctx context.Context) error {
	return c.do(ctx, "DELETE", c.Scope.Governance("task_policy"), nil, nil)
}
