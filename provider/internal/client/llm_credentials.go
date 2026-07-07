package client

import (
	"context"
	"net/url"
)

// LLMCredentialInput is the reference payload for an LLM provider credential in
// reference mode: a pointer to a secret in the operator's own store, never a
// value. It mirrors VaultReferenceInput but names the locator `ref_id` to match
// the clawvisor_llm_credential schema.
type LLMCredentialInput struct {
	Backend string `json:"backend"`
	RefID   string `json:"id"`
	JSONKey string `json:"json_key,omitempty"`
}

// llmCredentialEntry is one row of GET /api/runtime/llm-credentials. The server
// never returns the secret value — only whether a credential is stored.
type llmCredentialEntry struct {
	Provider    string `json:"provider"`
	Stored      bool   `json:"stored"`
	AgentStored bool   `json:"agent_stored"`
	AgentID     string `json:"agent_id"`
}

// SetLLMCredential pushes a literal provider api_key (push mode). agentID=""
// targets the instance-shared slot; a non-empty agentID targets the
// agent-scoped slot (`agent:<id>:<provider>`).
func (c *Client) SetLLMCredential(ctx context.Context, provider, agentID, apiKey string) error {
	body := map[string]string{"api_key": apiKey}
	return c.do(ctx, "PUT", llmCredentialPath(provider, agentID, false), body, nil)
}

// SetLLMCredentialReference stores an external-secret reference for the provider
// (reference mode). When verify is true the server dry-run resolves the target
// on write and fails fast on a bad reference; no value is ever returned or
// stored in state.
func (c *Client) SetLLMCredentialReference(ctx context.Context, provider, agentID string, ref LLMCredentialInput, verify bool) error {
	body := map[string]any{"reference": ref}
	return c.do(ctx, "PUT", llmCredentialPath(provider, agentID, verify), body, nil)
}

// DeleteLLMCredential removes the provider credential (push or reference).
func (c *Client) DeleteLLMCredential(ctx context.Context, provider, agentID string) error {
	return c.do(ctx, "DELETE", llmCredentialPath(provider, agentID, false), nil, nil)
}

// LLMCredentialExists reports whether a credential is stored for the
// (provider[, agentID]) pair. It never returns the secret value. A 404 (e.g. an
// agent_id whose agent was deleted out-of-band) is surfaced as an *APIError so
// the resource can decide to recreate.
func (c *Client) LLMCredentialExists(ctx context.Context, provider, agentID string) (bool, error) {
	path := "/api/runtime/llm-credentials"
	if agentID != "" {
		path += "?agent_id=" + url.QueryEscape(agentID)
	}
	var out struct {
		Credentials []llmCredentialEntry `json:"credentials"`
	}
	if err := c.do(ctx, "GET", path, nil, &out); err != nil {
		return false, err
	}
	for _, e := range out.Credentials {
		if e.Provider != provider {
			continue
		}
		if agentID != "" {
			return e.AgentStored, nil
		}
		return e.Stored, nil
	}
	return false, nil
}

// llmCredentialPath builds the PUT/DELETE path with the optional agent_id and
// verify query parameters.
func llmCredentialPath(provider, agentID string, verify bool) string {
	p := "/api/runtime/llm-credentials/" + url.PathEscape(provider)
	q := url.Values{}
	if agentID != "" {
		q.Set("agent_id", agentID)
	}
	if verify {
		q.Set("verify", "1")
	}
	if len(q) > 0 {
		p += "?" + q.Encode()
	}
	return p
}
